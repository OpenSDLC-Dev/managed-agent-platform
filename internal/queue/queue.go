// Package queue is the internal work queue over Postgres (FOR UPDATE SKIP
// LOCKED, per the plan's component 4). Five kinds share the work_items table —
// model_turn, tool_exec, web_exec, outputs_harvest and mcp_exec — and there are
// two ways to take one, which is the distinction to hold on to.
//
// Claim is the in-process lane, for a caller holding a database handle: the
// brain claims model_turn, and the platform executor claims the rest. Poll is
// the wire lane behind the work API, and it serves exactly one thing — a
// tool_exec item on a self_hosted environment. A BYOC worker therefore never
// sees a model_turn row, nor any other kind, nor another environment's work.
//
// That single split is what makes the platform executor and the BYOC worker
// the same pull protocol at two deployment points: one tool_exec item goes
// in-process when its environment is cloud and out to the customer's worker
// when it is self_hosted, and Claim's own predicate says so
// (kind <> 'tool_exec' OR e.kind = 'cloud'). Each Kind below records why it
// falls where it does.
//
// Enqueue is idempotent per (session, thread, kind) while a live item exists,
// so event-append triggers can fire without double-scheduling; a claim leases
// the item and an expired lease makes it claimable again. A model_turn is one
// thread's (plan 35 decision 5; NULL thread = the primary), so sibling threads
// run turns concurrently; the exec kinds stay session-keyed — one shared
// sandbox, one item covers every thread's backlog.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Kind discriminates the work streams.
type Kind string

const (
	// ModelTurn drives the brain: one turn of the orchestration loop, claimed
	// in-process by whichever brain replica gets there first. It is never
	// offered on the wire — a turn calls the model with the deployment's
	// credentials and appends to the event log, neither of which a customer's
	// worker has any way to do.
	ModelTurn Kind = "model_turn"
	// ToolExec runs a session's suspended tool calls in a sandbox, and is the
	// one kind that reaches both deployment points: Claim serves it in-process
	// when the environment is cloud, Poll serves it over the wire when the
	// environment is self_hosted. Nothing else crosses that line.
	ToolExec Kind = "tool_exec"
	// WebExec is the web_fetch/web_search work (docs/plan/15_web-tools.md):
	// run by the platform executor's web driver in its own process — no
	// sandbox — for cloud AND self_hosted sessions alike. Poll never serves
	// it; the official worker implements only the six sandbox tools.
	WebExec Kind = "web_exec"
	// OutputsHarvest is the deliverables harvest (docs/plan/21_outcomes.md,
	// Decision 8): the cloud executor snapshots /mnt/session/outputs/ into
	// the files registry before a grading pass. Internal-only — the brain
	// enqueues it for cloud environments alone, and Poll never serves it (a
	// self_hosted sandbox is unreachable from the platform; the reference
	// worker has no file lane).
	OutputsHarvest Kind = "outputs_harvest"
	// MCPExec is the MCP work (docs/plan/29_mcp-toolset.md): the executor's
	// MCP driver discovers a session's MCP servers' tools into mcp_catalogs,
	// and answers the session's MCP tool calls. One item does one of the two —
	// a pass with a call outstanding answers calls and does not discover, since
	// the turn is stopped on that call while a listing is only ever wanted by a
	// turn that has not started. Like WebExec it runs in
	// the platform executor's process for cloud AND self_hosted sessions
	// alike — the SDK states three times that MCP tools are server-side, the
	// work API has no MCP surface, and the BYOC worker's contract is
	// agent.tool_use + agent.custom_tool_use only — so Poll never serves it.
	MCPExec Kind = "mcp_exec"
)

// ErrLeaseLost reports that the item is no longer this claimant's: its lease
// expired and another claim took over, or it already finished.
var ErrLeaseLost = errors.New("queue: work item lease lost")

// ErrWorkStalled reports that the holder stopped making progress for longer than
// its keeper's stall budget, so the keeper cancelled the work and stopped
// renewing. It is a distinct outcome from ErrLeaseLost on purpose: the lease was
// never taken from this claimant, it was given up (#383).
var ErrWorkStalled = errors.New("queue: work item stalled")

