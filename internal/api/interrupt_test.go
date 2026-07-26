package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// --- user.interrupt: the escape hatch for a turn nothing will finish (#68) ---

func (s *tserver) environmentID(sessionID string) string {
	s.t.Helper()
	status, res := s.do(http.MethodGet, "/v1/sessions/"+sessionID, nil)
	if status != http.StatusOK {
		s.t.Fatalf("get session: %d %v", status, res)
	}
	return res["environment_id"].(string)
}

// stopReasonType reads the stop_reason.type off a session.status_idle event.
func stopReasonType(t *testing.T, ev map[string]any) string {
	t.Helper()
	stop, ok := ev["stop_reason"].(map[string]any)
	if !ok {
		t.Fatalf("session.status_idle has no stop_reason object: %v", ev)
	}
	return stop["type"].(string)
}

// suspendedOnTool drives a session to the state the escape hatch exists for: a
// turn suspended on a tool call, with the session running, the tool_exec item
// live, and nothing that will ever answer it. It returns the tool use's event id.
func suspendedOnTool(t *testing.T, s *tserver, sessionID string, typ domain.EventType) string {
	t.Helper()
	ctx := context.Background()
	q := queue.New(s.pool)

	sendEvents(t, s, sessionID, userMessage("run a tool"))
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim model_turn: %+v %v", item, err)
	}
	toolUseID := appendToolUse(t, s, sessionID, typ)
	if err := q.Complete(ctx, s.pool, item); err != nil {
		t.Fatal(err)
	}
	if typ == domain.EventAgentToolUse {
		if _, err := q.Enqueue(ctx, s.pool, domain.ID(s.environmentID(sessionID)), domain.ID(sessionID), queue.ToolExec); err != nil {
			t.Fatal(err)
		}
	}
	return toolUseID
}

func TestInterruptEndsATurnStuckOnAnUnansweredToolUse(t *testing.T) {
	// The issue's core case: a self_hosted session whose worker never returns
	// the result. Nothing on the platform can finish the turn, so the session
	// stays running forever until an interrupt settles it.
	s := newTestServer(t)
	sessionID := selfHostedSession(t, s)
	toolUseID := suspendedOnTool(t, s, sessionID, domain.EventAgentToolUse)

	if got := s.sessionStatus(sessionID); got != "running" {
		t.Fatalf("status before interrupt = %q, want running", got)
	}

	sendEvents(t, s, sessionID, map[string]any{"type": "user.interrupt"})

	if got := s.sessionStatus(sessionID); got != "idle" {
		t.Errorf("status after interrupt = %q, want idle", got)
	}
	want := []string{"user.message", "session.status_running", "agent.tool_use",
		"user.interrupt", "agent.tool_result", "session.status_idle"}
	if got := s.eventTypes(sessionID); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
	// The reference emits no interrupt-specific stop reason: an interrupted turn
	// ends on end_turn, exactly like one that finishes on its own.
	if got := stopReasonType(t, lastEventOfType(t, s, sessionID, "session.status_idle")); got != "end_turn" {
		t.Errorf("stop_reason.type = %q, want end_turn", got)
	}
	// The abandoned call is answered, or every later replay would carry a
	// tool_use the model protocol requires a result for.
	result := lastEventOfType(t, s, sessionID, "agent.tool_result")
	if result["tool_use_id"] != toolUseID {
		t.Errorf("agent.tool_result tool_use_id = %v, want %v", result["tool_use_id"], toolUseID)
	}
	if result["is_error"] != true {
		t.Errorf("agent.tool_result is_error = %v, want true", result["is_error"])
	}
	if result["processed_at"] == nil {
		t.Errorf("platform-emitted agent.tool_result has a null processed_at")
	}
	// Nothing may still be working the interrupted turn.
	if n := s.liveWork(sessionID, queue.ToolExec); n != 0 {
		t.Errorf("live tool_exec items after interrupt = %d, want 0", n)
	}

	// Resumable: the whole point. A user.message now wakes the session, which
	// the unanswered-tool gate refused before the interrupt.
	sendEvents(t, s, sessionID, userMessage("never mind, do something else"))
	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status after the follow-up message = %q, want running", got)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn items = %d, want 1", n)
	}
}

