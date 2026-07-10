package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
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

// appendToolUse plants a platform-emitted tool intent directly on the log
// (what the brain's settlement commits) and returns its event id — the id a
// client's tool result must reference.
func appendToolUse(t *testing.T, s *tserver, sessionID string, typ domain.EventType) string {
	t.Helper()
	evs, err := events.NewLog(s.pool).Append(context.Background(), domain.ID(sessionID),
		[]events.NewEvent{{Type: typ, Payload: []byte(`{"name":"lookup","input":{},"session_thread_id":null}`)}})
	if err != nil {
		t.Fatal(err)
	}
	return evs[0].ID.String()
}

func TestToolResultWhileRunningEnqueuesNextTurn(t *testing.T) {
	s := newTestServer(t)
	sessionID := selfHostedSession(t, s)
	ctx := context.Background()
	q := queue.New(s.pool)

	sendEvents(t, s, sessionID, userMessage("run a tool"))

	// The brain's turn happened: it emitted a tool intent, then claim and
	// finish the queued model_turn.
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}
	toolUseID := appendToolUse(t, s, sessionID, domain.EventAgentToolUse)
	if err := q.Complete(ctx, s.pool, item); err != nil {
		t.Fatal(err)
	}

	// A self-hosted worker posts the tool result → the next turn is queued,
	// with no extra status_running (the session never left running).
	sendEvents(t, s, sessionID, map[string]any{
		"type": "user.tool_result", "tool_use_id": toolUseID,
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
	toolUseID2 := appendToolUse(t, s, sessionID, domain.EventAgentToolUse)
	if err := q.Complete(ctx, s.pool, item); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE sessions SET status = 'idle' WHERE id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	sendEvents(t, s, sessionID, map[string]any{
		"type": "user.tool_result", "tool_use_id": toolUseID2,
	})
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 0 {
		t.Errorf("tool result on idle session enqueued %d turns, want 0", n)
	}
}

func TestParallelToolResultsResumeOnFullSet(t *testing.T) {
	// The reference protocol requires every tool_use answered before the
	// conversation continues, so the resume trigger must not fire until the
	// batch completes the set.
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	ctx := context.Background()
	q := queue.New(s.pool)

	sendEvents(t, s, sessionID, userMessage("do two things"))
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}
	idA := appendToolUse(t, s, sessionID, domain.EventAgentCustomToolUse)
	idB := appendToolUse(t, s, sessionID, domain.EventAgentCustomToolUse)
	if err := q.Complete(ctx, s.pool, item); err != nil {
		t.Fatal(err)
	}

	sendEvents(t, s, sessionID, map[string]any{
		"type": "user.custom_tool_result", "custom_tool_use_id": idA,
		"content": []any{map[string]any{"type": "text", "text": "one"}},
	})
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 0 {
		t.Errorf("partial result set scheduled %d turns, want 0", n)
	}

	sendEvents(t, s, sessionID, map[string]any{
		"type": "user.custom_tool_result", "custom_tool_use_id": idB,
		"content": []any{map[string]any{"type": "text", "text": "two"}},
	})
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 1 {
		t.Errorf("completing result scheduled %d turns, want 1", n)
	}
}

