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

// primaryStatusEvents are the thread events AppendInTx completes with the
// session's agent name when they carry no thread (the primary's own status
// changes). session.thread_created is deliberately not here: it is written
// to the parent's stream naming the *child's* agent, so its emitter supplies
// agent_name itself.
var primaryStatusEvents = map[domain.EventType]bool{
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
		out = append(out, NewEvent{Type: threadType, Payload: mustJSON(thread)})
	}
	return append(out, NewEvent{Type: sessionType, Payload: mustJSON(session)})
}

// withAgentName sets agent_name on a thread status payload that lacks it.
func withAgentName(payload json.RawMessage, name string) (json.RawMessage, error) {
	obj, err := asObject(payload, "thread event payload")
	if err != nil {
		return nil, err
	}
	if _, ok := obj["agent_name"]; ok {
		return payload, nil
	}
	obj["agent_name"] = mustJSON(name)
	return json.Marshal(obj)
}

// nullableID binds an optional id: NULL when empty.
func nullableID(id domain.ID) *string {
	if id == "" {
		return nil
	}
	s := id.String()
	return &s
}
