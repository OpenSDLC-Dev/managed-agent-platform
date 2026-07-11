package api

import (
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

// toWire maps a queue row onto the wire work item. The lifecycle timestamps a
// queued item has not reached are left nil (null); the state-transition
// endpoints populate them.
func toWire(w *queue.Work) workWire {
	meta := w.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	return workWire{
		ID:                w.ID.String(),
		CreatedAt:         w.CreatedAt.UTC(),
		Data:              workData{ID: w.SessionID.String(), Type: "session"},
		EnvironmentID:     w.EnvironmentID.String(),
		LatestHeartbeatAt: utcPtr(w.LastHeartbeat),
		Metadata:          meta,
		State:             w.State,
		Type:              "work",
	}
}

// pollWork is the wire work API's long poll (GET .../work/poll). It hands the
// oldest queued tool_exec item for this environment to a BYOC worker, or 200 +
// null when the queue is empty.
//
// block_ms is accepted but not yet honoured: this is a non-blocking poll that
// returns immediately, and the reference client already spaces empty polls with
// a client-side jitter sleep, so the protocol is unchanged — only chattier.
// True long-poll (hold the request open on a work_items NOTIFY) is a later
// enhancement.
func (s *server) pollWork(r *http.Request) (any, error) {
	envID := r.PathValue("id")
	if environmentFrom(r.Context()) != envID {
		return nil, errAuth("environment key is not valid for this environment")
	}
	w, err := s.queue.Poll(r.Context(), domain.ID(envID), reclaimWindow(r))
	if err != nil {
		return nil, err
	}
	if w == nil {
		return nil, nil // empty queue → 200 with a null body
	}
	return toWire(w), nil
}

// reclaimWindow reads reclaim_older_than_ms (default 5000). A non-positive or
// unparseable value falls back to the default rather than erroring — the wire
// treats it as an optional tuning knob, not a validated field.
func reclaimWindow(r *http.Request) time.Duration {
	ms := defaultReclaimMs
	if v := r.URL.Query().Get("reclaim_older_than_ms"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ms = n
		}
	}
	return time.Duration(ms) * time.Millisecond
}
