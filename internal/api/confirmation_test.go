package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// --- user.tool_confirmation state machine (requires_action round-trip) ---

// appendToolUseWithPerm plants a platform tool intent carrying an
// evaluated_permission (what the brain's suspend commits) and forces the
// session idle, mimicking a requires_action suspension. It returns the
// tool_use event id a confirmation must reference.
func appendToolUseWithPerm(t *testing.T, s *tserver, sessionID, name, perm string) string {
	t.Helper()
	return appendGatedToolUse(t, s, sessionID, domain.EventAgentToolUse,
		`{"name":"`+name+`","input":{},"evaluated_permission":"`+perm+`","session_thread_id":null}`)
}

// appendGatedToolUse plants one tool intent with the payload the brain would
// commit for it and forces the session idle. Both gated families use it: they
// differ in the event type and in one payload field, not in what the gate does
// with them, which is the point these tests exist to check.
func appendGatedToolUse(t *testing.T, s *tserver, sessionID string, typ domain.EventType, payload string) string {
	t.Helper()
	evs, err := events.NewLog(s.pool).Append(context.Background(), domain.ID(sessionID),
		[]events.NewEvent{{Type: typ, Payload: []byte(payload)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE sessions SET status = 'idle' WHERE id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	return evs[0].ID.String()
}

func appendAskToolUse(t *testing.T, s *tserver, sessionID, name string) string {
	t.Helper()
	return appendToolUseWithPerm(t, s, sessionID, name, "ask")
}

// appendAskMCPToolUse plants the MCP twin: the same gate, one field wider. The
// brain does not commit these yet — MCP tools reach no model until the catalog
// is offered at request assembly — so the shape is planted straight onto the log
// exactly as the reference declares it (name, mcp_server_name, input).
func appendAskMCPToolUse(t *testing.T, s *tserver, sessionID, server, name string) string {
	t.Helper()
	return appendGatedToolUse(t, s, sessionID, domain.EventAgentMCPToolUse,
		`{"name":"`+name+`","mcp_server_name":"`+server+
			`","input":{},"evaluated_permission":"ask","session_thread_id":null}`)
}

func confirm(id, result string, extra map[string]any) map[string]any {
	m := map[string]any{"type": "user.tool_confirmation", "result": result, "tool_use_id": id}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func lastEventOfType(t *testing.T, s *tserver, sessionID, typ string) map[string]any {
	t.Helper()
	_, res := s.do(http.MethodGet, "/v1/sessions/"+sessionID+"/events", nil)
	var last map[string]any
	for _, ev := range listData(t, res) {
		if ev["type"] == typ {
			last = ev
		}
	}
	if last == nil {
		t.Fatalf("no %s event on the log", typ)
	}
	return last
}

func countEventType(t *testing.T, s *tserver, sessionID, typ string) int {
	t.Helper()
	n := 0
	for _, got := range s.eventTypes(sessionID) {
		if got == typ {
			n++
		}
	}
	return n
}

func TestConfirmationAllowResumesWithToolExec(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	askID := appendAskToolUse(t, s, sessionID, "bash")

	sendEvents(t, s, sessionID, confirm(askID, "allow", nil))

	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status after allow = %q, want running", got)
	}
	// The allowed tool still has to run, so an executor is scheduled.
	if n := s.liveWork(sessionID, queue.ToolExec); n != 1 {
		t.Errorf("live tool_exec = %d, want 1", n)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 0 {
		t.Errorf("live model_turn = %d, want 0", n)
	}
	if n := countEventType(t, s, sessionID, "session.status_running"); n != 1 {
		t.Errorf("session.status_running count = %d, want 1", n)
	}
}

// A confirmed web tool resumes with web_exec — and a mixed confirmed set
// (web + sandbox) resumes with web_exec ONLY, the same web-first choice the
// brain's settlement makes: a tool_exec is visible to a BYOC worker, whose
// official toolset implements only the six sandbox tools, so it must not
// exist while a web call is unanswered. The executor's web driver answers
// the web calls and chains the tool_exec itself.
func TestConfirmationAllowWebToolResumesWithWebExec(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	webAsk := appendAskToolUse(t, s, sessionID, "web_search")
	bashAsk := appendAskToolUse(t, s, sessionID, "bash")

	sendEvents(t, s, sessionID,
		confirm(webAsk, "allow", nil),
		confirm(bashAsk, "allow", nil))

	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status after allow = %q, want running", got)
	}
	if n := s.liveWork(sessionID, queue.WebExec); n != 1 {
		t.Errorf("live web_exec = %d, want 1", n)
	}
	if n := s.liveWork(sessionID, queue.ToolExec); n != 0 {
		t.Errorf("live tool_exec = %d, want 0 — the web driver chains it", n)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 0 {
		t.Errorf("live model_turn = %d, want 0", n)
	}
}

// appendUngatedMCPToolUse plants an MCP call the brain never gated: the model
// asked for it, the policy allowed it, and it is waiting on the platform's MCP
// driver. It is the shape a confirmation resume has to route around, and no
// confirmation ever names it.
func appendUngatedMCPToolUse(t *testing.T, s *tserver, sessionID, server, name string) string {
	t.Helper()
	return appendGatedToolUse(t, s, sessionID, domain.EventAgentMCPToolUse,
		`{"name":"`+name+`","mcp_server_name":"`+server+
			`","input":{},"evaluated_permission":"allow","session_thread_id":null}`)
}

// A confirmation resume schedules the MCP call ahead of the built-ins it just
// cleared, web and sandbox alike. Only this platform's mcp_exec driver answers
// an agent.mcp_tool_use — a client posts neither the call nor its result, and a
// BYOC worker has no MCP surface — so scheduling anything else first hands the
// tool_exec a worker may claim a log it cannot finish.
func TestConfirmationResumesMCPAheadOfTheBuiltins(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	appendUngatedMCPToolUse(t, s, sessionID, "docs", "search")
	webAsk := appendAskToolUse(t, s, sessionID, "web_search")
	bashAsk := appendAskToolUse(t, s, sessionID, "bash")

	sendEvents(t, s, sessionID,
		confirm(webAsk, "allow", nil),
		confirm(bashAsk, "allow", nil))

	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status after allow = %q, want running", got)
	}
	if n := s.liveWork(sessionID, queue.MCPExec); n != 1 {
		t.Fatalf("live mcp_exec = %d, want 1", n)
	}
	if n := s.liveWork(sessionID, queue.WebExec); n != 0 {
		t.Errorf("live web_exec = %d, want 0 — the MCP pass chains it", n)
	}
	if n := s.liveWork(sessionID, queue.ToolExec); n != 0 {
		t.Errorf("live tool_exec = %d, want 0 — the MCP pass chains it", n)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 0 {
		t.Errorf("live model_turn = %d, want 0", n)
	}
}

// The same arm on the path that would otherwise schedule nothing at all: every
// gated tool is denied, so the denials answer them, but the MCP call is still
// outstanding. Without the arm this resume enqueues no work — the denied calls
// are answered so no executor is wanted, and the MCP call keeps the brain from
// being woken — and the session commits running with no trigger left, which
// also refuses archive and delete until a user.interrupt.
func TestConfirmationDenyStillSchedulesAnOutstandingMCPCall(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	appendUngatedMCPToolUse(t, s, sessionID, "docs", "search")
	bashAsk := appendAskToolUse(t, s, sessionID, "bash")

	sendEvents(t, s, sessionID, confirm(bashAsk, "deny", map[string]any{"deny_message": "no"}))

	if n := s.liveWork(sessionID, queue.MCPExec); n != 1 {
		t.Fatalf("live mcp_exec = %d, want 1", n)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 0 {
		t.Errorf("live model_turn = %d, want 0 — the MCP call is unanswered", n)
	}
}

func TestConfirmationDenyAnswersWithErrorAndResumesBrain(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	askID := appendAskToolUse(t, s, sessionID, "bash")

	sendEvents(t, s, sessionID, confirm(askID, "deny", map[string]any{"deny_message": "not allowed"}))

	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status after deny = %q, want running", got)
	}
	// No allowed tool remains, so the brain resumes directly — no executor.
	if n := s.liveWork(sessionID, queue.ToolExec); n != 0 {
		t.Errorf("live tool_exec = %d, want 0 (nothing to run)", n)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1 (resume on the deny result)", n)
	}
	// The denial is answered with an error result carrying the deny message.
	res := lastEventOfType(t, s, sessionID, "agent.tool_result")
	if res["tool_use_id"] != askID || res["is_error"] != true {
		t.Errorf("deny result = %v", res)
	}
	content, ok := res["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("deny result content = %v", res["content"])
	}
	if block := content[0].(map[string]any); block["text"] != "not allowed" {
		t.Errorf("deny result text = %v, want the deny_message", block["text"])
	}
}

// TestConfirmationDenyOfAnMCPCallAnswersInItsOwnFamily is the gate seen from the
// wire. A client watching an MCP tool is waiting for an agent.mcp_tool_result
// keyed by mcp_tool_use_id; answering with the built-in shape satisfies the
// database (the answered-ness check COALESCEs all three reference keys) and the
// replay, and is visible as wrong only here.
func TestConfirmationDenyOfAnMCPCallAnswersInItsOwnFamily(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	askID := appendAskMCPToolUse(t, s, sessionID, "docs", "search")

	sendEvents(t, s, sessionID, confirm(askID, "deny", map[string]any{"deny_message": "not allowed"}))

	res := lastEventOfType(t, s, sessionID, "agent.mcp_tool_result")
	if res["mcp_tool_use_id"] != askID {
		t.Errorf("mcp_tool_use_id = %v, want %s", res["mcp_tool_use_id"], askID)
	}
	if _, set := res["tool_use_id"]; set {
		t.Errorf("the MCP result also carries tool_use_id: %v", res)
	}
	if res["is_error"] != true {
		t.Errorf("is_error = %v, want true", res["is_error"])
	}
	// The store stamps it, since the platform emits it — a null here would look
	// to a settling brain like a client event still queued.
	if res["processed_at"] == nil {
		t.Errorf("processed_at = nil, want the append-time stamp")
	}
	content, ok := res["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %v, want one block", res["content"])
	}
	if block := content[0].(map[string]any); block["text"] != "not allowed" {
		t.Errorf("text = %v, want the deny_message", block["text"])
	}
	// The built-in shape must not be written alongside it.
	if n := countEventType(t, s, sessionID, "agent.tool_result"); n != 0 {
		t.Errorf("agent.tool_result count = %d, want 0", n)
	}
	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status after deny = %q, want running", got)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1 (resume on the deny result)", n)
	}
}

// TestConfirmationDenialsKeepEachCallInItsOwnFamily: one batch may refuse a
// built-in and an MCP call together, and each is answered in its own shape. A
// synthesis that picked one shape per batch — or read the family from the first
// denial — passes every single-family test and fails this one.
func TestConfirmationDenialsKeepEachCallInItsOwnFamily(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	builtin := appendAskToolUse(t, s, sessionID, "bash")
	mcp := appendAskMCPToolUse(t, s, sessionID, "docs", "search")

	sendEvents(t, s, sessionID,
		confirm(builtin, "deny", map[string]any{"deny_message": "no bash"}),
		confirm(mcp, "deny", map[string]any{"deny_message": "no docs"}))

	builtinRes := lastEventOfType(t, s, sessionID, "agent.tool_result")
	if builtinRes["tool_use_id"] != builtin {
		t.Errorf("agent.tool_result tool_use_id = %v, want %s", builtinRes["tool_use_id"], builtin)
	}
	mcpRes := lastEventOfType(t, s, sessionID, "agent.mcp_tool_result")
	if mcpRes["mcp_tool_use_id"] != mcp {
		t.Errorf("agent.mcp_tool_result mcp_tool_use_id = %v, want %s", mcpRes["mcp_tool_use_id"], mcp)
	}
	if n := countEventType(t, s, sessionID, "agent.tool_result"); n != 1 {
		t.Errorf("agent.tool_result count = %d, want 1", n)
	}
	if n := countEventType(t, s, sessionID, "agent.mcp_tool_result"); n != 1 {
		t.Errorf("agent.mcp_tool_result count = %d, want 1", n)
	}
}

// TestMCPCallBlocksTheRequiresActionGate: an ask-gated MCP call is part of the
// blocking set, so resolving only the built-in re-idles the session on the MCP
// id rather than resuming past a call no human has approved.
func TestMCPCallBlocksTheRequiresActionGate(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	builtin := appendAskToolUse(t, s, sessionID, "bash")
	mcp := appendAskMCPToolUse(t, s, sessionID, "docs", "search")

	sendEvents(t, s, sessionID, confirm(builtin, "allow", nil))

	if got := s.sessionStatus(sessionID); got != "idle" {
		t.Errorf("status = %q, want idle (the MCP ask is still blocking)", got)
	}
	idle := lastEventOfType(t, s, sessionID, "session.status_idle")
	stop, ok := idle["stop_reason"].(map[string]any)
	if !ok || stop["type"] != "requires_action" {
		t.Fatalf("stop_reason = %v, want requires_action", idle["stop_reason"])
	}
	ids, ok := stop["event_ids"].([]any)
	if !ok || len(ids) != 1 || ids[0] != mcp {
		t.Errorf("event_ids = %v, want [%s]", stop["event_ids"], mcp)
	}
	// Re-idling is only half of it: nothing may be scheduled either. A resume
	// arm that ran before the re-idle break would leave the session idle on the
	// ask *and* an mcp_exec item on its way to run the call the human has not
	// approved — a state this test would otherwise call correct.
	if n := s.liveWork(sessionID, queue.MCPExec); n != 0 {
		t.Errorf("mcp_exec = %d, want 0 — the call is still gated", n)
	}
	if n := s.liveWork(sessionID, queue.ToolExec); n != 0 {
		t.Errorf("tool_exec = %d, want 0 — the turn is suspended, not resumed", n)
	}
}

// TestConfirmationBatchedWithTheLastToolResultResumesTheTurn: a client may send
// its confirmation and an outstanding tool's result in one batch, and the two
// enqueue decisions that follow have to count that result. It is in the batch,
// not yet in the log, which is exactly why the two sibling arms of this switch
// pass ToolResultRefs — a confirmation arm that passes only the denied ids reads
// the call as still outstanding, declines to wake the brain, and commits a
// session with every call answered, nothing queued and no later trigger: the
// tool-result trigger fires on a *subsequent* send, and the client has nothing
// left to send. The session is stuck running, where archive and delete are both
// refused, and only a user.interrupt gets it back.
func TestConfirmationBatchedWithTheLastToolResultResumesTheTurn(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	askID := appendAskToolUse(t, s, sessionID, "bash")
	customID := appendToolUse(t, s, sessionID, domain.EventAgentCustomToolUse)

	sendEvents(t, s, sessionID,
		confirm(askID, "deny", map[string]any{"deny_message": "no"}),
		map[string]any{"type": "user.custom_tool_result", "custom_tool_use_id": customID,
			"content": []any{map[string]any{"type": "text", "text": "done"}}})

	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status = %q, want running", got)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1 — every call is answered, so the turn must resume", n)
	}
}