func TestInterruptAnswersEachToolFamilyInItsOwnShape(t *testing.T) {
	// A result event's family must match its tool use's: a custom tool is
	// answered by user.custom_tool_result keyed by custom_tool_use_id, a
	// platform built-in by agent.tool_result keyed by tool_use_id.
	s := newTestServer(t)
	sessionID := selfHostedSession(t, s)
	ctx := context.Background()
	q := queue.New(s.pool)

	sendEvents(t, s, sessionID, userMessage("do two things"))
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}
	toolID := appendToolUse(t, s, sessionID, domain.EventAgentToolUse)
	customID := appendToolUse(t, s, sessionID, domain.EventAgentCustomToolUse)
	if err := q.Complete(ctx, s.pool, item); err != nil {
		t.Fatal(err)
	}

	sendEvents(t, s, sessionID, map[string]any{"type": "user.interrupt"})

	agentResult := lastEventOfType(t, s, sessionID, "agent.tool_result")
	if agentResult["tool_use_id"] != toolID {
		t.Errorf("agent.tool_result tool_use_id = %v, want %v", agentResult["tool_use_id"], toolID)
	}
	customResult := lastEventOfType(t, s, sessionID, "user.custom_tool_result")
	if customResult["custom_tool_use_id"] != customID {
		t.Errorf("user.custom_tool_result custom_tool_use_id = %v, want %v",
			customResult["custom_tool_use_id"], customID)
	}
	if customResult["is_error"] != true {
		t.Errorf("user.custom_tool_result is_error = %v, want true", customResult["is_error"])
	}
	// The platform wrote this answer, so it is processed on arrival — not left
	// looking like a client event still queued behind earlier ones.
	if customResult["processed_at"] == nil {
		t.Errorf("synthesized user.custom_tool_result has a null processed_at")
	}

	// Both calls are answered, so the client's own late result is refused
	// rather than double-answering the call on the append-only log.
	status, _ := s.do(http.MethodPost, "/v1/sessions/"+sessionID+"/events", map[string]any{"events": []any{
		map[string]any{"type": "user.custom_tool_result", "custom_tool_use_id": customID,
			"content": []any{map[string]any{"type": "text", "text": "late"}}},
	}})
	if status != http.StatusBadRequest {
		t.Errorf("late custom tool result: status %d, want 400", status)
	}
}

func TestInterruptAndRedirectInOneBatch(t *testing.T) {
	// The documented way to steer a running agent: one send carrying the
	// interrupt and the new instruction. The interrupted turn still ends on its
	// own session.status_idle before the redirect starts the next one.
	s := newTestServer(t)
	sessionID := selfHostedSession(t, s)
	suspendedOnTool(t, s, sessionID, domain.EventAgentToolUse)

	echo := sendEvents(t, s, sessionID,
		map[string]any{"type": "user.interrupt"},
		userMessage("Instead, focus on fixing the bug in line 42."))
	// The response echoes the posted events only, never the platform reaction.
	if len(echo) != 2 || echo[0]["type"] != "user.interrupt" || echo[1]["type"] != "user.message" {
		t.Fatalf("echo = %v", echo)
	}

	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status after interrupt+redirect = %q, want running", got)
	}
	want := []string{"user.message", "session.status_running", "agent.tool_use",
		"user.interrupt", "user.message", "agent.tool_result",
		"session.status_idle", "session.status_running"}
	if got := s.eventTypes(sessionID); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
	if got := stopReasonType(t, lastEventOfType(t, s, sessionID, "session.status_idle")); got != "end_turn" {
		t.Errorf("stop_reason.type = %q, want end_turn", got)
	}
	// The cancelled turn's slot is free, so the redirect gets its own item.
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn items = %d, want 1", n)
	}
	if n := s.liveWork(sessionID, queue.ToolExec); n != 0 {
		t.Errorf("live tool_exec items = %d, want 0", n)
	}
}

