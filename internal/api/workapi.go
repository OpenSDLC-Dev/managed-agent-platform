package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/worktoken"
	"github.com/jackc/pgx/v5"
)

// defaultReclaimMs is the wire default for reclaim_older_than_ms: a work item
// handed to a worker but not acknowledged within this window becomes pollable
// again, so a worker that dies between poll and ack never strands its item.
const defaultReclaimMs = 5000

// maxReclaimMs caps reclaim_older_than_ms. Beyond a sane bound the value is
// meaningless (no worker waits ten minutes to ack), and a caller-supplied
// int large enough to overflow time.Duration would wrap negative — a past
// reservation that defeats the soft handout. Clamping closes both.
const maxReclaimMs = 600_000 // 10 minutes

// maxBlockMs caps block_ms at the reference server's ceiling: the SDK's own
// work poller documents that "the server caps this at 999" and sends exactly
// 999 (anthropic-sdk-go lib/environments/poller.go).
const maxBlockMs = 999

// workData is a work item's payload. For every self-hosted work item it is a
// reference to the session the worker attaches to — the tool_use events the
// worker runs live on that session's event log, and it posts results back
// there (there is no result endpoint on the work API).
type workData struct {
	ID   string `json:"id"`
	Type string `json:"type"` // always "session"
}

// workWire is the BetaSelfHostedWork response shape, field for field. Every
// field is required on the wire, including the lifecycle timestamps that a
// still-queued item has not reached — those render as null (a queued item has
// not been acknowledged, started, or stopped).
type workWire struct {
	ID                string            `json:"id"`
	AcknowledgedAt    *time.Time        `json:"acknowledged_at"`
	CreatedAt         time.Time         `json:"created_at"`
	Data              workData          `json:"data"`
	EnvironmentID     string            `json:"environment_id"`
	LatestHeartbeatAt *time.Time        `json:"latest_heartbeat_at"`
	Metadata          map[string]string `json:"metadata"`
	// Secret is the reference's credential payload for the worker to execute the
	// item with: on the poll response of an item whose session attaches a
	// memory store, the sessions token in the envelope the reference worker
	// decodes (internal/worktoken); null on every other retrieval path, and for
	// every other item (plan 36 decision 15). #165's vault-credential bundle
	// would be a second key in the same envelope.
	Secret          *string    `json:"secret"`
	StartedAt       *time.Time `json:"started_at"`
	State           string     `json:"state"`
	StopRequestedAt *time.Time `json:"stop_requested_at"`
	StoppedAt       *time.Time `json:"stopped_at"`
	Type            string     `json:"type"` // always "work"
}

// toWire maps a queue row onto the wire work item. Lifecycle timestamps a work
// item has not reached are null; the state-transition endpoints populate them.
func toWire(w *queue.Work) workWire {
	meta := w.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	return workWire{
		ID:                w.ID.String(),
		AcknowledgedAt:    utcPtr(w.AcknowledgedAt),
		CreatedAt:         w.CreatedAt.UTC(),
		Data:              workData{ID: w.SessionID.String(), Type: "session"},
		EnvironmentID:     w.EnvironmentID.String(),
		LatestHeartbeatAt: utcPtr(w.LastHeartbeat),
		Metadata:          meta,
		StartedAt:         utcPtr(w.StartedAt),
		State:             w.State,
		StopRequestedAt:   utcPtr(w.StopRequestedAt),
		StoppedAt:         utcPtr(w.StoppedAt),
		Type:              "work",
	}
}

