package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/jackc/pgx/v5"
)

// Session events: POST (send, batch), GET (list, cursor-paged), and the SSE
// stream. Wire shapes follow the reference SDK exactly — see the events
// package for the inbound contract and STATE.md for the documented v1
// divergences.

// sendSessionEvents implements POST /v1/sessions/{id}/events. The body is
// always a batch ({"events":[…]}); the response echoes the persisted events
// as {"data":[…]} with server-assigned ids.
func (s *server) sendSessionEvents(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))

	body, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(body, "events"); err != nil {
		return nil, err
	}
	rawEvents, err := rawList(body["events"], "events")
	if err != nil {
		return nil, err
	}

	// user.tool_result is only valid on self_hosted environments, so the
	// batch is validated against the session's environment kind.
	var envKind string
	err = s.pool.QueryRow(ctx,
		`SELECT e.kind FROM sessions s JOIN environments e ON e.id = s.environment_id WHERE s.id = $1`,
		id).Scan(&envKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("session %s not found", id)
	}
	if err != nil {
		return nil, err
	}

	newEvents, err := events.NormalizeInbound(envKind, rawEvents)
	if err != nil {
		return nil, errInvalid("%s", err)
	}

	appended, err := s.log.Append(ctx, domain.ID(id), newEvents)
	switch {
	case errors.Is(err, events.ErrSessionNotFound):
		return nil, errNotFound("session %s not found", id)
	case errors.Is(err, events.ErrSessionArchived):
		return nil, errInvalid("session %s is archived and read-only", id)
	case err != nil:
		return nil, err
	}

	data := make([]any, 0, len(appended))
	for _, ev := range appended {
		wire, err := eventWire(ev)
		if err != nil {
			return nil, err
		}
		data = append(data, wire)
	}
	return map[string]any{"data": data}, nil
}

// listSessionEvents implements GET /v1/sessions/{id}/events with the
// PageCursor envelope {"data":[…],"next_page":…} (no prev_page on events).
func (s *server) listSessionEvents(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	q := r.URL.Query()

	page, err := parsePage(q)
	if err != nil {
		return nil, err
	}
	query := events.ListQuery{Limit: page.limit + 1}
	if page.cur != nil {
		if !page.cur.versioned {
			return nil, errInvalid("invalid page cursor")
		}
		query.AfterSeq = &page.cur.version
	}
	switch q.Get("order") {
	case "", "asc":
	case "desc":
		query.Desc = true
	default:
		return nil, errInvalid(`order must be "asc" or "desc"`)
	}
	query.Types = listParam(q, "types")
	for key, dst := range map[string]**time.Time{
		"created_at[gt]": &query.CreatedGT, "created_at[gte]": &query.CreatedGTE,
		"created_at[lt]": &query.CreatedLT, "created_at[lte]": &query.CreatedLTE,
	} {
		t, err := parseTimeParam(q, key)
		if err != nil {
			return nil, err
		}
		*dst = t
	}

	if err := s.sessionExists(ctx, id); err != nil {
		return nil, err
	}

	evs, err := s.log.List(ctx, domain.ID(id), query)
	if err != nil {
		return nil, err
	}
	more := len(evs) > page.limit
	if more {
		evs = evs[:page.limit]
	}
	data := make([]any, 0, len(evs))
	for _, ev := range evs {
		wire, err := eventWire(ev)
		if err != nil {
			return nil, err
		}
		data = append(data, wire)
	}
	var next *string
	if more {
		c := encodeVersionCursor(evs[len(evs)-1].Seq)
		next = &c
	}
	return pageJSON{Data: data, NextPage: next}, nil
}

