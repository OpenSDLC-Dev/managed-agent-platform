package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// The wire work API's state-machine outcomes, mapped by the API layer onto HTTP
// statuses: not-found → 404, conflict → 409, heartbeat mismatch → 412.
var (
	ErrWorkNotFound      = errors.New("queue: work item not found")
	ErrWorkConflict      = errors.New("queue: work item is in a conflicting state")
	ErrHeartbeatMismatch = errors.New("queue: heartbeat precondition failed")
)

// NoHeartbeat is the sentinel a worker's first heartbeat sends as
// expected_last_heartbeat to claim an unclaimed lease (the wire's optimistic
// concurrency: subsequent heartbeats echo the server's prior value).
const NoHeartbeat = "NO_HEARTBEAT"

// ackStartupLeaseSeconds is the grace a just-acked (starting) item gets to send
// its first heartbeat before Poll may reclaim it as a dead worker. Ack sets it
// as the item's lease so a starting item is governed by a real lease, not the
// short un-acked poll reservation (reclaim_older_than_ms) it was polled with —
// otherwise a slow-but-live worker's item could be reclaimed in the ack →
// first-heartbeat gap. It matches the default heartbeat TTL (api's
// defaultHeartbeatTTLSeconds), so a starting item's window equals an active
// one's; the queue cannot import api, so the value is mirrored here.
const ackStartupLeaseSeconds = 30

// workAPIScope restricts a work-API query to the wire's notion of a work item —
// a tool_exec item in a self_hosted environment. Two other row kinds share the
// work_items table and must never be reachable through a worker's
// environment-key endpoints: model_turn is the brain's own queue (acking one
// would wedge the brain's turn), and a cloud environment's tool_exec is the
// platform executor's (force-stopping one would yank it from the executor
// mid-run). Poll/Claim already scope this way; the lifecycle mutators must
// match. Append to a `WHERE id = $1 AND environment_id = $2` prefix.
const workAPIScope = ` AND kind = 'tool_exec'
	AND EXISTS (SELECT 1 FROM environments e WHERE e.id = environment_id AND e.kind = 'self_hosted')`

// HeartbeatResult is the wire heartbeat response projection.
type HeartbeatResult struct {
	LastHeartbeat time.Time
	State         string
	LeaseExtended bool
	TTLSeconds    int64
}