// Item is one claimed unit of work.
type Item struct {
	ID            domain.ID
	EnvironmentID domain.ID
	SessionID     domain.ID
	// ThreadID is the thread a model_turn belongs to; empty for the primary
	// thread and for every session-keyed exec kind.
	ThreadID domain.ID
	Kind     Kind
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
	// Chain is how many consecutive times this item has already been handed
	// back by RequeueSettlement — a delegated turn that answered its own
	// calls and had nothing left to wait for. It rides on the item rather
	// than in the claimant, because the next turn of a chain may be claimed
	// by a different brain. Zero on a fresh item and on every kind but
	// model_turn; the cap that reads it is internal/brain/delegate.go's.
	Chain int
	// TraceContext is the W3C trace context captured at enqueue, so the
	// claimant can parent its work on the turn that produced it — the same
	// context a BYOC worker gets from its poll response (see Work). Nil when
	// the item was enqueued with no active span.
	TraceContext map[string]string
}

// Work is a work_items row projected for the wire work API (poll/get/list and
// the state-transition endpoints). Unlike Item (a claimant's lease proof for the
// internal executor), it carries the fields a BetaSelfHostedWork response
// renders, including the lifecycle timestamps the state machine populates. Each
// nullable timestamp is null until its transition is reached (a queued item has
// none of them).
type Work struct {
	ID              domain.ID
	EnvironmentID   domain.ID
	SessionID       domain.ID
	State           string
	Metadata        map[string]string
	CreatedAt       time.Time
	AcknowledgedAt  *time.Time // set by ack (queued → starting)
	StartedAt       *time.Time // set by the first heartbeat (→ active)
	StopRequestedAt *time.Time // set by stop
	StoppedAt       *time.Time // set when the item reaches stopped
	// LastHeartbeat is the wire's latest_heartbeat_at — null until the worker
	// heartbeats, which a freshly polled (still-queued) item has not.
	LastHeartbeat *time.Time
	// TraceContext is the W3C trace context (traceparent/tracestate) captured at
	// enqueue from the active span, so the executor or worker that runs the item
	// can parent its tool-execution spans on the turn that produced the work. It
	// is control-plane-internal (nil when enqueued with no active span) and never
	// rendered into the wire work object's metadata — a poll carries it in a
	// response header instead (see the API layer).
	TraceContext map[string]string
}

// workColumns is the ordered projection every Work-returning query selects, so
// scanWork can decode any of them uniformly. Prefix with a table alias where a
// query joins (e.g. "t.").
const workColumns = `id, environment_id, session_id, state, metadata, created_at,
	acknowledged_at, started_at, stop_requested_at, stopped_at, last_heartbeat, trace_context`