// TestConfirmationBatchedWithAToolResultDoesNotRunTheExecutorForIt is the same
// omission seen from the other side: a platform call answered in the same batch
// must not look outstanding either, or the resume provisions a sandbox and runs
// an executor pass that finds nothing to do.
func TestConfirmationBatchedWithAToolResultDoesNotRunTheExecutorForIt(t *testing.T) {
	s := newTestServer(t)
	sessionID := selfHostedSession(t, s)
	askID := appendAskToolUse(t, s, sessionID, "bash")
	otherID := appendToolUse(t, s, sessionID, domain.EventAgentToolUse)

	sendEvents(t, s, sessionID,
		confirm(askID, "deny", map[string]any{"deny_message": "no"}),
		map[string]any{"type": "user.tool_result", "tool_use_id": otherID,
			"content": []any{map[string]any{"type": "text", "text": "done"}}})

	if n := s.liveWork(sessionID, queue.ToolExec); n != 0 {
		t.Errorf("live tool_exec = %d, want 0 — the only platform call was answered in this batch", n)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1", n)
	}
}

func TestConfirmationPartialReIdlesWithRemainder(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	idA := appendAskToolUse(t, s, sessionID, "bash")
	idB := appendAskToolUse(t, s, sessionID, "read")

	// Confirm only A: the session re-idles blocked on B alone.
	sendEvents(t, s, sessionID, confirm(idA, "allow", nil))

	if got := s.sessionStatus(sessionID); got != "idle" {
		t.Errorf("status after partial confirm = %q, want idle", got)
	}
	if n := s.liveWork(sessionID, queue.ToolExec); n != 0 {
		t.Errorf("tool_exec after partial = %d, want 0", n)
	}
	idle := lastEventOfType(t, s, sessionID, "session.status_idle")
	stop, ok := idle["stop_reason"].(map[string]any)
	if !ok || stop["type"] != "requires_action" {
		t.Fatalf("stop_reason = %v, want requires_action", idle["stop_reason"])
	}
	ids, ok := stop["event_ids"].([]any)
	if !ok || len(ids) != 1 || ids[0] != idB {
		t.Errorf("remaining event_ids = %v, want [%s]", stop["event_ids"], idB)
	}

	// Confirm B: the last ask resolved, the session resumes.
	sendEvents(t, s, sessionID, confirm(idB, "allow", nil))
	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status after full confirm = %q, want running", got)
	}
	if n := s.liveWork(sessionID, queue.ToolExec); n != 1 {
		t.Errorf("tool_exec after full confirm = %d, want 1", n)
	}
}

