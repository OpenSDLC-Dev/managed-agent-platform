package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// The wire work API's state-machine outcomes, mapped by the API layer onto HTTP
// statuses: not-found → 404, heartbeat mismatch → 412. A stop that moves
// nothing is no error (see Stop).
var (
	ErrWorkNotFound      = errors.New("queue: work item not found")
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
// a tool_exec item in a self_hosted environment. Every other row in the
// work_items table must be unreachable through a worker's environment-key
// endpoints, and two of them are hazards rather than merely out of scope:
// model_turn is the brain's own queue (acking one would wedge the brain's
// turn), and a cloud environment's tool_exec is the platform executor's
// (force-stopping one would yank it from the executor mid-run). It names the
// hazards rather than counting the kinds, so that adding a kind cannot falsify
// it. Poll/Claim already scope this way; the lifecycle mutators must match.
// Append to a `WHERE id = $1 AND environment_id = $2` prefix.
const workAPIScope = ` AND kind = 'tool_exec'
	AND EXISTS (SELECT 1 FROM environments e WHERE e.id = environment_id AND e.kind = 'self_hosted')`

// HeartbeatResult is the wire heartbeat response projection. LastHeartbeat is
// nil when no beat has ever reached the item, which only the claim on a
// never-claimed stopping item answers (see Heartbeat).
type HeartbeatResult struct {
	LastHeartbeat *time.Time
	State         string
	LeaseExtended bool
	TTLSeconds    int64
}

// HasUndrainedWork reports whether the environment's work queue still holds an
// item a worker could be handed or is already holding — work-API scope (see
// workAPIScope), minus the items that have reached 'stopped'. It answers false
// for an environment that does not exist, and for every cloud environment,
// because a cloud tool_exec is the platform executor's queue rather than a
// worker's and the scope excludes it.
//
// It lives here rather than in the one handler that asks (the environment
// delete's refusal, #546) so that the wire's notion of the queue is defined
// once. A hand-written copy in internal/api would drift the day this scope
// changes, and would drift silently: the delete would start refusing on rows
// no worker can drain, or stop refusing on rows one is running.
//
// "Undrained" is the boundary Stop's own force arm draws, and it is wider than
// Stats' depth on purpose — depth counts the unreserved backlog a worker could
// poll next, while a delete has to account for the leased item a worker is
// running right now, which is the one whose loss costs most.
func (q *Queue) HasUndrainedWork(ctx context.Context, envID domain.ID) (bool, error) {
	var undrained bool
	if err := q.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM work_items
		                 WHERE environment_id = $1`+workAPIScope+`
		                   AND state <> 'stopped')`, envID).Scan(&undrained); err != nil {
		return false, fmt.Errorf("queue: undrained work %s: %w", envID, err)
	}
	return undrained, nil
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
// unchanged — save a missing started_at, which any ack heals to created_at
// (see Poll). The startup lease (ackStartupLeaseSeconds) governs a starting item
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
		     started_at       = COALESCE(started_at, created_at),
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
// A heartbeat on an item the control plane has since moved to stopping or
// stopped succeeds without extending the lease, so the worker learns to wind
// down: the echo of a worker that claimed the item, and the claim of one that
// had not yet when a graceful stop parked it in stopping. An item not visible
// to the work API is ErrWorkNotFound; a visible item whose precondition does
// not hold (the expected value is not the row's current last_heartbeat, or the
// first-heartbeat preconditions fail) is ErrHeartbeatMismatch (412).
func (q *Queue) Heartbeat(ctx context.Context, envID, workID domain.ID, expected string, ttlSeconds int64) (*HeartbeatResult, error) {
	var row pgx.Row
	if expected == NoHeartbeat {
		row = q.pool.QueryRow(ctx,
			`UPDATE work_items
			 SET last_heartbeat   = now(),
			     state            = 'active',
			     started_at       = COALESCE(started_at, created_at),
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
	// No row updated. A genuinely absent item is not-found, and a present one
	// whose claim or extend precondition did not hold is a mismatch — save one
	// claim. On stopping work no beat has reached, a graceful stop landed
	// between the ack and the claim, and the claim is the one beat that item's
	// worker still sends, so it is answered with the stop, as the reference
	// answers it (2026-09-19 custom-mixed-tools #44): not extended, stopping,
	// no last heartbeat, and nothing written. Every other failed claim keeps
	// its 412 — before the ack, on active work, on once-claimed stopping work
	// (whose worker learns from its echo), on stopped work.
	var state string
	var last *time.Time
	err = q.pool.QueryRow(ctx,
		`SELECT state, last_heartbeat FROM work_items WHERE id = $1 AND environment_id = $2`+workAPIScope,
		workID, envID).Scan(&state, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWorkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("queue: heartbeat %s: %w", workID, err)
	}
	if expected == NoHeartbeat && state == "stopping" && last == nil {
		return &HeartbeatResult{State: state, TTLSeconds: ttlSeconds}, nil
	}
	return nil, ErrHeartbeatMismatch
}