// scanWork decodes a workColumns row into a Work.
func scanWork(row pgx.Row) (*Work, error) {
	var w Work
	if err := row.Scan(&w.ID, &w.EnvironmentID, &w.SessionID, &w.State, &w.Metadata,
		&w.CreatedAt, &w.AcknowledgedAt, &w.StartedAt, &w.StopRequestedAt, &w.StoppedAt,
		&w.LastHeartbeat, &w.TraceContext); err != nil {
		return nil, err
	}
	return &w, nil
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
// created; false means an existing live item already covers the work. The
// item is the primary thread's (a model_turn) or session-keyed (the exec
// kinds) — EnqueueThread is the child-thread form.
func (q *Queue) Enqueue(ctx context.Context, db DB, envID, sessionID domain.ID, kind Kind) (bool, error) {
	return q.EnqueueThread(ctx, db, envID, sessionID, "", kind)
}

// EnqueueThread is Enqueue for one thread's model_turn (empty threadID = the
// primary): the live-dedup key is (session, thread, kind), so each thread has
// at most one live turn and sibling threads' turns do not suppress each other.
// Exec kinds are session-keyed and take an empty threadID.
func (q *Queue) EnqueueThread(ctx context.Context, db DB, envID, sessionID, threadID domain.ID, kind Kind) (bool, error) {
	// Only executor-consumed work carries a trace context — tool_exec
	// (consumed by the cloud executor's Claim and the BYOC worker's poll),
	// web_exec (the executor's web driver), outputs_harvest (the deliverables
	// snapshot), and mcp_exec (the MCP driver) — so their consumer spans
	// parent on the enqueuing turn. A model_turn drives the brain, which opens
	// its own model_request span per turn and never reads this back, so
	// capturing it there would only persist an unread payload; leave it NULL.
	var traceCtx any
	if kind == ToolExec || kind == WebExec || kind == OutputsHarvest || kind == MCPExec {
		traceCtx = traceContextArg(ctx)
	}
	tag, err := db.Exec(ctx,
		`INSERT INTO work_items (id, environment_id, session_id, thread_id, kind, trace_context)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb)
		 ON CONFLICT (session_id, thread_id, kind) WHERE state IN ('queued', 'starting', 'active')
		 DO NOTHING`,
		domain.NewID("work"), envID, sessionID, nullableThread(threadID), kind, traceCtx)
	if err != nil {
		return false, fmt.Errorf("queue: enqueue %s for %s: %w", kind, sessionID, err)
	}
	created := tag.RowsAffected() == 1
	// A committed tool_exec item may have a worker long-polling for it: ride
	// the work-items NOTIFY on the caller's handle — inside a transaction it
	// is delivered on commit — so the work API's blocked poll re-polls right
	// away (#74). Only tool_exec, the one kind Poll serves, wakes anyone;
	// the executor- and brain-claimed kinds are pulled on their own cadence.
	if created && kind == ToolExec {
		if err := events.NotifyWorkEnqueued(ctx, db, envID); err != nil {
			return false, fmt.Errorf("queue: enqueue %s for %s: notify: %w", kind, sessionID, err)
		}
	}
	return created, nil
}

// traceContextArg captures the active span's W3C trace context (traceparent/
// tracestate) as a JSON string for the work item's trace_context column, so an
// executor or worker that later runs the item can parent its tool-execution
// spans on the enqueuing turn. It returns nil — SQL NULL — when no span is
// active, and degrades to nil on the (practically impossible) marshal failure,
// since trace context is best-effort observability, never correctness.
func traceContextArg(ctx context.Context) any {
	carrier := map[string]string{}
	telemetry.Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil
	}
	b, err := json.Marshal(carrier)
	if err != nil {
		return nil
	}
	return string(b)
}

// Claim leases the oldest available item of the kind: queued items first-come
// first-served, plus active items whose lease expired (their claimant died).
// It returns nil with no error when there is nothing to do.
//
// tool_exec claims are scoped to cloud environments — the platform-managed
// executor is the cloud hands. A self_hosted environment's tool_exec work is
// served only by Poll (a BYOC worker), never Claim, so an item a worker has
// polled can never also be run by the executor. Every other kind is claimed
// for every environment: the brain (model calls) and the executor's web
// driver (web_fetch/web_search) run on the platform regardless of where a
// session's sandbox lives.
func (q *Queue) Claim(ctx context.Context, kind Kind, ttl time.Duration) (*Item, error) {
	var it Item
	var prevState string
	err := q.pool.QueryRow(ctx,
		`WITH picked AS (
		    SELECT w.id, w.state FROM work_items w
		    JOIN environments e ON e.id = w.environment_id
		    WHERE w.kind = $1
		      AND (w.kind <> 'tool_exec' OR e.kind = 'cloud')
		      AND (w.state = 'queued' OR (w.state = 'active' AND w.lease_expires_at < now()))
		    ORDER BY w.created_at
		    FOR UPDATE OF w SKIP LOCKED
		    LIMIT 1
		 )
		 UPDATE work_items w
		 SET state = 'active',
		     lease_expires_at = now() + make_interval(secs => $2),
		     updated_at = now()
		 FROM picked p
		 WHERE w.id = p.id
		 RETURNING w.id, w.environment_id, w.session_id, COALESCE(w.thread_id, ''), w.kind,
		           w.lease_expires_at, p.state, w.trace_context,
		           CASE WHEN jsonb_typeof(w.metadata->'settlement_chain') = 'number'
		                THEN (w.metadata->>'settlement_chain')::int ELSE 0 END`,
		kind, ttl.Seconds()).Scan(&it.ID, &it.EnvironmentID, &it.SessionID, &it.ThreadID, &it.Kind, &it.Lease,
		&prevState, &it.TraceContext, &it.Chain)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("queue: claim %s: %w", kind, err)
	}
	it.Reclaimed = prevState == "active"
	return &it, nil
}

