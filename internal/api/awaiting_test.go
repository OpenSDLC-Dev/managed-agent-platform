package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// whileAwaiting is the reference's refusal of an event posted while the
// session waits on responses (2026-09-12-followups budget-runtime.json #4
// `rec.budget.message-over-budget`), its ids as Go renders a slice.
func whileAwaiting(typ string, index int, ids ...string) string {
	return fmt.Sprintf("Invalid %s event at events[%d]: waiting on responses to events [%s]; "+
		"only `user.tool_confirmation`, `user.custom_tool_result`, `user.tool_result`, or `user.interrupt` may be sent "+
		"(a `system.message` may trail a tool result)", typ, index, strings.Join(ids, " "))
}

// sendRefusedWhileAwaiting posts a batch and asserts the refusal, and that it
// left the log, the status and the work queue as they were.
func sendRefusedWhileAwaiting(t *testing.T, s *tserver, sessionID, want string, evs ...map[string]any) {
	t.Helper()
	before, status := s.eventTypes(sessionID), s.sessionStatus(sessionID)
	code, body := s.do(http.MethodPost, "/v1/sessions/"+sessionID+"/events", map[string]any{"events": evs})
	wantErrMsg(t, code, body, http.StatusBadRequest, "invalid_request_error", want)
	if got := s.eventTypes(sessionID); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("a refused send changed the log: %v, was %v", got, before)
	}
	if got := s.sessionStatus(sessionID); got != status {
		t.Errorf("a refused send moved the status to %q from %q", got, status)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 0 {
		t.Errorf("a refused send enqueued %d model turns", n)
	}
}

// TestAMessageWhileACustomCallAwaitsItsResultIsRefused pins the recorded
// refusal: the session idle on requires_action for one agent.custom_tool_use,
// a lone user.message is 400 naming that call (2026-09-12-followups
// budget-runtime.json #4), where this platform used to queue it behind the
// call. The result is what the reference then took (#6), and a message after
// it is read.
func TestAMessageWhileACustomCallAwaitsItsResultIsRefused(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	callID := appendGatedToolUse(t, s, sid, domain.EventAgentCustomToolUse, `{"name":"record_probe","input":{"value":"ok"},"session_thread_id":null}`)

	sendRefusedWhileAwaiting(t, s, sid, whileAwaiting("user.message", 0, callID), userMessage("Say DONE."))

	sendEvents(t, s, sid, customResult(callID))
	sendEvents(t, s, sid, userMessage("Say DONE."))
	if n := countEventType(t, s, sid, "user.message"); n != 1 {
		t.Errorf("user.message on the log = %d, want the one sent after the result", n)
	}
}

