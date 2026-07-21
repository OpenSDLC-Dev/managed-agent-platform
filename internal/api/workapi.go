package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
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
	StartedAt         *time.Time        `json:"started_at"`
	State             string            `json:"state"`
	StopRequestedAt   *time.Time        `json:"stop_requested_at"`
	StoppedAt         *time.Time        `json:"stopped_at"`
	Type              string            `json:"type"` // always "work"
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
// block_ms is accepted but not yet honoured: this is a non-blocking poll that
// returns immediately, and the reference client already spaces empty polls with
// a client-side jitter sleep, so the protocol is unchanged — only chattier.
// True long-poll (hold the request open on a work_items NOTIFY) is a later
// enhancement.
func (s *server) pollWork(w http.ResponseWriter, r *http.Request) {
	envID, _, err := s.workScope(r) // poll has no work_id path value; ignore it
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
	item, err := s.queue.Poll(r.Context(), envID, reclaimWindow(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if item == nil {
		writeJSON(w, http.StatusOK, nil) // empty queue → 200 with a null body
		return
	}
	// TraceContext holds only the W3C keys telemetry.Inject wrote (traceparent,
	// optional tracestate); Set canonicalises them, and the worker's Header.Get
	// canonicalises to match.
	for k, v := range item.TraceContext {
		w.Header().Set(k, v)
	}
	writeJSON(w, http.StatusOK, toWire(item))
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
	if _, err := s.queue.Stop(r.Context(), envID, workID, force); err != nil {
		return mapWorkErr(err)
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