func TestConfirmationMixedAllowDenyInOneBatch(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	idAllow := appendAskToolUse(t, s, sessionID, "bash")
	idDeny := appendAskToolUse(t, s, sessionID, "write")

	sendEvents(t, s, sessionID,
		confirm(idAllow, "allow", nil),
		confirm(idDeny, "deny", map[string]any{"deny_message": "nope"}))

	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status = %q, want running", got)
	}
	// One allowed tool still needs running → executor, not a bare model_turn.
	if n := s.liveWork(sessionID, queue.ToolExec); n != 1 {
		t.Errorf("tool_exec = %d, want 1", n)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 0 {
		t.Errorf("model_turn = %d, want 0", n)
	}
	res := lastEventOfType(t, s, sessionID, "agent.tool_result")
	if res["tool_use_id"] != idDeny {
		t.Errorf("deny result references %v, want %s", res["tool_use_id"], idDeny)
	}
}

// A user.message posted while the session is gated on confirmation must not
// wake the turn: replaying past the unresolved tool_use is a request the model
// rejects, and requires_action resolves only by confirmation. The message is
// appended and rides the next replay once the gate clears.
func TestConfirmationUserMessageDoesNotBypassGate(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	askID := appendAskToolUse(t, s, sessionID, "bash")

	sendEvents(t, s, sessionID, userMessage("actually, do something else"))

	if got := s.sessionStatus(sessionID); got != "idle" {
		t.Errorf("status after user.message while gated = %q, want idle", got)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 0 {
		t.Errorf("model_turn enqueued while gated = %d, want 0", n)
	}
	if n := countEventType(t, s, sessionID, "session.status_running"); n != 0 {
		t.Errorf("session.status_running while gated = %d, want 0", n)
	}

	// The confirmation still resolves the gate, and the message is on the log
	// to be replayed.
	sendEvents(t, s, sessionID, confirm(askID, "allow", nil))
	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status after confirmation = %q, want running", got)
	}
	if n := s.liveWork(sessionID, queue.ToolExec); n != 1 {
		t.Errorf("tool_exec after confirmation = %d, want 1", n)
	}
	if countEventType(t, s, sessionID, "user.message") != 1 {
		t.Errorf("user.message was not retained on the log")
	}
}