// TestWhatTheSendWaitsOn pins the generalization of the recorded refusal to
// every response the primary can wait on, and how a send's own events count.
// The reference's sentence names the four events that may be sent, so it is
// read as the one gate for all three waits: a confirmation an ask-gated call
// needs, a custom call's result and a self_hosted worker's. A
// user.define_outcome is held to it like a message. A send is judged with its
// own answers in, wherever they sit in it, so a message beside the answers
// that free the primary is read as before, and an interrupt reaching the
// primary answers everything; only what the primary would still await
// afterwards is refused, the ids listed in log order. None of this but the
// lone message beside one custom call is recorded (docs/DIVERGENCES.md).
func TestWhatTheSendWaitsOn(t *testing.T) {
	s := newTestServer(t)
	ask := func(t *testing.T) (string, string) {
		sid := eventsFixture(t, s)
		return sid, appendAskToolUse(t, s, sid, "bash")
	}

	t.Run("an unconfirmed ask", func(t *testing.T) {
		sid, askID := ask(t)
		sendRefusedWhileAwaiting(t, s, sid, whileAwaiting("user.message", 0, askID), userMessage("go on"))
		sendRefusedWhileAwaiting(t, s, sid, whileAwaiting("user.define_outcome", 0, askID), defineOutcome("ship it", nil))
		// An allow answers it: the platform runs the call, nothing is awaited.
		sendEvents(t, s, sid, confirm(askID, "allow", nil), userMessage("and then this"))
	})

	t.Run("a worker's result", func(t *testing.T) {
		sid := selfHostedSession(t, s)
		callID := appendGatedToolUse(t, s, sid, domain.EventAgentToolUse,
			`{"name":"bash","input":{},"evaluated_permission":"allow","session_thread_id":null}`)
		sendRefusedWhileAwaiting(t, s, sid, whileAwaiting("user.message", 0, callID), userMessage("still there?"))
		sendEventsAs(t, s, workerAuth(t, s, sid), sid, userMessage("carry on"),
			map[string]any{"type": "user.tool_result", "tool_use_id": callID,
				"content": []any{map[string]any{"type": "text", "text": "ok"}}})
	})

	t.Run("an allow leaves a worker's call awaiting its result", func(t *testing.T) {
		sid := selfHostedSession(t, s)
		callID := appendGatedToolUse(t, s, sid, domain.EventAgentToolUse,
			`{"name":"bash","input":{},"evaluated_permission":"ask","session_thread_id":null}`)
		sendRefusedWhileAwaiting(t, s, sid, whileAwaiting("user.message", 1, callID),
			confirm(callID, "allow", nil), userMessage("go"))
		// A denial is the call's answer.
		sendEvents(t, s, sid, confirm(callID, "deny", nil), userMessage("do something else"))
	})

	t.Run("several calls, one answered in the send", func(t *testing.T) {
		sid := eventsFixture(t, s)
		first := appendGatedToolUse(t, s, sid, domain.EventAgentCustomToolUse, `{"name":"a","input":{},"session_thread_id":null}`)
		second := appendGatedToolUse(t, s, sid, domain.EventAgentCustomToolUse, `{"name":"b","input":{},"session_thread_id":null}`)
		third := appendAskToolUse(t, s, sid, "bash")
		sendRefusedWhileAwaiting(t, s, sid, whileAwaiting("user.message", 0, first, second, third), userMessage("hello"))
		sendRefusedWhileAwaiting(t, s, sid, whileAwaiting("user.message", 1, first, third),
			customResult(second), userMessage("hello"))
		// The message first, the answers after it: all are in, so it is read.
		sendEvents(t, s, sid, userMessage("hello"), customResult(first), customResult(second), confirm(third, "deny", nil))
	})

	// A denial answers its call even while it waits behind an earlier one,
	// before the walk writes its result: a worker's gated call denied ahead of
	// the custom call it follows is no longer awaited, neither named nor in
	// the way of the custom call's answer.
	t.Run("a denial queued behind an earlier call", func(t *testing.T) {
		sid := selfHostedSession(t, s)
		custom := appendGatedToolUse(t, s, sid, domain.EventAgentCustomToolUse, `{"name":"a","input":{},"session_thread_id":null}`)
		gated := appendGatedToolUse(t, s, sid, domain.EventAgentToolUse,
			`{"name":"bash","input":{},"evaluated_permission":"ask","session_thread_id":null}`)
		sendEvents(t, s, sid, confirm(gated, "deny", nil))
		sendRefusedWhileAwaiting(t, s, sid, whileAwaiting("user.message", 0, custom), userMessage("hello"))
		sendEvents(t, s, sid, customResult(custom), userMessage("hello"))
	})

	t.Run("an interrupt reaching the primary", func(t *testing.T) {
		sid, _ := ask(t)
		sendEvents(t, s, sid, map[string]any{"type": "user.interrupt"}, userMessage("redirect"))
	})

	// Only a primary resting idle on its calls was recorded refusing: one
	// still running — its turn, or a platform call beside the custom one —
	// queues the message as before, unprocessed until a turn reads it.
	t.Run("a running primary", func(t *testing.T) {
		sid := eventsFixture(t, s)
		appendGatedToolUse(t, s, sid, domain.EventAgentCustomToolUse, `{"name":"a","input":{},"session_thread_id":null}`)
		pgtest.SetSessionStatus(t, s.pool, domain.ID(sid), "running")
		sendEvents(t, s, sid, userMessage("meanwhile"))
		wantQueuedMessage(t, s, sid, "running")
	})

	t.Run("a system.message trailing a result", func(t *testing.T) {
		sid := eventsFixture(t, s)
		first := appendGatedToolUse(t, s, sid, domain.EventAgentCustomToolUse, `{"name":"a","input":{},"session_thread_id":null}`)
		appendGatedToolUse(t, s, sid, domain.EventAgentCustomToolUse, `{"name":"b","input":{},"session_thread_id":null}`)
		sendEvents(t, s, sid, customResult(first),
			map[string]any{"type": "system.message", "content": []any{map[string]any{"type": "text", "text": "note"}}})
	})
}

// TestAMessageBehindACallAwaitingNothingExternalQueues pins the wake arm's
// other half: an idle primary whose head call awaits nothing from outside —
// here a platform call allowed to run and not yet run, as a log stranded
// before #181 holds one — is no state the reference refuses in, and its
// message is accepted and queued behind the call rather than waking a turn
// that would replay a tool_use nothing answers.
func TestAMessageBehindACallAwaitingNothingExternalQueues(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	appendToolUseWithPerm(t, s, sid, "bash", "allow")
	sendEvents(t, s, sid, userMessage("are you still there?"))
	wantQueuedMessage(t, s, sid, "idle")
}

// wantQueuedMessage asserts the session's one user.message is on the log
// unprocessed, the status unmoved and no turn enqueued.
func wantQueuedMessage(t *testing.T, s *tserver, sid, status string) {
	t.Helper()
	var n, processed int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*), count(processed_at) FROM events
		 WHERE session_id = $1 AND type = 'user.message'`, sid).Scan(&n, &processed); err != nil {
		t.Fatal(err)
	}
	if n != 1 || processed != 0 {
		t.Errorf("user.message rows = %d, processed %d; want one, queued", n, processed)
	}
	if got := s.sessionStatus(sid); got != status {
		t.Errorf("status = %q, want %q", got, status)
	}
	if n := s.liveWork(sid, queue.ModelTurn); n != 0 {
		t.Errorf("model turns enqueued = %d, want 0", n)
	}
}
