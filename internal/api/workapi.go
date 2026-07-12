package api

import (
	"encoding/json"
	"errors"
	"io"
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
	w, err := s.queue.GetWork(r.Context(), envID, workID)
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

// stopWork stops a work item (POST .../work/{work_id}/stop) and returns the
// updated item. The wire Stop responds with the BetaSelfHostedWork (the SDK
// types it `*BetaSelfHostedWork`), not an empty 204 — a 204 breaks the SDK's
// typed decoder. An already-stopped item is 409, which the reference worker
// ignores.
func (s *server) stopWork(r *http.Request) (any, error) {
	envID, workID, err := s.workScope(r)
	if err != nil {
		return nil, err
	}
	force, err := parseStopForce(r)
	if err != nil {
		return nil, err
	}
	w, err := s.queue.Stop(r.Context(), envID, workID, force)
	if err != nil {
		return nil, mapWorkErr(err)
	}
	return toWire(w), nil
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
