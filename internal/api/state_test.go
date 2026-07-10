package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// --- session state machine triggers on POST /events ---

func (s *tserver) sessionStatus(sessionID string) string {
	s.t.Helper()
	status, res := s.do(http.MethodGet, "/v1/sessions/"+sessionID, nil)
	if status != http.StatusOK {
		s.t.Fatalf("get session: %d %v", status, res)
	}
	return res["status"].(string)
}

func (s *tserver) eventTypes(sessionID string) []string {
	s.t.Helper()
	status, res := s.do(http.MethodGet, "/v1/sessions/"+sessionID+"/events", nil)
	if status != http.StatusOK {
		s.t.Fatalf("list events: %d %v", status, res)
	}
	var types []string
	for _, ev := range listData(s.t, res) {
		types = append(types, ev["type"].(string))
	}
	return types
}

func (s *tserver) liveWork(sessionID string, kind queue.Kind) int {
	s.t.Helper()
	var n int
	err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM work_items
		 WHERE session_id = $1 AND kind = $2 AND state IN ('queued','starting','active')`,
		sessionID, string(kind)).Scan(&n)
	if err != nil {
		s.t.Fatalf("count work: %v", err)
	}
	return n
}

func TestUserMessageFlipsIdleToRunningAndEnqueues(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)

	if got := s.sessionStatus(sessionID); got != "idle" {
		t.Fatalf("new session status = %q", got)
	}

	echoed := sendEvents(t, s, sessionID, userMessage("hi"))
	// The response echoes only the posted events, not the platform reaction.
	if len(echoed) != 1 || echoed[0]["type"] != "user.message" {
		t.Errorf("echo = %v", echoed)
	}

	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status after user.message = %q, want running", got)
	}
	types := s.eventTypes(sessionID)
	if len(types) != 2 || types[0] != "user.message" || types[1] != "session.status_running" {
		t.Errorf("event log = %v, want [user.message session.status_running]", types)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn items = %d, want 1", n)
	}
}

func TestUserMessageWhileRunningOnlyAppends(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)

	sendEvents(t, s, sessionID, userMessage("first"))
	sendEvents(t, s, sessionID, userMessage("second"))

	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status = %q", got)
	}
	var running int
	for _, typ := range s.eventTypes(sessionID) {
		if typ == "session.status_running" {
			running++
		}
	}
	if running != 1 {
		t.Errorf("session.status_running emitted %d times, want once per idle→running flip", running)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn items = %d, want 1 (idempotent)", n)
	}
}

func TestToolResultWhileRunningEnqueuesNextTurn(t *testing.T) {
	s := newTestServer(t)
	sessionID := selfHostedSession(t, s)
	ctx := context.Background()
	q := queue.New(s.pool)

	sendEvents(t, s, sessionID, userMessage("run a tool"))

	// The brain's turn happened: claim and finish the queued model_turn.
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}
	if err := q.Complete(ctx, s.pool, item); err != nil {
		t.Fatal(err)
	}

	// A self-hosted worker posts the tool result → the next turn is queued,
	// with no extra status_running (the session never left running).
	sendEvents(t, s, sessionID, map[string]any{
		"type": "user.tool_result", "tool_use_id": "sevt_00000000000000000000000001",
		"content": []any{map[string]any{"type": "text", "text": "ok"}},
	})
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn after tool result = %d, want 1", n)
	}
	var running int
	for _, typ := range s.eventTypes(sessionID) {
		if typ == "session.status_running" {
			running++
		}
	}
	if running != 1 {
		t.Errorf("session.status_running count = %d, want 1", running)
	}

	// A tool result on an idle session is appended but schedules nothing.
	item, err = q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil {
		t.Fatal(err)
	}
	if err := q.Complete(ctx, s.pool, item); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE sessions SET status = 'idle' WHERE id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	sendEvents(t, s, sessionID, map[string]any{
		"type": "user.tool_result", "tool_use_id": "sevt_00000000000000000000000002",
	})
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 0 {
		t.Errorf("tool result on idle session enqueued %d turns, want 0", n)
	}
}

// --- session.updated emission from PATCH ---

func TestUpdateSessionEmitsOnlyChangedFields(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)

	status, _ := s.do(http.MethodPost, "/v1/sessions/"+sessionID, map[string]any{"title": "new title"})
	if status != http.StatusOK {
		t.Fatalf("update: %d", status)
	}

	st, res := s.do(http.MethodGet, "/v1/sessions/"+sessionID+"/events", nil)
	if st != http.StatusOK {
		t.Fatal(st)
	}
	evs := listData(t, res)
	if len(evs) != 1 || evs[0]["type"] != "session.updated" {
		t.Fatalf("events after title update = %v", evs)
	}
	wantExactKeys(t, evs[0], "id", "type", "processed_at", "title")
	if evs[0]["title"] != "new title" {
		t.Errorf("title = %v", evs[0]["title"])
	}

	// Metadata update carries metadata, not title.
	status, _ = s.do(http.MethodPost, "/v1/sessions/"+sessionID, map[string]any{"metadata": map[string]any{"k": "v"}})
	if status != http.StatusOK {
		t.Fatalf("metadata update: %d", status)
	}
	_, res = s.do(http.MethodGet, "/v1/sessions/"+sessionID+"/events", nil)
	evs = listData(t, res)
	last := evs[len(evs)-1]
	if last["type"] != "session.updated" {
		t.Fatalf("last event = %v", last)
	}
	wantExactKeys(t, last, "id", "type", "processed_at", "metadata")

	// An agent-config update snapshots the resolved agent.
	status, _ = s.do(http.MethodPost, "/v1/sessions/"+sessionID, map[string]any{
		"agent": map[string]any{"tools": []any{map[string]any{"type": "agent_toolset_20260401"}}},
	})
	if status != http.StatusOK {
		t.Fatalf("agent update: %d", status)
	}
	_, res = s.do(http.MethodGet, "/v1/sessions/"+sessionID+"/events", nil)
	evs = listData(t, res)
	last = evs[len(evs)-1]
	wantExactKeys(t, last, "id", "type", "processed_at", "agent")
	agent := last["agent"].(map[string]any)
	if agent["type"] != "agent" || agent["tools"] == nil {
		t.Errorf("agent snapshot = %v", agent)
	}

	// A no-op update emits nothing.
	before := len(s.eventTypes(sessionID))
	status, _ = s.do(http.MethodPost, "/v1/sessions/"+sessionID, map[string]any{"title": "new title"})
	if status != http.StatusOK {
		t.Fatalf("noop update: %d", status)
	}
	if after := len(s.eventTypes(sessionID)); after != before {
		t.Errorf("no-op update emitted an event (%d -> %d)", before, after)
	}
}