// Stop stops a work item and returns the item after the stop, which the wire
// answers with (200 and the BetaSelfHostedWork, as the recorded service does;
// #804), and whether the stop moved it. force stops any not-yet-stopped item
// immediately (→ stopped); a graceful stop asks the item's worker to wind down,
// so it moves the item to stopping whenever a worker has acked it — starting or
// active — as the reference does (2026-09-19 recordings; #810). Stopping is
// left by that worker: it learns of the stop from its next heartbeat, winds its
// tools down and stops the item (internal/worker/lease.go). An active item's
// next beat is an echo, which reports the state without extending the lease; a
// starting item's is its claim, which on stopping work no beat has reached
// answers the stop too (see Heartbeat). Whether that worker is still alive is
// not knowable here, and does not need to be: a wind-down nobody finishes is
// finalized by a Poll of the environment once its lease — for a starting item,
// the startup lease its ack installed — has lapsed and WindDown has passed
// since the request (see Poll). moved is true for the move to stopping, though
// the item is not stopped yet: the re-arm a finished stop owes the session
// (plan 35 decision 13 iii) is owed by whichever path later finishes it — the
// worker's own force stop, or the finalizing poll, in its own transaction.
//
// A queued item, polled or not, has no worker to ask — nobody acked it — and
// nothing in flight to wind down, so a graceful stop of one completes outright
// (→ stopped) rather than wait non-terminal for a poll to finalize it, one an
// emptied environment may never see (#25). What the reference does with one
// is unrecorded (docs/DIVERGENCES.md).
//
// Stopping an item that is already at or past the requested transition
// (graceful-stopping a stopping item, or stopping a stopped one) changes nothing:
// it returns the item as it stands with moved false, so a caller can tell it
// from a transition it owes follow-up work for. The wire answers it 200 with the
// item unchanged, the recorded service's answer to every repeat stop (#804). An
// item not visible to the work API is ErrWorkNotFound.
func (q *Queue) Stop(ctx context.Context, envID, workID domain.ID, force bool) (w *Work, moved bool, err error) {
	return q.StopWith(ctx, q.pool, envID, workID, force)
}

// StopWith is Stop on the caller's db handle, so the control plane can stop
// an item inside a transaction that holds the session row lock and re-arms
// the session's remaining runnable calls in the same commit (plan 35
// decision 13 iii).
func (q *Queue) StopWith(ctx context.Context, db DB, envID, workID domain.ID, force bool) (w *Work, moved bool, err error) {
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
		// Every CASE reads the row's pre-update state, so all four agree on which
		// branch they are in. An item a worker has acked keeps its lease — an
		// active item's claimed one, a starting item's startup lease: its
		// lapsing past WindDown is what tells the control plane the wind-down was
		// abandoned (see Poll). A starting item's would not survive clearing: a
		// null lease is the finalizer's legacy arm, which would settle the item
		// at the next poll, before a claim as late as the recorded one.
		sql = `UPDATE work_items
		       SET state             = CASE WHEN state IN ('starting', 'active') THEN 'stopping' ELSE 'stopped' END,
		           stop_requested_at = COALESCE(stop_requested_at, now()),
		           stopped_at        = CASE WHEN state IN ('starting', 'active') THEN stopped_at ELSE now() END,
		           lease_expires_at  = CASE WHEN state IN ('starting', 'active') THEN lease_expires_at ELSE NULL END,
		           updated_at        = now()
		       WHERE id = $1 AND environment_id = $2` + workAPIScope + `
		         AND state IN ('queued', 'starting', 'active')
		       RETURNING ` + workColumns
	}
	w, err = scanWork(db.QueryRow(ctx, sql, workID, envID))
	if err == nil {
		return w, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("queue: stop %s: %w", workID, err)
	}
	// No row moved: the item as it stands, if the work API can see it.
	w, err = scanWork(db.QueryRow(ctx,
		`SELECT `+workColumns+` FROM work_items
		 WHERE id = $1 AND environment_id = $2`+workAPIScope,
		workID, envID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrWorkNotFound
	}
	if err != nil {
		return nil, false, fmt.Errorf("queue: stop %s: %w", workID, err)
	}
	return w, false, nil
}

// UpdateMetadata applies a metadata patch to a work item and returns the updated
// item (the wire Update responds with the BetaSelfHostedWork). upserts sets or
// overwrites keys; deletes removes keys; both are applied in one atomic UPDATE
// (metadata || upserts, then minus deletes), so a concurrent worker state
// transition on the same row cannot be lost to a read-modify-write and two
// overlapping patches cannot drop each other's writes — work items carry no
// optimistic version to guard a read-modify-write with, unlike the versioned
// resources. The patch is orthogonal to lifecycle: any item visible to the work
// API (see workAPIScope) is patchable in any state. An item not visible is
// ErrWorkNotFound.
func (q *Queue) UpdateMetadata(ctx context.Context, envID, workID domain.ID, upserts map[string]string, deletes []string) (*Work, error) {
	if upserts == nil {
		upserts = map[string]string{} // marshal to {} not null, so `metadata || $` is a merge
	}
	if deletes == nil {
		deletes = []string{} // an empty text[] removes nothing; a nil slice encodes as SQL NULL and would null the column
	}
	patch, err := json.Marshal(upserts)
	if err != nil {
		return nil, fmt.Errorf("queue: update metadata %s: %w", workID, err)
	}
	w, err := scanWork(q.pool.QueryRow(ctx,
		`UPDATE work_items
		 SET metadata   = (metadata || $3::jsonb) - $4::text[],
		     updated_at = now()
		 WHERE id = $1 AND environment_id = $2`+workAPIScope+`
		 RETURNING `+workColumns,
		workID, envID, string(patch), deletes))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWorkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("queue: update metadata %s: %w", workID, err)
	}
	return w, nil
}
