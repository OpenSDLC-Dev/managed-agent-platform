package api_test

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// Within one commit the log is written in processing order, as the reference
// lists it (#793, docs/plan/56_processing-order.md): a waking user.message or
// user.define_outcome follows the running pair of the turn that consumes it,
// and an interrupt follows the results it synthesizes and precedes the idle it
// ends in (#539). The list and the live stream read the same order; the POST
// echo keeps the order the client posted.

func TestAWakingMessageFollowsItsRunningPair(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	st := s.stream(t, "/v1/sessions/"+sid+"/events/stream")

	echo := sendEvents(t, s, sid, userMessage("first"), userMessage("second"))

	want := []string{"session.status_running", "session.thread_status_running", "user.message", "user.message"}
	if got := s.eventTypes(sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
	for i, w := range want {
		if f := st.next(t); f.name != w {
			t.Errorf("stream frame %d = %q, want %q", i, f.name, w)
		}
	}
	st.expectNone(t)
	// The echo is the posted events, in the order posted, under the ids the
	// log holds them by — not the first rows of the batch, which are now the
	// platform's pair.
	_, list := s.do("GET", "/v1/sessions/"+sid+"/events", nil)
	evs := listData(t, list)
	if len(echo) != 2 || echo[0]["id"] != evs[2]["id"] || echo[1]["id"] != evs[3]["id"] {
		t.Fatalf("echo = %v, want the two messages as listed at 2 and 3", echo)
	}
	for i, text := range []string{"first", "second"} {
		if got := echo[i]["content"].([]any)[0].(map[string]any)["text"]; got != text {
			t.Errorf("echo[%d] text = %v, want %q", i, got, text)
		}
		// A waking message is stamped when the request that consumes it
		// starts (#793), which is after the echo, so it is still queued then.
		if echo[i]["processed_at"] != nil {
			t.Errorf("echo[%d] processed_at = %v, want null until its request starts", i, echo[i]["processed_at"])
		}
	}
}

func TestAWakingOutcomeFollowsItsRunningPair(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)

	sendEvents(t, s, sid, defineOutcome("Build a DCF model", nil))

	want := []string{"session.status_running", "session.thread_status_running", "user.define_outcome"}
	if got := s.eventTypes(sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
}

// A redirect posted message-first still logs the interrupt first: it is
// processed first, ending the turn the message then replaces. The echo keeps
// the posted order.
func TestARedirectPostedMessageFirstEchoesAsPosted(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	sendEvents(t, s, sid, userMessage("start"))

	echo := sendEvents(t, s, sid, userMessage("instead"), map[string]any{"type": "user.interrupt"})

	if len(echo) != 2 || echo[0]["type"] != "user.message" || echo[1]["type"] != "user.interrupt" {
		t.Fatalf("echo = %v, want the posted order", echo)
	}
	want := []string{"session.status_running", "session.thread_status_running", "user.message",
		"user.interrupt", "session.thread_status_idle", "session.status_idle",
		"session.status_running", "session.thread_status_running", "user.message"}
	if got := s.eventTypes(sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
	_, list := s.do("GET", "/v1/sessions/"+sid+"/events", nil)
	evs := listData(t, list)
	if echo[0]["id"] != evs[8]["id"] || echo[1]["id"] != evs[3]["id"] {
		t.Errorf("echo ids %v/%v, want the redirect message at 8 and the interrupt at 3", echo[0]["id"], echo[1]["id"])
	}
}

// The documented way to chain outcomes: an interrupt settles the active one,
// and a define_outcome in the same send starts the next. The end the interrupt
// writes comes before it, and the new outcome after the running pair of the
// turn that pursues it.
func TestAChainedOutcomeFollowsTheEndItReplaces(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	sendEvents(t, s, sid, defineOutcome("first", nil))

	sendEvents(t, s, sid, map[string]any{"type": "user.interrupt"}, defineOutcome("second", nil))

	want := []string{"session.status_running", "session.thread_status_running", "user.define_outcome",
		"span.outcome_evaluation_end", "user.interrupt", "session.thread_status_idle", "session.status_idle",
		"session.status_running", "session.thread_status_running", "user.define_outcome"}
	if got := s.eventTypes(sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
}

// The dream runner's whole-session interrupt writes what a client's does, in
// the same order (#539).
func TestTheRunnersInterruptWritesItsResultsFirst(t *testing.T) {
	s := newTestServer(t)
	sid := selfHostedSession(t, s)
	suspendedOnTool(t, s, sid, domain.EventAgentToolUse)

	if err := api.InterruptSessionForTest(context.Background(), s.pool, sid); err != nil {
		t.Fatal(err)
	}

	want := []string{"session.status_running", "session.thread_status_running", "user.message",
		"agent.tool_use", "agent.tool_result", "user.interrupt",
		"session.thread_status_idle", "session.status_idle"}
	if got := s.eventTypes(sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
}