// GetWork returns one work item visible to the work API (see workAPIScope), or
// ErrWorkNotFound.
func (q *Queue) GetWork(ctx context.Context, envID, workID domain.ID) (*Work, error) {
	w, err := scanWork(q.pool.QueryRow(ctx,
		`SELECT `+workColumns+` FROM work_items
		 WHERE id = $1 AND environment_id = $2`+workAPIScope,
		workID, envID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWorkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("queue: get work %s: %w", workID, err)
	}
	return w, nil
}

// ListWork returns a page of work items visible to the work API (see
// workAPIScope) for the environment, newest first by (created_at, id). It fetches
// up to `fetch` rows so the caller can pass limit+1 and detect a further page.
// When after is true, the (afterT, afterID) keyset position excludes rows at or
// newer than it, continuing a previous page.
func (q *Queue) ListWork(ctx context.Context, envID domain.ID, after bool, afterT time.Time, afterID string, fetch int) ([]*Work, error) {
	query := `SELECT ` + workColumns + ` FROM work_items
	          WHERE environment_id = $1` + workAPIScope
	args := []any{envID}
	if after {
		args = append(args, afterT, afterID)
		query += fmt.Sprintf(` AND (created_at, id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	args = append(args, fetch)
	query += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args))

	rows, err := q.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("queue: list work: %w", err)
	}
	defer rows.Close()
	var out []*Work
	for rows.Next() {
		w, err := scanWork(rows)
		if err != nil {
			return nil, fmt.Errorf("queue: list work: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: list work: %w", err)
	}
	return out, nil
}

// Ack acknowledges a polled work item, transitioning queued → starting. It is
// idempotent: only the queued→starting edge stamps acknowledged_at and installs
// the startup lease, so a re-ack of an already-advanced item returns it
// unchanged. The startup lease (ackStartupLeaseSeconds) governs a starting item
// until its first heartbeat replaces it, so Poll reclaims a dead worker's
// starting item on a real lease, not the short un-acked poll reservation. An
// item not visible to the work API (missing, wrong environment, or not a
// self_hosted tool_exec item) is ErrWorkNotFound.
func (q *Queue) Ack(ctx context.Context, envID, workID domain.ID) (*Work, error) {
	w, err := scanWork(q.pool.QueryRow(ctx,
		`UPDATE work_items
		 SET state            = CASE WHEN state = 'queued' THEN 'starting' ELSE state END,
		     acknowledged_at  = CASE WHEN state = 'queued' THEN now() ELSE acknowledged_at END,
		     lease_expires_at = CASE WHEN state = 'queued'
		                             THEN now() + make_interval(secs => ($3)::double precision)
		                             ELSE lease_expires_at END,
		     updated_at       = now()
		 WHERE id = $1 AND environment_id = $2`+workAPIScope+`
		 RETURNING `+workColumns,
		workID, envID, ackStartupLeaseSeconds))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWorkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("queue: ack %s: %w", workID, err)
	}
	return w, nil
}

// Heartbeat applies the wire's optimistic-concurrency heartbeat. The first
// heartbeat (expected == NoHeartbeat) claims the lease of a just-acked
// (starting) item and moves it to active; subsequent heartbeats echo the
// server's prior last_heartbeat and extend the lease while the item is active.
// A heartbeat on an active item the control plane has since moved to
// stopping/stopped succeeds without extending the lease, so the worker learns
// to wind down. An item not visible to the work API is ErrWorkNotFound; a
// visible item whose precondition does not hold (the expected value is not the
// row's current last_heartbeat, or the first-heartbeat preconditions fail) is
// ErrHeartbeatMismatch (412).
func (q *Queue) Heartbeat(ctx context.Context, envID, workID domain.ID, expected string, ttlSeconds int64) (*HeartbeatResult, error) {
	var row pgx.Row
	if expected == NoHeartbeat {
		row = q.pool.QueryRow(ctx,
			`UPDATE work_items
			 SET last_heartbeat   = now(),
			     started_at       = now(),
			     state            = 'active',
			     lease_expires_at = now() + make_interval(secs => ($3)::double precision),
			     updated_at       = now()
			 WHERE id = $1 AND environment_id = $2`+workAPIScope+`
			   AND last_heartbeat IS NULL AND state = 'starting'
			 RETURNING last_heartbeat, state, true`,
			workID, envID, ttlSeconds)
	} else {
		// expected must be a timestamp the server itself emitted (RFC3339Nano,
		// the JSON encoding of the returned last_heartbeat). Parse it here rather
		// than casting the raw string in SQL: a non-timestamp precondition would
		// make ($n)::timestamptz raise a DB error that surfaces as a 500, when it
		// is simply a value that cannot be the current last_heartbeat — a 412.
		ts, perr := time.Parse(time.RFC3339Nano, expected)
		if perr != nil {
			return nil, ErrHeartbeatMismatch
		}
		row = q.pool.QueryRow(ctx,
			`UPDATE work_items
			 SET last_heartbeat   = CASE WHEN state = 'active' THEN now() ELSE last_heartbeat END,
			     lease_expires_at = CASE WHEN state = 'active'
			                             THEN now() + make_interval(secs => ($3)::double precision)
			                             ELSE lease_expires_at END,
			     updated_at       = now()
			 WHERE id = $1 AND environment_id = $2`+workAPIScope+`
			   AND last_heartbeat = $4
			 RETURNING last_heartbeat, state, (state = 'active')`,
			workID, envID, ttlSeconds, ts)
	}
	res := HeartbeatResult{TTLSeconds: ttlSeconds}
	err := row.Scan(&res.LastHeartbeat, &res.State, &res.LeaseExtended)
	if err == nil {
		return &res, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("queue: heartbeat %s: %w", workID, err)
	}
	// No row updated: a genuinely absent item is not-found; a present one whose
	// claim/extend precondition did not hold is a mismatch.
	present, verr := q.visible(ctx, envID, workID)
	if verr != nil {
		return nil, fmt.Errorf("queue: heartbeat %s: %w", workID, verr)
	}
	if !present {
		return nil, ErrWorkNotFound
	}
	return nil, ErrHeartbeatMismatch
}

// Stop stops a work item and returns the updated item (the wire Stop responds
// with the BetaSelfHostedWork, like ack/heartbeat — not an empty 204). force
// stops any not-yet-stopped item immediately (→ stopped); a graceful stop moves
// a live (queued/starting/active) item to stopping so the worker can wind down.
// Stopping an item that is already past the requested transition (e.g.
// graceful-stopping a stopping item, or stopping a stopped one) is
// ErrWorkConflict; an item not visible to the work API is ErrWorkNotFound.
func (q *Queue) Stop(ctx context.Context, envID, workID domain.ID, force bool) (*Work, error) {
	var sql string
	if force {
		sql = `UPDATE work_items
		       SET state             = 'stopped',
		           stop_requested_at = COALESCE(stop_requested_at, now()),
		           stopped_at        = now(),
		           lease_expires_at  = NULL,
		           updated_at        = now()
		       WHERE id = $1 AND environment_id = $2` + workAPIScope + ` AND state <> 'stopped'
		       RETURNING ` + workColumns
	} else {
		sql = `UPDATE work_items
		       SET state             = 'stopping',
		           stop_requested_at = COALESCE(stop_requested_at, now()),
		           updated_at        = now()
		       WHERE id = $1 AND environment_id = $2` + workAPIScope + `
		         AND state IN ('queued', 'starting', 'active')
		       RETURNING ` + workColumns
	}
	w, err := scanWork(q.pool.QueryRow(ctx, sql, workID, envID))
	if err == nil {
		return w, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("queue: stop %s: %w", workID, err)
	}
	// No row updated: distinguish a missing item from a conflicting state.
	present, verr := q.visible(ctx, envID, workID)
	if verr != nil {
		return nil, fmt.Errorf("queue: stop %s: %w", workID, verr)
	}
	if !present {
		return nil, ErrWorkNotFound
	}
	return nil, ErrWorkConflict
}

// visible reports whether a work item is visible to the wire work API (see
// workAPIScope), scoped to envID. It lets Heartbeat/Stop tell a genuinely absent
// item (→ ErrWorkNotFound) apart from one whose state-machine precondition
// simply did not hold (→ mismatch/conflict).
func (q *Queue) visible(ctx context.Context, envID, workID domain.ID) (bool, error) {
	var one int
	err := q.pool.QueryRow(ctx,
		`SELECT 1 FROM work_items WHERE id = $1 AND environment_id = $2`+workAPIScope,
		workID, envID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