func TestInterruptWithNothingToStopOnlyAppends(t *testing.T) {
	// An idle session with no outstanding call has no turn to end. The event is
	// still accepted and logged — it just settles nothing, so no phantom
	// session.status_idle lands on a session that never left idle.
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)

	sendEvents(t, s, sessionID, map[string]any{"type": "user.interrupt"})

	if got := s.sessionStatus(sessionID); got != "idle" {
		t.Errorf("status = %q, want idle", got)
	}
	if got := s.eventTypes(sessionID); !sameStrings(got, []string{"user.interrupt"}) {
		t.Errorf("event log = %v, want [user.interrupt]", got)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 0 {
		t.Errorf("live model_turn items = %d, want 0", n)
	}

	// It also does not block the next message from starting a turn normally.
	sendEvents(t, s, sessionID, userMessage("hello"))
	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status after message = %q, want running", got)
	}
}

func TestInterruptReleasesARequiresActionGate(t *testing.T) {
	// A confirmation that never comes is the same dead end as a result that
	// never comes: the session idles on requires_action and only a
	// user.tool_confirmation can move it. The interrupt abandons the gated call
	// so the session is resumable again.
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	askID := appendToolUseWithPerm(t, s, sessionID, "risky", "ask")

	sendEvents(t, s, sessionID, map[string]any{"type": "user.interrupt"})

	if got := stopReasonType(t, lastEventOfType(t, s, sessionID, "session.status_idle")); got != "end_turn" {
		t.Errorf("stop_reason.type = %q, want end_turn", got)
	}
	if got := lastEventOfType(t, s, sessionID, "agent.tool_result")["tool_use_id"]; got != askID {
		t.Errorf("agent.tool_result tool_use_id = %v, want %v", got, askID)
	}

	// The abandoned call is answered, so a confirmation for it can no longer
	// land: allowing one would let a denial write a second result for the same
	// call onto the append-only log.
	status, _ := s.do(http.MethodPost, "/v1/sessions/"+sessionID+"/events", map[string]any{"events": []any{
		map[string]any{"type": "user.tool_confirmation", "result": "deny", "tool_use_id": askID},
	}})
	if status != http.StatusBadRequest {
		t.Errorf("confirmation for an interrupted call: status %d, want 400", status)
	}

	// And the gate no longer holds the session: a message resumes it.
	sendEvents(t, s, sessionID, userMessage("skip that, do this"))
	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status after the follow-up message = %q, want running", got)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn items = %d, want 1", n)
	}
}

func TestInterruptCancelsALiveModelTurn(t *testing.T) {
	// A session running a model turn with no tool call outstanding still has a
	// turn to stop: the item is cancelled so the brain that holds it can commit
	// nothing, and the session settles idle.
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	ctx := context.Background()
	q := queue.New(s.pool)

	sendEvents(t, s, sessionID, userMessage("long job"))
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim: %+v %v", item, err)
	}

	sendEvents(t, s, sessionID, map[string]any{"type": "user.interrupt"})

	if got := s.sessionStatus(sessionID); got != "idle" {
		t.Errorf("status after interrupt = %q, want idle", got)
	}
	if got := stopReasonType(t, lastEventOfType(t, s, sessionID, "session.status_idle")); got != "end_turn" {
		t.Errorf("stop_reason.type = %q, want end_turn", got)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 0 {
		t.Errorf("live model_turn items after interrupt = %d, want 0", n)
	}
	// The claimant's lease proof is gone with the item, so its settlement can
	// no longer commit the turn it was running.
	if err := q.Complete(ctx, s.pool, item); err == nil {
		t.Error("completing a cancelled item succeeded, want a lost-lease error")
	}
}

// sameStrings reports whether two string slices are equal element by element.
func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