// Poll reserves the oldest queued tool_exec item for one environment and hands
// it back to a BYOC worker. This is the wire work API's poll: unlike Claim (the
// executor's queued→active lease), poll is a soft reservation — the item stays
// queued, and the separate ack transitions it to starting. The reservation is
// recorded as a lease pushed out by reclaim, so a concurrent poll won't
// re-hand-out the same item until the window lapses (the wire's
// reclaim_older_than_ms — the reference's "reclaim un-ack'd work" knob). It
// returns nil with no error when the environment's tool_exec queue is empty.
// model_turn work drives the platform's own brain and is never offered to a
// worker.
//
// Poll reclaims two kinds of stranded item. A still-queued (un-acked)
// reservation whose window lapsed is re-offered — the wire's reclaim_older_than_ms
// knob, carried in the reclaim argument. AND a dead worker's already-acked
// (starting) or heartbeating (active) item whose lease has lapsed
// (lease_expires_at < now(), i.e. the worker stopped heartbeating) is reclaimed:
// it is reset to a fresh queued reservation (state → queued; last_heartbeat,
// acknowledged_at, started_at cleared, so it is indistinguishable on the wire
// from a never-run queued item) so the next worker can re-poll, re-ack, and
// re-claim it with a fresh NO_HEARTBEAT — the mirror of Claim's expired-active
// reclaim for cloud. Note the lease a starting/active item is reclaimed on is a
// real lease (Ack installs a startup lease, heartbeats extend it), not the
// un-acked poll reservation. The active-item reclaim keys on the lapsed lease,
// NOT on reclaim_older_than_ms (which stays the un-acked-reservation window, per
// the wire). The C2a driver re-derives work from the still-unanswered tool uses,
// so a reclaimed run re-executes only unanswered tools.
//
// Poll also FINALIZES the one stranded item it must never re-offer. A graceful
// stop is finished by the worker that was asked for it, but a worker that dies
// mid-wind-down never gets there, and re-offering a stopping item would resurrect
// work a caller asked to stop. Its lease lapses like any other holder's, and that
// is the signal: a stopping item whose lease has lapsed has nobody left to finish
// it, so the poll settles it terminally (→ stopped, stopped_at stamped, lease
// cleared) instead of leaving it non-terminal forever (#25). Such an item always
// carries a lease to lapse, because a graceful stop only enters stopping from
// active (see Stop) — the null-lease arm is not for it, but for the one row the
// new state machine does not write: during a rolling upgrade a not-yet-upgraded
// replica can still park a never-polled queued item, which has no lease at all,
// in stopping. Migration 0014 finalizes the ones written before the upgrade;
// ListAbandoned + FinalizeAbandoned — run by the work API ahead of every
// poll, so the re-arm the finalize owes (plan 35 decision 13 iii) happens
// under the session lock the poll itself cannot take, in the same
// transaction as the flip to stopped — catch the ones written during it.
// Concurrent polls of one environment serialize per session on that lock,
// and the loser's guarded flip is a no-op; the list is bounded so a poll
// costs a bounded read even in the pathological case (an environment whose
// whole fleet died mid-wind-down across many sessions); the remainder drains
// on the polls that follow, and no row can starve because a finalized one
// leaves the set. No ORDER BY: every row in the set is equally abandoned, so
// which 50 go first does not matter, and ordering would buy a sort the bound
// exists to avoid. They need no environment-kind guard of their own: only
// the work API's Stop produces a stopping row, and that is already scoped to
// a self_hosted tool_exec item.
//
// Every RE-hand-out mints a fresh work id (#62). A work item's identity is stable
// only while one worker holds it, because the wire's lifecycle calls carry no
// ownership proof — stop's body is {force} only, and the work object has no
// generation/version field — so re-offering the same id let a hung-then-revived
// worker's force-stop land on whatever worker held the item next, re-stranding
// the session. Under a rotated id that worker's stop, ack, and heartbeat all
// address an id that no longer exists (ErrWorkNotFound → 404) while the
// replacement's item is untouched. The three 404s are safe by different routes,
// not one: our worker's heartbeat reads a 4xx as a lost lease and cancels the
// run, a 404 stop is logged and dropped, and a 404 ack is dropped as an empty
// poll (routine hand-off, not a fault to back off from). The row, and with it
// the item's metadata and trace context, carries over unchanged.
//
// The FIRST hand-out keeps the id enqueue minted — no worker has ever held it, so
// there is nothing to invalidate, and a client that listed the queued item can
// still address it. That is exactly the lease_expires_at IS NULL case: every
// other branch matched on a lease the item was previously handed out under. Ids
// are opaque to the client, so the wire is unchanged. Claim needs no equivalent:
// the cloud executor proves ownership with the lease itself (see Item.Lease).
//
// Poll serves only self_hosted environments — the mirror of Claim scoping
// tool_exec to cloud. The two are therefore mutually exclusive by environment
// kind, so an item a worker has polled is never also run by the executor even
// if an environment key were misconfigured against a cloud environment.
func (q *Queue) Poll(ctx context.Context, envID domain.ID, reclaim time.Duration) (*Work, error) {
	w, err := scanWork(q.pool.QueryRow(ctx,
		`WITH picked AS (
		    SELECT w.id AS pid FROM work_items w
		    JOIN environments e ON e.id = w.environment_id
		    WHERE w.environment_id = $1 AND e.kind = 'self_hosted'
		      AND w.kind = 'tool_exec'
		      AND (
		            (w.state = 'queued' AND (w.lease_expires_at IS NULL OR w.lease_expires_at < now()))
		         OR (w.state IN ('starting', 'active') AND w.lease_expires_at < now())
		      )
		    ORDER BY w.created_at
		    FOR UPDATE OF w SKIP LOCKED
		    LIMIT 1
		 )
		 UPDATE work_items t
		 SET id               = CASE WHEN t.lease_expires_at IS NULL THEN t.id ELSE $3 END,
		     state            = 'queued',
		     last_heartbeat   = NULL,
		     acknowledged_at  = NULL,
		     started_at       = NULL,
		     lease_expires_at = now() + make_interval(secs => $2),
		     updated_at       = now()
		 FROM picked p
		 WHERE t.id = p.pid
		 RETURNING `+workColumns,
		envID, reclaim.Seconds(), domain.NewID("work")))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("queue: poll %s: %w", envID, err)
	}
	return w, nil
}

