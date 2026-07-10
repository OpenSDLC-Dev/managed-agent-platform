// Package queue is the internal work queue over Postgres (FOR UPDATE SKIP
// LOCKED, per the plan's component 4). Two kinds share the work_items table:
// model_turn drives the brain, tool_exec drives executors (consumed from
// slice 6). Enqueue is idempotent per (session, kind) while a live item
// exists, so event-append triggers can fire without double-scheduling; a
// claim leases the item and an expired lease makes it claimable again.
package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Kind discriminates the two work streams.
type Kind string

const (
	ModelTurn Kind = "model_turn"
	ToolExec  Kind = "tool_exec"
)

// ErrLeaseLost reports that the item is no longer this claimant's: its lease
// expired and another claim took over, or it already finished.
var ErrLeaseLost = errors.New("queue: work item lease lost")

// Item is one claimed unit of work.
type Item struct {
	ID            domain.ID
	EnvironmentID domain.ID
	SessionID     domain.ID
	Kind          Kind
	// Lease is the claim's expiry as recorded by the database. It is the
	// claimant's proof of ownership: Extend and Complete match it against
	// the row (the same optimistic-concurrency shape as the reference work
	// API's expected_last_heartbeat), so a claimant that lost its lease to
	// a reclaim gets ErrLeaseLost instead of silently finishing someone
	// else's item.
	Lease time.Time
	// Reclaimed marks an item whose previous claimant let the lease expire —
	// the session was mid-turn when its brain died, so the new claimant
	// should surface recovery (session.status_rescheduled) before replaying.
	Reclaimed bool
}

// DB is the slice of pgx shared by pools and transactions, so Enqueue can
// join the caller's transaction (event append + status flip + enqueue must
// commit atomically).
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Queue hands out work over one Postgres pool.
type Queue struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Queue { return &Queue{pool: pool} }

// Enqueue inserts a queued item unless a live (queued/starting/active) item
// for the same session and kind exists. It reports whether a new item was
// created; false means an existing live item already covers the work.
func (q *Queue) Enqueue(ctx context.Context, db DB, envID, sessionID domain.ID, kind Kind) (bool, error) {
	tag, err := db.Exec(ctx,
		`INSERT INTO work_items (id, environment_id, session_id, kind)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (session_id, kind) WHERE state IN ('queued', 'starting', 'active')
		 DO NOTHING`,
		domain.NewID("work"), envID, sessionID, kind)
	if err != nil {
		return false, fmt.Errorf("queue: enqueue %s for %s: %w", kind, sessionID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Claim leases the oldest available item of the kind: queued items first-come
// first-served, plus active items whose lease expired (their claimant died).
// It returns nil with no error when there is nothing to do.
func (q *Queue) Claim(ctx context.Context, kind Kind, ttl time.Duration) (*Item, error) {
	var it Item
	var prevState string
	err := q.pool.QueryRow(ctx,
		`WITH picked AS (
		    SELECT id, state FROM work_items
		    WHERE kind = $1
		      AND (state = 'queued' OR (state = 'active' AND lease_expires_at < now()))
		    ORDER BY created_at
		    FOR UPDATE SKIP LOCKED
		    LIMIT 1
		 )
		 UPDATE work_items w
		 SET state = 'active',
		     lease_expires_at = now() + make_interval(secs => $2),
		     updated_at = now()
		 FROM picked p
		 WHERE w.id = p.id
		 RETURNING w.id, w.environment_id, w.session_id, w.kind, w.lease_expires_at, p.state`,
		kind, ttl.Seconds()).Scan(&it.ID, &it.EnvironmentID, &it.SessionID, &it.Kind, &it.Lease, &prevState)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("queue: claim %s: %w", kind, err)
	}
	it.Reclaimed = prevState == "active"
	return &it, nil
}

// Extend renews the claimant's lease mid-work (long provider streams) and
// returns the new lease proof.
func (q *Queue) Extend(ctx context.Context, item *Item, ttl time.Duration) error {
	err := q.pool.QueryRow(ctx,
		`UPDATE work_items
		 SET lease_expires_at = now() + make_interval(secs => $3), updated_at = now()
		 WHERE id = $1 AND state = 'active' AND lease_expires_at = $2
		 RETURNING lease_expires_at`,
		item.ID, item.Lease, ttl.Seconds()).Scan(&item.Lease)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("queue: extend %s: %w", item.ID, ErrLeaseLost)
	}
	if err != nil {
		return fmt.Errorf("queue: extend %s: %w", item.ID, err)
	}
	return nil
}

// Requeue hands a claimed item back to the queue inside the caller's
// transaction: the claimant discovered follow-on work for the same session
// (input that arrived mid-turn) and chains it under the item's existing
// live slot — an Enqueue would be suppressed by it. Requires the lease.
func (q *Queue) Requeue(ctx context.Context, db DB, item *Item) error {
	tag, err := db.Exec(ctx,
		`UPDATE work_items
		 SET state = 'queued', lease_expires_at = NULL, updated_at = now()
		 WHERE id = $1 AND state = 'active' AND lease_expires_at = $2`,
		item.ID, item.Lease)
	if err != nil {
		return fmt.Errorf("queue: requeue %s: %w", item.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("queue: requeue %s: %w", item.ID, ErrLeaseLost)
	}
	return nil
}

// Complete marks the item finished. Losing the lease first (another claimant
// took over after expiry) is an error: the caller's work may have raced the
// replacement's and must not be treated as cleanly finished.
func (q *Queue) Complete(ctx context.Context, item *Item) error {
	tag, err := q.pool.Exec(ctx,
		`UPDATE work_items
		 SET state = 'stopped', lease_expires_at = NULL, updated_at = now()
		 WHERE id = $1 AND state = 'active' AND lease_expires_at = $2`,
		item.ID, item.Lease)
	if err != nil {
		return fmt.Errorf("queue: complete %s: %w", item.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("queue: complete %s: %w", item.ID, ErrLeaseLost)
	}
	return nil
}
