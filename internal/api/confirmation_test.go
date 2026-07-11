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
	payload := []byte(`{"name":"` + name + `","input":{},"evaluated_permission":"` + perm + `","session_thread_id":null}`)
	evs, err := events.NewLog(s.pool).Append(context.Background(), domain.ID(sessionID),
		[]events.NewEvent{{Type: domain.EventAgentToolUse, Payload: payload}})
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