// AbandonedWork names one abandoned wind-down — a stopping tool_exec item
// whose lease lapsed or never existed (see Poll) — and the session it served.
type AbandonedWork struct {
	ID        domain.ID
	SessionID domain.ID
}

// ListAbandoned lists the environment's abandoned wind-downs: candidates for
// FinalizeAbandoned, which re-checks each row under its own transaction — so
// this is a plain bounded read, and a candidate a concurrent poll settles
// first is simply not settled twice. Bounded to 50 per call; the rest drain
// on the polls that follow.
func (q *Queue) ListAbandoned(ctx context.Context, envID domain.ID) ([]AbandonedWork, error) {
	rows, err := q.pool.Query(ctx,
		`SELECT id, session_id FROM work_items
		 WHERE environment_id = $1 AND kind = 'tool_exec'
		   AND state = 'stopping'
		   AND (lease_expires_at IS NULL OR lease_expires_at < now())
		 LIMIT 50`, envID)
	if err != nil {
		return nil, fmt.Errorf("queue: list abandoned %s: %w", envID, err)
	}
	defer rows.Close()
	var out []AbandonedWork
	for rows.Next() {
		var id, sid string
		if err := rows.Scan(&id, &sid); err != nil {
			return nil, err
		}
		out = append(out, AbandonedWork{ID: domain.ID(id), SessionID: domain.ID(sid)})
	}
	return out, rows.Err()
}