// A batch that mixes a confirmation clearing the gate with a user.message must
// resolve as a confirmation (run the confirmed tool), not wake on the message.
func TestConfirmationWithUserMessageInOneBatchResolvesGate(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	askID := appendAskToolUse(t, s, sessionID, "bash")

	sendEvents(t, s, sessionID,
		confirm(askID, "allow", nil),
		userMessage("and also this"))

	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status = %q, want running", got)
	}
	// The confirmed tool must run — an executor, not a bare model_turn that
	// would replay the unanswered tool_use.
	if n := s.liveWork(sessionID, queue.ToolExec); n != 1 {
		t.Errorf("tool_exec = %d, want 1 (confirmation ran, not the message)", n)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 0 {
		t.Errorf("model_turn = %d, want 0", n)
	}
}

// A tool result cannot answer an ask-gated tool before the human confirms it:
// that would bypass the approval and, on a later denial, double-answer the tool
// use on the append-only log.
func TestConfirmationToolResultForUnconfirmedAskRejected(t *testing.T) {
	s := newTestServer(t)
	sessionID := selfHostedSession(t, s) // user.tool_result is self_hosted-only
	askID := appendAskToolUse(t, s, sessionID, "bash")

	status, body := s.do(http.MethodPost, "/v1/sessions/"+sessionID+"/events",
		map[string]any{"events": []any{map[string]any{
			"type": "user.tool_result", "tool_use_id": askID,
			"content": []any{map[string]any{"type": "text", "text": "sneaky"}}}}})
	if status != http.StatusBadRequest {
		t.Errorf("tool_result for unconfirmed ask: status %d, want 400 (body %v)", status, body)
	}
}

