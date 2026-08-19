package brain_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// The brain over a child thread (plan 35 slice 3): a child's turn runs on its
// own log with its own agent, its settlement moves its own row, and the
// outcome intercept fires at the session's quiescence, never on a child's
// end_turn alone (decision 15). The child row comes from the slice-3 test seam.

// childTurn plants a running child with one queued user.message on its own
// log and enqueues its turn — what the slice-4 spawn will commit.
func (h *harness) childTurn(t *testing.T, text string) domain.ID {
	t.Helper()
	child := pgtest.NewChildThread(t, h.pool, h.sessionID)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE session_threads SET status = 'running' WHERE id = $1`, child.String()); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
	if _, err := h.log.Append(context.Background(), h.sessionID, []events.NewEvent{
		{Type: domain.EventUserMessage, ThreadID: child, Payload: payload}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.queue.EnqueueThread(context.Background(), h.pool, h.envID, h.sessionID, child, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	return child
}

func (h *harness) threadStatus(t *testing.T, tid domain.ID) string {
	t.Helper()
	var s string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT status FROM session_threads WHERE id = $1`, tid.String()).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// A coordinator's end_turn while its worker still runs does not start the
// outcome evaluation, and neither does the worker's own end_turn start one
// on the worker; the session's quiescence — the last running thread idling —
// grades once, on the primary.
func TestOutcomeGradesAtSessionQuiescenceNotOnAChildsEndTurn(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		agentReply("delegated the work"),
		agentReply("worker done"),
		graderReply("all criteria met", "satisfied"),
	}, nil)
	outcomeID := h.wakeOutcome(t, "Build a DCF model", 3)
	child := h.childTurn(t, "build the model")

	// The primary's turn ends first: the child still runs, so the session
	// stays running and nothing is graded.
	h.runOnce(t)
	if got := h.status(t); got != "running" {
		t.Errorf("session after the primary's end_turn = %q, want running (the child still runs)", got)
	}
	if got := h.threadStatus(t, domain.PrimaryThreadID(h.sessionID)); got != "idle" {
		t.Errorf("primary thread after its end_turn = %q, want idle", got)
	}
	if n := len(h.eventsOfType(t, domain.EventSpanOutcomeEvalStart)); n != 0 {
		t.Fatalf("outcome evaluation started on the primary's end_turn under a running child: %d starts", n)
	}
	if n := h.countType(t, "session.status_idle"); n != 0 {
		t.Errorf("session.status_idle emitted while the child runs")
	}

	// The child's turn ends: quiescence. The child idles, the primary is
	// brought back running for the grading turn, the start fires once.
	h.runOnce(t)
	if got := h.threadStatus(t, child); got != "idle" {
		t.Errorf("child after its end_turn = %q, want idle", got)
	}
	if n := len(h.eventsOfType(t, domain.EventSpanOutcomeEvalStart)); n != 1 {
		t.Fatalf("outcome evaluation starts after quiescence = %d, want 1", n)
	}
	if got := h.status(t); got != "running" {
		t.Errorf("session during grading = %q, want running", got)
	}
	// The child's own view carries its message, its turn and its idle event
	// — no grading artefact: the start is the primary's.
	own, err := h.log.List(context.Background(), h.sessionID, events.ListQuery{Scope: events.ScopeThread, ThreadID: child})
	if err != nil {
		t.Fatal(err)
	}
	var ownTypes []string
	for _, ev := range own {
		ownTypes = append(ownTypes, string(ev.Type))
	}
	if !typesEqual(ownTypes, []string{"user.message", "span.model_request_start", "agent.message", "span.model_request_end",
		"session.thread_status_idle"}) {
		t.Errorf("child's own view = %v", ownTypes)
	}

	h.drain(t)
	if got := h.status(t); got != "idle" {
		t.Errorf("status after grading = %q, want idle", got)
	}
	evals := h.outcomes(t)
	if len(evals) != 1 || evals[0].OutcomeID != outcomeID || evals[0].Result != domain.OutcomeResultSatisfied {
		t.Fatalf("outcomes = %+v, want the one satisfied entry", evals)
	}
	if len(h.provider.calls) != 3 {
		t.Fatalf("provider calls = %d, want primary + child + grader", len(h.provider.calls))
	}
	// The child's request was assembled from the child's log, not the
	// primary's: its only message is the child's own.
	childCall := h.provider.calls[1]
	if len(childCall.Messages) != 1 || !strings.Contains(string(childCall.Messages[0].Content), "build the model") {
		t.Errorf("child's request messages = %+v, want the child's own message alone", childCall.Messages)
	}
}

// A child's turn suspending on tools: its ask-gated and client-executed calls
// are cross-posted — the humans and clients who must answer them read the
// session view — while an allow-policy built-in the platform runs itself
// stays on the child's own log (cloud; the self_hosted view rule is slice 4).
func TestChildTurnCrossPostsOnlyWhatAClientMustAnswer(t *testing.T) {
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{{
		provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
			ID: "toolu_bash", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}},
		provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
			ID: "toolu_lookup", Name: "lookup", Input: json.RawMessage(`{"q":"go"}`)}},
		done("tool_use", 3),
	}}, nil)
	pgtest.SetSessionStatus(t, h.pool, h.sessionID, "running")
	agent := `{"type":"agent","id":"agent_worker","version":1,"name":"worker","model":{"id":"fixture-model"},` +
		`"system":"","description":"","tools":[{"type":"agent_toolset_20260401"},` +
		`{"type":"custom","name":"lookup","description":"look things up","input_schema":{"type":"object"}}],` +
		`"mcp_servers":[],"skills":[]}`
	child := pgtest.NewChildThreadWithAgent(t, h.pool, h.sessionID, agent)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE session_threads SET status = 'running' WHERE id = $1`, child.String()); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"content": []map[string]string{{"type": "text", "text": "go"}}})
	if _, err := h.log.Append(context.Background(), h.sessionID, []events.NewEvent{
		{Type: domain.EventUserMessage, ThreadID: child, Payload: payload}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.queue.EnqueueThread(context.Background(), h.pool, h.envID, h.sessionID, child, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}

	h.runOnce(t)

	view, err := h.log.List(context.Background(), h.sessionID, events.ListQuery{Scope: events.ScopeSession})
	if err != nil {
		t.Fatal(err)
	}
	var viewTypes []string
	for _, ev := range view {
		viewTypes = append(viewTypes, string(ev.Type))
		if ev.Type == domain.EventAgentCustomToolUse && ev.ThreadID != child {
			t.Errorf("the child's custom tool use on the session view carries thread %q, want the child's", ev.ThreadID)
		}
	}
	if !typesEqual(viewTypes, []string{"agent.custom_tool_use"}) {
		t.Errorf("session view = %v, want the child's custom tool use alone (its bash call is the platform's to run)", viewTypes)
	}
	if got := h.threadStatus(t, child); got != "running" {
		t.Errorf("child = %q, want running (suspended on its tools)", got)
	}
	if n := h.liveOf(t, queue.ToolExec); n != 1 {
		t.Errorf("live tool_exec = %d, want the session's one", n)
	}
}
