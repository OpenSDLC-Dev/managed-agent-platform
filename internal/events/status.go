package events

import (
	"encoding/json"
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// Session status transitions are recorded as a pair (plan 35 decision 12):
// the primary thread's own session.thread_status_* first, the session's
// session.status_* second — the fact, then the rollup, the order the
// reference's own sequences show. Every session has a primary thread, so
// every session.status_* emission on every session goes through StatusChange;
// a history from before the thread resource existed holds no thread events
// (append-only; nothing backfills them).

// threadLifecycle is the set of events that name a thread and its agent.
var threadLifecycle = map[domain.EventType]bool{
	domain.EventSessionThreadCreated:           true,
	domain.EventSessionThreadStatusRunning:     true,
	domain.EventSessionThreadStatusIdle:        true,
	domain.EventSessionThreadStatusRescheduled: true,
	domain.EventSessionThreadStatusTerminated:  true,
}

// threadStatusOf maps a session status to the thread-status event that
// announces it. The primary thread never terminates (decision 12 — only
// _running is asserted for every session, and the webhooks page carves the
// primary's end out), so terminated has no thread event.
var threadStatusOf = map[domain.SessionStatus]domain.EventType{
	domain.SessionRunning:      domain.EventSessionThreadStatusRunning,
	domain.SessionIdle:         domain.EventSessionThreadStatusIdle,
	domain.SessionRescheduling: domain.EventSessionThreadStatusRescheduled,
}

var sessionStatusOf = map[domain.SessionStatus]domain.EventType{
	domain.SessionRunning:      domain.EventSessionStatusRunning,
	domain.SessionIdle:         domain.EventSessionStatusIdle,
	domain.SessionRescheduling: domain.EventSessionStatusRescheduled,
	domain.SessionTerminated:   domain.EventSessionStatusTerminated,
}

// StatusChange is the event pair recording that sessionID's primary thread —
// and so the session — moved to status: {session.thread_status_*,
// session.status_*}, stop the idle stop reason (nil otherwise). The thread
// event carries session_thread_id here and agent_name once appended
// (AppendInTx reads it under the session lock). The pair is a batch to
// append together; the caller decides whether the column moves
// (AppendOptions.SetStatus), exactly as before — an emission over an
// unchanged status (a re-idle with a new stop reason, a reclaim's
// rescheduled+running) is still a pair.
func StatusChange(sessionID domain.ID, status domain.SessionStatus, stop *domain.StopReason) []NewEvent {
	sessionType, ok := sessionStatusOf[status]
	if !ok {
		panic(fmt.Sprintf("events: no status event for session status %q", status))
	}
	session := map[string]any{}
	thread := map[string]any{"session_thread_id": domain.PrimaryThreadID(sessionID).String()}
	if stop != nil {
		session["stop_reason"] = stop
		thread["stop_reason"] = stop
	}
	out := make([]NewEvent, 0, 2)
	if threadType, ok := threadStatusOf[status]; ok {
		out = append(out, NewEvent{Type: threadType, Payload: mustMarshal(thread)})
	}
	return append(out, NewEvent{Type: sessionType, Payload: mustMarshal(session)})
}

// withAgentName sets agent_name on a thread lifecycle payload that lacks it.
func withAgentName(payload json.RawMessage, name string) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("thread event payload is not an object: %v", err)
	}
	if _, ok := obj["agent_name"]; ok {
		return payload, nil
	}
	obj["agent_name"] = mustMarshal(name)
	return json.Marshal(obj)
}

func mustMarshal(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err) // maps of strings and a StopReason cannot fail to marshal
	}
	return raw
}

// nullableID binds an optional id: NULL when empty.
func nullableID(id domain.ID) *string {
	if id == "" {
		return nil
	}
	s := id.String()
	return &s
}