// Denying the only platform tool while a client-executed custom tool is still
// unanswered must not enqueue a tool_exec: the executor runs only built-ins, so
// it would provision a sandbox for nothing. The session resumes on the client's
// custom result instead.
func TestConfirmationDenyWithPendingCustomToolWaitsForClient(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	askID := appendAskToolUse(t, s, sessionID, "bash")
	customID := appendToolUse(t, s, sessionID, domain.EventAgentCustomToolUse)

	sendEvents(t, s, sessionID, confirm(askID, "deny", map[string]any{"deny_message": "no"}))

	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status = %q, want running", got)
	}
	if n := s.liveWork(sessionID, queue.ToolExec); n != 0 {
		t.Errorf("tool_exec = %d, want 0 (no platform work; custom tool is client-executed)", n)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 0 {
		t.Errorf("model_turn = %d, want 0 (waiting on the client's custom result)", n)
	}

	// The client's custom result completes the set and resumes the turn.
	sendEvents(t, s, sessionID, map[string]any{
		"type": "user.custom_tool_result", "custom_tool_use_id": customID,
		"content": []any{map[string]any{"type": "text", "text": "done"}}})
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 1 {
		t.Errorf("model_turn after custom result = %d, want 1", n)
	}
}

func TestConfirmationValidation(t *testing.T) {
	s := newTestServer(t)
	sessionID := eventsFixture(t, s)
	askID := appendAskToolUse(t, s, sessionID, "bash")

	post := func(evs ...map[string]any) int {
		status, _ := s.do(http.MethodPost, "/v1/sessions/"+sessionID+"/events", map[string]any{"events": evs})
		return status
	}

	// Unknown reference.
	if got := post(confirm("sevt_00000000000000000000000000", "allow", nil)); got != http.StatusBadRequest {
		t.Errorf("unknown ref: status %d, want 400", got)
	}
	// deny_message is only valid with a deny (inbound validation).
	if got := post(confirm(askID, "allow", map[string]any{"deny_message": "x"})); got != http.StatusBadRequest {
		t.Errorf("deny_message on allow: status %d, want 400", got)
	}
	// Duplicate within one request.
	if got := post(confirm(askID, "allow", nil), confirm(askID, "allow", nil)); got != http.StatusBadRequest {
		t.Errorf("intra-batch duplicate: status %d, want 400", got)
	}
	// A tool that was not gated for confirmation cannot be confirmed.
	allowID := appendToolUseWithPerm(t, s, sessionID, "grep", "allow")
	if got := post(confirm(allowID, "allow", nil)); got != http.StatusBadRequest {
		t.Errorf("non-ask tool: status %d, want 400", got)
	}
	// The valid confirmation lands…
	if got := post(confirm(askID, "allow", nil)); got != http.StatusOK {
		t.Errorf("valid confirm: status %d, want 200", got)
	}
	// …and a second confirmation for the same call is rejected.
	if got := post(confirm(askID, "allow", nil)); got != http.StatusBadRequest {
		t.Errorf("already confirmed: status %d, want 400", got)
	}
}