// streamSessionEvents implements GET /v1/sessions/{id}/events/stream: a live
// SSE tail of the session's log from connect time (reconnecting clients seed
// history through the list endpoint — the wire has no stream cursor).
// Frames are `event: <type>` + `data: <json>`; the reference client drops
// frames without a recognized event name, so the name always mirrors the
// payload's type. Previews (event_start/event_delta) are only sent for the
// types opted into via ?event_deltas[].
func (s *server) streamSessionEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	q := r.URL.Query()

	previews := make(map[string]bool)
	for _, v := range listParam(q, "event_deltas") {
		if !events.Previewable(domain.EventType(v)) {
			writeError(w, r, errInvalid(`event_deltas values must be "agent.message" or "agent.thinking"`))
			return
		}
		previews[v] = true
	}
	if err := s.sessionExists(ctx, id); err != nil {
		writeError(w, r, err)
		return
	}

	sub := s.broker.Subscribe(domain.ID(id))
	defer sub.Close()
	if err := s.broker.Ready(ctx); err != nil {
		writeError(w, r, err)
		return
	}
	// Snapshot the tail position after LISTEN coverage is active: anything
	// committed later is guaranteed a wake, so nothing can fall in between.
	var lastSeq int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM events WHERE session_id = $1`, id).Scan(&lastSeq); err != nil {
		writeError(w, r, err)
		return
	}

	h := w.Header()
	h.Set("content-type", "text/event-stream; charset=utf-8")
	h.Set("cache-control", "no-cache")
	h.Del("content-length")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()

	// event_delta frames carry only event_id, so remember each previewed
	// event's type from its event_start until the buffered event lands.
	started := make(map[string]string)
	ping := time.NewTicker(ssePingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-sub.Wake():
			evs, err := s.log.List(ctx, domain.ID(id), events.ListQuery{AfterSeq: &lastSeq})
			if err != nil {
				return
			}
			for _, ev := range evs {
				wire, err := eventWire(ev)
				if err != nil {
					return
				}
				writeSSEFrame(w, string(ev.Type), wire)
				lastSeq = ev.Seq
				delete(started, ev.ID.String())
			}
			flusher.Flush()

		case raw := <-sub.Frames():
			var frame struct {
				Type    string `json:"type"`
				EventID string `json:"event_id"`
				Event   struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"event"`
			}
			if json.Unmarshal(raw, &frame) != nil {
				continue
			}
			switch frame.Type {
			case "event_start":
				started[frame.Event.ID] = frame.Event.Type
				if !previews[frame.Event.Type] {
					continue
				}
			case "event_delta":
				if !previews[started[frame.EventID]] {
					continue
				}
			}
			writeSSEFrame(w, frame.Type, raw)
			flusher.Flush()
			// The deleted session's row is gone; nothing further can arrive.
			if frame.Type == "session.deleted" {
				return
			}

		case <-ping.C:
			writeSSEFrame(w, "ping", []byte(`{"type":"ping"}`))
			flusher.Flush()
		}
	}
}

// ssePingInterval keeps idle streams alive through proxies. The reference
// client skips ping frames wholesale.
var ssePingInterval = 15 * time.Second

// writeSSEFrame emits one server-sent event. The event name is required: the
// reference decoder dispatches on it and silently drops unnamed frames.
func writeSSEFrame(w io.Writer, name string, data []byte) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
}

// eventWire renders a stored event onto the wire: the type-specific payload
// fields merged with the id/type/processed_at envelope. Payload bytes pass
// through untouched, so content blocks round-trip exactly.
func eventWire(ev domain.Event) (json.RawMessage, error) {
	var out map[string]json.RawMessage
	if err := json.Unmarshal(ev.Body, &out); err != nil {
		return nil, fmt.Errorf("event %s payload is corrupt: %w", ev.ID, err)
	}
	if out == nil {
		out = make(map[string]json.RawMessage)
	}
	idRaw, err := json.Marshal(ev.ID.String())
	if err != nil {
		return nil, err
	}
	typeRaw, err := json.Marshal(string(ev.Type))
	if err != nil {
		return nil, err
	}
	processedAt := json.RawMessage("null")
	if ev.ProcessedAt != nil {
		processedAt, err = json.Marshal(ev.ProcessedAt.UTC())
		if err != nil {
			return nil, err
		}
	}
	out["id"], out["type"], out["processed_at"] = idRaw, typeRaw, processedAt
	return json.Marshal(out)
}

// sessionExists resolves list/stream 404s (session_ already normalized).
func (s *server) sessionExists(ctx context.Context, id string) error {
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM sessions WHERE id = $1`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound("session %s not found", id)
	}
	return err
}

// listParam collects a repeatable array query parameter in both wire
// spellings, key[]=v (bracket serialization) and key=v.
func listParam(q url.Values, key string) []string {
	var out []string
	out = append(out, q[key+"[]"]...)
	out = append(out, q[key]...)
	return out
}