// claimWork is the poll's claim: Poll's statement and, for an item whose
// session attaches a memory store, the sessions token's row, in one
// transaction — an item is never leased without the credential its worker
// needs, and a token insert that fails leaves the item unclaimed for the
// next poll (plan 36 decision 15). The rendered secret comes back beside the
// item; nil for a storeless session, whose item is what it was before the
// plan, byte for byte.
func (s *server) claimWork(ctx context.Context, envID domain.ID, reclaim time.Duration) (*queue.Work, *string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	item, err := s.queue.PollOn(ctx, tx, envID, reclaim)
	if err != nil || item == nil {
		return nil, nil, err
	}
	var withStore bool
	if err := tx.QueryRow(ctx,
		`SELECT resources @> '[{"type": "memory_store"}]'::jsonb FROM sessions WHERE id = $1`,
		item.SessionID.String()).Scan(&withStore); err != nil {
		return nil, nil, err
	}
	var secret *string
	if withStore {
		token, err := worktoken.Mint(ctx, tx, item.ID.String(), item.SessionID.String())
		if err != nil {
			return nil, nil, err
		}
		rendered := worktoken.Secret(token)
		secret = &rendered
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return item, secret, nil
}

// pollWork is the wire work API's long poll (GET .../work/poll). It hands the
// oldest queued tool_exec item for this environment to a BYOC worker, or 200 +
// null when the queue is empty.
//
// Unlike the other work endpoints it is a full http.HandlerFunc rather than a
// typed handler: on a hit it emits the item's enqueue-time W3C trace context as
// response headers (traceparent/tracestate) so the worker can parent its
// tool-execution spans on the turn that produced the work — one trace across the
// control-plane→worker boundary. The trace context rides a header, never the
// wire body: toWire deliberately omits it (it lives in a dedicated column, out
// of the client-facing metadata namespace).
//
// block_ms (validated in blockWindow) holds an empty poll open: the handler
// subscribes to the broker keyed by the environment id before the first poll —
// so an enqueue committed once coverage is active can never slip between poll
// and wait — then loops poll → wait until a work-items NOTIFY (fired by
// queue.Enqueue), the window's deadline, or the client disconnecting ends it.
// The deadline arm polls once more before answering null, because a wake can
// race the timer. Availability that arrives without an enqueue (a lapsed
// reservation or lease reclaim) has no NOTIFY and is found by the next poll —
// the window is capped at 999ms, so that discovery is at most one window late,
// and the reference client spaces its empty polls with a jitter sleep besides.
// The broker's listener is only held while subscribers exist, so a lone idle
// worker cycles the LISTEN connection once per poll — accepted: any concurrent
// subscriber (another poll, an SSE stream) keeps it alive.
func (s *server) pollWork(w http.ResponseWriter, r *http.Request) {
	envID, _, err := s.workScope(r) // poll has no work_id path value; ignore it
	if err != nil {
		writeError(w, r, err)
		return
	}
	block, err := blockWindow(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Record the worker's poll for the workers_polling stat before handing out
	// work. It is a second round-trip on the poll path, but a single cheap upsert;
	// a poll without the Anthropic-Worker-ID header is simply not attributed to a
	// worker, and a tracking failure is best-effort — it must not fail the poll.
	if wid := r.Header.Get("Anthropic-Worker-ID"); wid != "" {
		if err := s.queue.RecordPoll(r.Context(), envID, wid); err != nil {
			slog.WarnContext(r.Context(), "record worker poll", "environment", envID, "error", err)
		}
	}
	reclaim := reclaimWindow(r)
	deadline := time.Now().Add(block)
	var sub *events.Subscription
	if block > 0 {
		sub = s.broker.Subscribe(envID)
		defer sub.Close()
		// Wait for LISTEN coverage before the first poll, bounded by the
		// window: a broker that cannot establish coverage degrades this to a
		// plain timed wait instead of holding the request past its window.
		readyCtx, cancel := context.WithDeadline(r.Context(), deadline)
		_ = s.broker.Ready(readyCtx)
		cancel()
	}
	for {
		// Abandoned wind-downs first: a stopping tool_exec whose worker never
		// finished is finalized and its session re-armed (plan 35 decision
		// 13 iii), so the poll below hands the fresh item out. Opportunistic
		// cleanup: a transient finalize failure must not fail work delivery —
		// the rows stay stopping and the next poll retries.
		if err := s.finalizeAbandoned(r.Context(), envID); err != nil {
			slog.WarnContext(r.Context(), "finalize abandoned wind-downs", "environment", envID, "error", err)
		}
		item, secret, err := s.claimWork(r.Context(), envID, reclaim)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if item != nil {
			// TraceContext holds only the W3C keys telemetry.Inject wrote
			// (traceparent, optional tracestate); Set canonicalises them, and
			// the worker's Header.Get canonicalises to match.
			for k, v := range item.TraceContext {
				w.Header().Set(k, v)
			}
			wire := toWire(item)
			wire.Secret = secret
			writeJSON(w, http.StatusOK, wire)
			return
		}
		remaining := time.Until(deadline)
		if sub == nil || remaining <= 0 {
			writeJSON(w, http.StatusOK, nil) // empty queue → 200 with a null body
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-r.Context().Done():
			timer.Stop()
			writeJSON(w, http.StatusOK, nil) // client gone; the write is moot
			return
		case <-timer.C:
			// Deadline: loop for one final poll (a wake can race the timer),
			// which the remaining <= 0 branch then answers.
		case <-sub.Wake():
			timer.Stop()
		}
	}
}

// blockWindow reads block_ms — how long an empty poll is held open before
// answering null. Absent means non-blocking (the wire default). Unlike the
// reclaim knob this one is validated: the SDK records that the reference
// rejects an explicit 0 (non-blocking is expressed by omission —
// anthropic-sdk-go lib/environments/poller.go), so zero, negative, empty
// (present-but-valueless, which is not omission), unparseable, and repeated
// values are 400; an over-cap value is clamped to the server ceiling the same
// source documents.
func blockWindow(r *http.Request) (time.Duration, error) {
	vs, ok := r.URL.Query()["block_ms"]
	if !ok {
		return 0, nil
	}
	if len(vs) != 1 {
		return 0, errInvalid("block_ms must be given at most once")
	}
	n, err := strconv.Atoi(vs[0])
	if err != nil || n < 1 {
		return 0, errInvalid("block_ms must be an integer between 1 and %d", maxBlockMs)
	}
	if n > maxBlockMs {
		n = maxBlockMs
	}
	return time.Duration(n) * time.Millisecond, nil
}

// listWork lists the environment's work items (GET .../work), newest first, in
// the {data, next_page} envelope. It is scoped like the rest of the work API to
// self_hosted tool_exec items, so a worker sees only the queue it can act on —
// never the brain's model_turn rows or another environment's work.
func (s *server) listWork(r *http.Request) (any, error) {
	envID, _, err := s.workScope(r) // list has no work_id path value; ignore it
	if err != nil {
		return nil, err
	}
	page, err := parsePage(r.URL.Query())
	if err != nil {
		return nil, err
	}
	after := false
	var afterT time.Time
	var afterID string
	if page.cur != nil {
		// Unidirectional list: only a forward time cursor is valid here.
		if page.cur.versioned || page.cur.seqKeyed || page.cur.dir != dirNext {
			return nil, errInvalid("invalid page cursor")
		}
		after, afterT, afterID = true, page.cur.t, page.cur.id
	}

	items, err := s.queue.ListWork(r.Context(), envID, after, afterT, afterID, page.limit+1)
	if err != nil {
		return nil, err
	}
	out := pageJSON{Data: []any{}}
	for i, w := range items {
		if i >= page.limit {
			break
		}
		out.Data = append(out.Data, toWire(w))
	}
	if len(items) > page.limit {
		last := items[page.limit-1]
		c := encodeTimeCursor(dirNext, last.CreatedAt, last.ID.String())
		out.NextPage = &c
	}
	return out, nil
}

// workStatsWire is the BetaSelfHostedWorkQueueStats response shape. oldest_queued_at
// is an RFC3339 timestamp or null (an empty queue); the other counts are always
// present.
type workStatsWire struct {
	Depth          int64      `json:"depth"`
	OldestQueuedAt *time.Time `json:"oldest_queued_at"`
	Pending        int64      `json:"pending"`
	Type           string     `json:"type"` // always "work_queue_stats"
	WorkersPolling int64      `json:"workers_polling"`
}

// statsWork reports work-queue statistics (GET .../work/stats): the queue depth
// (items waiting to be picked up), the pending count (polled but not acked), the
// oldest queued item's timestamp, and the number of workers that have polled in
// the last 30s. Scoped and authed like the rest of the work API — a worker sees
// only its own environment's self_hosted queue.
func (s *server) statsWork(r *http.Request) (any, error) {
	envID, _, err := s.workScope(r) // stats has no work_id path value; ignore it
	if err != nil {
		return nil, err
	}
	st, err := s.queue.Stats(r.Context(), envID)
	if err != nil {
		return nil, err
	}
	return workStatsWire{
		Depth:          st.Depth,
		OldestQueuedAt: utcPtr(st.OldestQueuedAt),
		Pending:        st.Pending,
		Type:           "work_queue_stats",
		WorkersPolling: st.WorkersPolling,
	}, nil
}

// heartbeatWire is the BetaSelfHostedWorkHeartbeatResponse shape.
type heartbeatWire struct {
	LastHeartbeat time.Time `json:"last_heartbeat"`
	LeaseExtended bool      `json:"lease_extended"`
	State         string    `json:"state"`
	TTLSeconds    int64     `json:"ttl_seconds"`
	Type          string    `json:"type"` // always "work_heartbeat"
}

const (
	defaultHeartbeatTTLSeconds = 30
	maxHeartbeatTTLSeconds     = 300
)

// workScope resolves the path environment, asserts the Bearer key authorises it
// (a key is scoped to one environment), and returns the environment and work ids.
func (s *server) workScope(r *http.Request) (envID, workID domain.ID, err error) {
	e := r.PathValue("id")
	if environmentFrom(r.Context()) != e {
		return "", "", errAuth("environment key is not valid for this environment")
	}
	return domain.ID(e), domain.ID(r.PathValue("work_id")), nil
}

// mapWorkErr maps a queue state-machine error onto its wire status: a missing
// item is 404, a conflicting-state stop is 409, a heartbeat precondition failure
// is 412. Anything else is an internal fault.
func mapWorkErr(err error) error {
	switch {
	case errors.Is(err, queue.ErrWorkNotFound):
		return errNotFound("work item not found")
	case errors.Is(err, queue.ErrWorkConflict):
		return errConflict("work item is already stopping or stopped")
	case errors.Is(err, queue.ErrHeartbeatMismatch):
		return &apiError{http.StatusPreconditionFailed, errTypeInvalidRequest,
			"expected_last_heartbeat does not match the current lease"}
	default:
		return err
	}
}

// getWork returns one work item (GET .../work/{work_id}).
func (s *server) getWork(r *http.Request) (any, error) {
	envID, workID, err := s.workScope(r)
	if err != nil {
		return nil, err
	}
	if err := checkWorkID(workID); err != nil {
		return nil, err
	}
	w, err := s.queue.GetWork(r.Context(), envID, workID)
	if err != nil {
		return nil, mapWorkErr(err)
	}
	return toWire(w), nil
}

// updateWork applies a metadata patch to a work item (POST .../work/{work_id})
// and returns the updated BetaSelfHostedWork. The body is {"metadata": {...}}: a
// string value upserts a key, an explicit null deletes it, and omitted keys are
// preserved — the same patch semantics as session/agent metadata. The patch is
// orthogonal to lifecycle state; any item the work API can see is patchable.
func (s *server) updateWork(r *http.Request) (any, error) {
	envID, workID, err := s.workScope(r)
	if err != nil {
		return nil, err
	}
	obj, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(obj, "metadata"); err != nil {
		return nil, err
	}
	raw, ok := obj["metadata"]
	if !ok || isNull(raw) {
		return nil, errInvalid("metadata is required")
	}
	// The work wire deletes only on an explicit null; an empty string is a
	// literal value (unlike the environment rule), hence emptyDeletes=false.
	upserts, deletes, err := splitMetadataPatch(raw, false)
	if err != nil {
		return nil, err
	}
	// After the body is validated, so an empty/bad patch is still the 400 the
	// reference returns before an item lookup (a malformed work_id is a 404).
	if err := checkWorkID(workID); err != nil {
		return nil, err
	}
	w, err := s.queue.UpdateMetadata(r.Context(), envID, workID, upserts, deletes)
	if err != nil {
		return nil, mapWorkErr(err)
	}
	return toWire(w), nil
}

// ackWork acknowledges a polled item (POST .../work/{work_id}/ack), moving it
// queued → starting.
func (s *server) ackWork(r *http.Request) (any, error) {
	envID, workID, err := s.workScope(r)
	if err != nil {
		return nil, err
	}
	if err := checkWorkID(workID); err != nil {
		return nil, err
	}
	w, err := s.queue.Ack(r.Context(), envID, workID)
	if err != nil {
		return nil, mapWorkErr(err)
	}
	return toWire(w), nil
}

// heartbeatWork applies the optimistic-concurrency heartbeat (POST
// .../work/{work_id}/heartbeat).
func (s *server) heartbeatWork(r *http.Request) (any, error) {
	envID, workID, err := s.workScope(r)
	if err != nil {
		return nil, err
	}
	if err := checkWorkID(workID); err != nil {
		return nil, err
	}
	expected := r.URL.Query().Get("expected_last_heartbeat")
	if expected == "" {
		return nil, errInvalid("expected_last_heartbeat is required")
	}
	res, err := s.queue.Heartbeat(r.Context(), envID, workID, expected, heartbeatTTL(r))
	if err != nil {
		return nil, mapWorkErr(err)
	}
	return heartbeatWire{
		LastHeartbeat: res.LastHeartbeat.UTC(),
		LeaseExtended: res.LeaseExtended,
		State:         res.State,
		TTLSeconds:    res.TTLSeconds,
		Type:          "work_heartbeat",
	}, nil
}

// stopWork stops a work item (POST .../work/{work_id}/stop). Success is a
// bodiless 204: the reference service sends no body here even though the
// generated SDK method is typed `*BetaSelfHostedWork`, which is why the SDK's
// own work poller rebinds the response destination to bypass its strict decoder
// (anthropic-sdk-go lib/environments/poller.go, stopWork). A caller that needs
// the resulting state reads it back with GET .../work/{work_id}. An
// already-stopped item is 409, which the reference worker ignores.
func (s *server) stopWork(r *http.Request) error {
	envID, workID, err := s.workScope(r)
	if err != nil {
		return err
	}
	if err := checkWorkID(workID); err != nil {
		return err
	}
	force, err := parseStopForce(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The session row lock first (plan 35 decision 13 iii): a one-pass worker
	// that stops its item after its found set may leave calls a sibling
	// thread committed under the live item, and the re-arm below must be
	// serialized with every settlement and trigger that appends calls or
	// enqueues — not rest on ON CONFLICT's wait-and-recheck. An item outside
	// the work API's view has no session to lock; Stop reports it.
	var sessionID, kind string
	err = tx.QueryRow(ctx, `SELECT session_id, kind FROM work_items WHERE id = $1 AND environment_id = $2`,
		workID, envID).Scan(&sessionID, &kind)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sessionID); err != nil {
			return err
		}
	}
	w, err := s.queue.StopWith(ctx, tx, envID, workID, force)
	if err != nil {
		return mapWorkErr(err)
	}
	// A tool_exec that reached stopped is re-armed. A graceful stop that
	// parked the item stopping is not yet stopped; the worker's own stop after
	// its wind-down lands here again, and a wind-down the worker never
	// finishes is finalized — and re-armed — by the next poll.
	if w.State == "stopped" && kind == string(queue.ToolExec) {
		if err := s.rearm(ctx, tx, envID, domain.ID(sessionID)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// rearm queues a fresh exec item for the session's runnable platform calls
// after its tool_exec reached stopped (plan 35 decision 13 iii): the kind the
// send trigger would pick — mcp_exec first, the only driver that can answer
// one; the runnable set, so an ask-gated sibling call never loops stop →
// re-arm → nothing runnable → stop. Under the session row lock the caller
// holds, serialized with every settlement and trigger that appends calls.
func (s *server) rearm(ctx context.Context, tx pgx.Tx, envID, sessionID domain.ID) error {
	kind, err := execKindFor(ctx, tx, sessionID, nil, nil)
	if err != nil {
		return err
	}
	if kind == "" {
		return nil
	}
	_, err = s.queue.Enqueue(ctx, tx, envID, sessionID, kind)
	return err
}

// finalizeAbandoned finalizes the environment's abandoned wind-downs, each
// with the re-arm it owes in the same transaction, under the session row
// lock stopWork shares: committed together, a crash between them cannot
// strand the session's runnable calls behind a row already stopped — one
// still stopping is retried by the next poll. The guarded flip re-checks the
// candidate under the lock, so racing polls settle and re-arm each session
// exactly once. The lock is taken SKIP LOCKED: a session a settlement or a
// send holds is left for the next poll rather than blocking this one past
// its window.
func (s *server) finalizeAbandoned(ctx context.Context, envID domain.ID) error {
	items, err := s.queue.ListAbandoned(ctx, envID)
	if err != nil {
		return err
	}
	for _, it := range items {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		err = func() error {
			defer func() { _ = tx.Rollback(ctx) }()
			var one int
			err := tx.QueryRow(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE SKIP LOCKED`,
				it.SessionID.String()).Scan(&one)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			done, err := s.queue.FinalizeAbandoned(ctx, tx, envID, it.ID)
			if err != nil {
				return err
			}
			if !done {
				return nil
			}
			if err := s.rearm(ctx, tx, envID, it.SessionID); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

// heartbeatTTL reads desired_ttl_seconds (default 30, clamped to
// maxHeartbeatTTLSeconds). A non-positive or unparseable value falls back to the
// default.
func heartbeatTTL(r *http.Request) int64 {
	ttl := int64(defaultHeartbeatTTLSeconds)
	if v := r.URL.Query().Get("desired_ttl_seconds"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			ttl = n
		}
	}
	if ttl > maxHeartbeatTTLSeconds {
		ttl = maxHeartbeatTTLSeconds
	}
	return ttl
}

// parseStopForce reads the optional {force?:bool} stop body; an empty body means
// a graceful stop (force false).
func parseStopForce(r *http.Request) (bool, error) {
	var req struct {
		Force bool `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		return false, errInvalid("invalid stop request body: %v", err)
	}
	return req.Force, nil
}

// reclaimWindow reads reclaim_older_than_ms (default 5000, clamped to
// maxReclaimMs). A non-positive or unparseable value falls back to the default
// rather than erroring — the wire treats it as an optional tuning knob, not a
// validated field — and an over-large value is clamped so it can never overflow
// time.Duration into a past (negative) reservation.
func reclaimWindow(r *http.Request) time.Duration {
	ms := defaultReclaimMs
	if v := r.URL.Query().Get("reclaim_older_than_ms"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ms = n
		}
	}
	if ms > maxReclaimMs {
		ms = maxReclaimMs
	}
	return time.Duration(ms) * time.Millisecond
}