// FinalizeAbandoned settles one abandoned wind-down terminally (→ stopped,
// stopped_at stamped, lease cleared) under the caller's transaction — the
// transaction that also re-arms the session's runnable calls (plan 35
// decision 13 iii): a stop the worker never finished is a stop all the same,
// and the calls the item covered are nobody's otherwise, so the flip and the
// re-arm it owes commit or fail together — split, a crash between them would
// strand the calls behind a row already stopped, which no later poll
// retries. The flip re-checks the abandonment, so a stale candidate — one a
// concurrent poll settled first — reports false and is left alone.
func (q *Queue) FinalizeAbandoned(ctx context.Context, db DB, envID, workID domain.ID) (bool, error) {
	tag, err := db.Exec(ctx,
		`UPDATE work_items
		 SET state            = 'stopped',
		     stopped_at       = now(),
		     lease_expires_at = NULL,
		     updated_at       = now()
		 WHERE id = $1 AND environment_id = $2 AND kind = 'tool_exec'
		   AND state = 'stopping'
		   AND (lease_expires_at IS NULL OR lease_expires_at < now())`,
		workID, envID)
	if err != nil {
		return false, fmt.Errorf("queue: finalize abandoned %s: %w", workID, err)
	}
	return tag.RowsAffected() > 0, nil
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
	return q.requeue(ctx, db, item, 0)
}

// RequeueSettlement is Requeue for the one chain nobody feeds. A delegated
// turn whose calls were all answered in the settlement itself has nothing
// left to wait for, so it hands its own model_turn straight back — and the
// shapes that reach it are ones a model repeats, so left alone it loops with
// no delay, no budget and no end (#442). This records how long the run is;
// the brain caps it (internal/brain/delegate.go). A chain taken because input
// arrived mid-turn passes 0: that input is progress, and resetting is what
// keeps a busy coordinator from tripping a cap meant for a stuck one — as
// does plain Requeue, which clears the count for the same reason.
//
// The count lives in metadata because work_items already has the column, so
// no migration. It bounds a thread that keeps chaining its own turn with
// nothing feeding it; a peer waking this thread enqueues a fresh item and so
// starts a fresh run, deliberately, because a message from another agent is
// input like any other. Requires the lease, exactly as Requeue does.
func (q *Queue) RequeueSettlement(ctx context.Context, db DB, item *Item, chain int) error {
	return q.requeue(ctx, db, item, chain)
}

// requeue is the shared body of the two above: the lease proof is written
// once, because a claim's ownership rule must not be able to differ between
// two paths that both hand an item back.
//
// chain is the only difference. A positive count writes the settlement key; 0
// removes it, which is what makes Requeue's meaning "whatever unfed run was
// in flight has ended" — any other reason to chain an item is progress. The
// removal is a no-op on the rows that never carried the key, so a tool_exec
// item's client-visible metadata is untouched by the executor's own requeue;
// and since only the delegation settlement passes a positive count, and it
// only ever holds a model_turn item, the key cannot reach the wire work
// object either way (workAPIScope is tool_exec only).
func (q *Queue) requeue(ctx context.Context, db DB, item *Item, chain int) error {
	tag, err := db.Exec(ctx,
		`UPDATE work_items
		 SET state = 'queued', lease_expires_at = NULL, updated_at = now(),
		     metadata = CASE WHEN $3::int > 0
		                     THEN jsonb_set(metadata, '{settlement_chain}', to_jsonb($3::int), true)
		                     ELSE metadata - 'settlement_chain' END
		 WHERE id = $1 AND state = 'active' AND lease_expires_at = $2`,
		item.ID, item.Lease, chain)
	if err != nil {
		return fmt.Errorf("queue: requeue %s: %w", item.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("queue: requeue %s: %w", item.ID, ErrLeaseLost)
	}
	return nil
}

// Assert verifies the claimant still owns the item, inside the caller's
// transaction. Session state written mid-turn (the reclaim recovery
// announcement) must carry this proof like every other state write: a
// claimant that stalled past its lease could otherwise flip a session
// another brain has since settled.
//
// It locks the work row rather than merely reading it, because an unlocked read
// is only a proof at the instant it runs: a reclaim committing between the read
// and the caller's commit would leave two holders writing the same session. The
// lock lasts as long as the caller's transaction, which is the only place this
// belongs — handed a pool it is a bare read again, the row lock ending with the
// implicit transaction the statement ran in. The
// lock closes that window from both sides — Claim takes its rows FOR UPDATE
// SKIP LOCKED, so it skips a row being asserted rather than blocking on it, and
// a reclaim that got there first has already changed lease_expires_at, which
// this read then fails on. The window is widest where nothing renews the lease
// while the caller settles, which is exactly the stalled executor's partial
// commit (#383).
func (q *Queue) Assert(ctx context.Context, db DB, item *Item) error {
	var one int
	err := db.QueryRow(ctx,
		`SELECT 1 FROM work_items WHERE id = $1 AND state = 'active' AND lease_expires_at = $2 FOR UPDATE`,
		item.ID, item.Lease).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("queue: assert %s: %w", item.ID, ErrLeaseLost)
	}
	if err != nil {
		return fmt.Errorf("queue: assert %s: %w", item.ID, err)
	}
	return nil
}