func TestInboundToolResultValidation(t *testing.T) {
	// The log is append-only: a result with a wrong, unknown, or duplicate
	// reference would poison every future replay, so it must be the
	// client's 400 instead.
	s := newTestServer(t)
	sessionID := selfHostedSession(t, s)

	sendEvents(t, s, sessionID, userMessage("hi"))
	customID := appendToolUse(t, s, sessionID, domain.EventAgentCustomToolUse)

	post := func(evs ...map[string]any) int {
		status, _ := s.do(http.MethodPost, "/v1/sessions/"+sessionID+"/events", map[string]any{"events": evs})
		return status
	}
	result := func(id string) map[string]any {
		return map[string]any{
			"type": "user.custom_tool_result", "custom_tool_use_id": id,
			"content": []any{map[string]any{"type": "text", "text": "ok"}},
		}
	}

	// Unknown reference.
	if got := post(result("sevt_00000000000000000000000000")); got != http.StatusBadRequest {
		t.Errorf("unknown tool_use ref: status %d, want 400", got)
	}
	// Kind mismatch: a user.tool_result cannot answer a custom tool call.
	if got := post(map[string]any{"type": "user.tool_result", "tool_use_id": customID}); got != http.StatusBadRequest {
		t.Errorf("kind mismatch: status %d, want 400", got)
	}
	// Duplicate within one request.
	if got := post(result(customID), result(customID)); got != http.StatusBadRequest {
		t.Errorf("intra-batch duplicate: status %d, want 400", got)
	}
	// The valid result lands…
	if got := post(result(customID)); got != http.StatusOK {
		t.Errorf("valid result: status %d, want 200", got)
	}
	// …and a second answer for the same call is rejected.
	if got := post(result(customID)); got != http.StatusBadRequest {
		t.Errorf("already answered: status %d, want 400", got)
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

	// No-op updates emit nothing — including metadata and agent retries,
	// where the stored jsonb never byte-matches a fresh marshal and only a
	// semantic comparison can tell nothing changed.
	before := len(s.eventTypes(sessionID))
	for _, patch := range []map[string]any{
		{"title": "new title"},
		{"metadata": map[string]any{"k": "v"}},
		{"agent": map[string]any{"tools": []any{map[string]any{"type": "agent_toolset_20260401"}}}},
	} {
		status, _ = s.do(http.MethodPost, "/v1/sessions/"+sessionID, patch)
		if status != http.StatusOK {
			t.Fatalf("noop update %v: %d", patch, status)
		}
	}
	if after := len(s.eventTypes(sessionID)); after != before {
		t.Errorf("no-op updates emitted events (%d -> %d)", before, after)
	}

	// A mixed patch (title changed, metadata identical) carries only the
	// field that actually changed.
	status, _ = s.do(http.MethodPost, "/v1/sessions/"+sessionID, map[string]any{
		"title": "final title", "metadata": map[string]any{"k": "v"},
	})
	if status != http.StatusOK {
		t.Fatalf("mixed update: %d", status)
	}
	_, res = s.do(http.MethodGet, "/v1/sessions/"+sessionID+"/events", nil)
	evs = listData(t, res)
	last = evs[len(evs)-1]
	wantExactKeys(t, last, "id", "type", "processed_at", "title")
}

func TestUpdateSessionComparesNumbersByValue(t *testing.T) {
	// Change detection sees the client's literal on one side and Postgres's
	// jsonb normalization on the other, so numbers must be compared as
	// values: 1e2 IS 100 (no phantom event), while two integers that differ
	// past 2^53 are distinct (float64 would collapse them).
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)

	toolWithMax := func(max json.Number) map[string]any {
		return map[string]any{"agent": map[string]any{"tools": []any{map[string]any{
			"type": "custom", "name": "t", "description": "d",
			"input_schema": map[string]any{"type": "object", "maximum": max},
		}}}}
	}
	patch := func(max json.Number) {
		t.Helper()
		if status, res := s.do(http.MethodPost, "/v1/sessions/"+sessionID, toolWithMax(max)); status != http.StatusOK {
			t.Fatalf("agent update %s: %d %v", max, status, res)
		}
	}

	patch("9007199254740992") // 2^53
	before := len(s.eventTypes(sessionID))

	patch("9007199254740993") // 2^53 + 1: a real change
	if after := len(s.eventTypes(sessionID)); after != before+1 {
		t.Fatalf("a change past 2^53 emitted no session.updated (%d -> %d)", before, after)
	}
	before++

	// Re-sending the same value in exponent form changes nothing: Postgres
	// stored 100, the client's raw bytes still say 1e2.
	patch("100")
	before++
	patch("1e2")
	if after := len(s.eventTypes(sessionID)); after != before {
		t.Errorf("1e2 vs stored 100 emitted a phantom session.updated (%d -> %d)", before, after)
	}
}