// CancelSession stops every work item this session still has in flight, inside
// the caller's transaction — what a user.interrupt does to the turn it ends.
// Unlike Complete it carries no lease proof, because it is not a claimant
// finishing its own work: it takes the work away from whoever holds it. That is
// the point. A claimant only ever writes under a lease it re-asserts in the same
// transaction (Complete, Requeue, Assert), so a brain or executor still running
// the interrupted turn finds its item gone and rolls its whole settlement back,
// which is what keeps a stale half-turn — duplicate tool results, tool intents
// nothing may answer — off the append-only log. Cancelling under the session row
// lock the interrupt already holds is what makes that a decision and not a race.
//
// It also frees the (session, kind) slot Enqueue is idempotent on, so a
// user.message in the same batch can schedule the redirect turn immediately.
func (q *Queue) CancelSession(ctx context.Context, db DB, sessionID domain.ID) error {
	if _, err := db.Exec(ctx,
		`UPDATE work_items
		 SET state             = 'stopped',
		     stop_requested_at = COALESCE(stop_requested_at, now()),
		     stopped_at        = now(),
		     lease_expires_at  = NULL,
		     updated_at        = now()
		 WHERE session_id = $1 AND state <> 'stopped'`, sessionID); err != nil {
		return fmt.Errorf("queue: cancel session %s: %w", sessionID, err)
	}
	return nil
}

// CancelThread is CancelSession for one thread's model_turn — a thread-scoped
// user.interrupt ends that thread's turn and nothing else (plan 35 decision 9):
// the session-keyed exec items stay live, because a sibling's calls ride on
// them; the interrupted thread's own in-flight call is dropped by the drivers'
// answered-means-cancelled check, not by stopping the item.
func (q *Queue) CancelThread(ctx context.Context, db DB, sessionID, threadID domain.ID) error {
	if _, err := db.Exec(ctx,
		`UPDATE work_items
		 SET state             = 'stopped',
		     stop_requested_at = COALESCE(stop_requested_at, now()),
		     stopped_at        = now(),
		     lease_expires_at  = NULL,
		     updated_at        = now()
		 WHERE session_id = $1 AND kind = 'model_turn' AND state <> 'stopped'
		   AND thread_id IS NOT DISTINCT FROM $2`, sessionID, nullableThread(threadID)); err != nil {
		return fmt.Errorf("queue: cancel thread %s: %w", threadID, err)
	}
	return nil
}

// nullableThread binds a thread id: NULL for the primary.
func nullableThread(threadID domain.ID) *string {
	if threadID == "" {
		return nil
	}
	s := threadID.String()
	return &s
}

// Complete marks the item finished, in the caller's transaction when one is
// passed (a turn's settlement completes its item atomically with the state
// it writes, so a concurrent trigger serialized behind the same session lock
// always sees either a live item or a completed one — never a gap). Losing
// the lease first (another claimant took over after expiry) is an error: the
// caller's work may have raced the replacement's and must not be treated as
// cleanly finished.
func (q *Queue) Complete(ctx context.Context, db DB, item *Item) error {
	tag, err := db.Exec(ctx,
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
