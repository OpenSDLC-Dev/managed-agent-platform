package api_test

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// A thread an answer resumes is written in processing order too (#793 PR-C,
// docs/plan/56_processing-order.md): the answer where it was consumed, on
// receipt; then the resume's running pair; then what the resumed turn
// consumes — on the primary, the result a denial writes and the input posted
// beside the answer. The send writes the pair itself, so the settlement after
// the append finds the thread running and writes no second one.

// gatedPrimary parks the primary on the given calls, planted on its own log,
// idle on requires_action naming all of them, and returns their ids.
func gatedPrimary(t *testing.T, s *tserver, sid string, calls ...plantedCall) []string {
	t.Helper()
	var ids []string
	for _, c := range calls {
		ids = append(ids, appendOn(t, s, sid, "", false, c.typ, c.payload))
	}
	stop := `{"type":"requires_action","event_ids":[`
	for i, id := range ids {
		if i > 0 {
			stop += ","
		}
		stop += `"` + id + `"`
	}
	setThread(t, s, domain.PrimaryThreadID(domain.ID(sid)).String(), "idle", stop+`]}`)
	return ids
}

// plantedCall is one call gatedPrimary plants.
type plantedCall struct {
	typ     domain.EventType
	payload string
}

var (
	askGate    = plantedCall{domain.EventAgentToolUse, askBashCall}
	customGate = plantedCall{domain.EventAgentCustomToolUse, customCall}
)

// The recorded shape, end to end with a real brain: a denial that resumes the
// primary lists the confirmation, the resume's running pair, the denial's
// result and then the resumed turn's span.model_request_start, as
// 2026-09-12-archived-threads/batch1.json ([5].body.data idx 31 to 35) and
// every other recorded primary deny list them. One result, one pair.
func TestADenialThatResumesThePrimaryFollowsItsRunningPair(t *testing.T) {
	s := newTestServer(t)
	sid, askID := suspendViaBrain(t, s)
	seq := lastSeq(t, s, sid)

	sendEvents(t, s, sid, confirm(askID, "deny", nil))
	b := newScriptedBrain(t, s.pool, []provider.Chunk{
		{Kind: provider.KindTextDelta, Text: "understood"},
		{Kind: provider.KindDone, StopReason: "end_turn", Usage: &domain.ModelUsage{InputTokens: 5, OutputTokens: 1}},
	})
	if found, err := b.RunOnce(context.Background()); err != nil || !found {
		t.Fatalf("brain RunOnce: found=%v err=%v", found, err)
	}

	got := s.eventTypes(sid)
	confirmAt := -1
	for i, ty := range got {
		if ty == "user.tool_confirmation" {
			confirmAt = i
		}
	}
	want := []string{"user.tool_confirmation", "session.status_running", "session.thread_status_running",
		"agent.tool_result", "span.model_request_start"}
	if confirmAt < 0 || len(got) < confirmAt+len(want) || !sameStrings(got[confirmAt:confirmAt+len(want)], want) {
		t.Fatalf("event log = %v, want %v from the confirmation on", got, want)
	}
	after := got[confirmAt:]
	for ty, n := range map[string]int{"agent.tool_result": 1, "session.status_running": 1, "session.thread_status_running": 1} {
		if c := len(typesAmong(after, ty)); c != n {
			t.Errorf("%d %s from the confirmation on, want %d: %v", c, ty, n, after)
		}
	}
	stampsRunForward(t, s, sid, seq)
}

// Input posted beside a denial that resumes the primary is read by the resumed
// turn, so it follows the pair and the denial's result, in either posted order.
func TestAMessageBesideADenialThatResumesThePrimaryFollowsIt(t *testing.T) {
	for _, first := range []string{"deny", "message"} {
		t.Run(first+" first", func(t *testing.T) {
			s := newTestServer(t)
			sid := eventsFixture(t, s)
			ids := gatedPrimary(t, s, sid, askGate)
			seq := lastSeq(t, s, sid)

			posted := []map[string]any{confirm(ids[0], "deny", nil), userMessage("do it another way")}
			if first == "message" {
				posted[0], posted[1] = posted[1], posted[0]
			}
			echo := sendEvents(t, s, sid, posted...)

			want := []string{"agent.tool_use", "user.tool_confirmation", "session.status_running",
				"session.thread_status_running", "agent.tool_result", "user.message"}
			if got := s.eventTypes(sid); !sameStrings(got, want) {
				t.Fatalf("event log = %v, want %v", got, want)
			}
			if echo[0]["type"] != posted[0]["type"] || echo[1]["type"] != posted[1]["type"] {
				t.Errorf("echo = %v, want the posted order", echo)
			}
			stampsRunForward(t, s, sid, seq)
			pendingOnlyAtTail(t, s, sid, seq)
		})
	}
}

// An allow resumes the primary to run the call it released, and the message
// posted beside it waits for the turn after that call: it follows the pair.
func TestAMessageBesideAnAllowThatResumesThePrimaryFollowsThePair(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	ids := gatedPrimary(t, s, sid, askGate)
	seq := lastSeq(t, s, sid)

	sendEvents(t, s, sid, userMessage("and then"), confirm(ids[0], "allow", nil))

	want := []string{"agent.tool_use", "user.tool_confirmation", "session.status_running",
		"session.thread_status_running", "user.message"}
	if got := s.eventTypes(sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
	if n := s.liveWork(sid, queue.ToolExec); n != 1 {
		t.Errorf("live tool_exec = %d, want 1: the allowed call runs first", n)
	}
	stampsRunForward(t, s, sid, seq)
}

// A tool answer that completes the primary's set resumes it; the message
// posted beside it is what the resumed turn reads, so it follows the pair. The
// placement PR-A registered as out of reach, now matched.
func TestAMessageBesideAnAnswerThatResumesThePrimaryFollowsTheResume(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	ids := gatedPrimary(t, s, sid, customGate)
	seq := lastSeq(t, s, sid)

	sendEvents(t, s, sid, customResult(ids[0]), userMessage("and also"))

	want := []string{"agent.custom_tool_use", "user.custom_tool_result",
		"session.status_running", "session.thread_status_running", "user.message"}
	if got := s.eventTypes(sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
	stampsRunForward(t, s, sid, seq)
}

// A denial of the primary's first call and a result for its second, in one
// send, resume it together: both answers where they were consumed, in the
// order the walk processes them; then the pair; then the denial's result,
// which the resumed turn consumes. The reference lists a denial held behind a
// later answer the same way once that answer arrives
// (2026-09-19-custom-order-followup setup.json,
// ask-first-deny.final-audit.events idx 15 to 19: the custom result, the pair,
// the denial's result, the request).
func TestADenialAndTheAnswerItUnlocksResumeThePrimaryBeforeTheDenialsResult(t *testing.T) {
	for _, first := range []string{"deny", "result"} {
		t.Run(first+" first", func(t *testing.T) {
			s := newTestServer(t)
			sid := eventsFixture(t, s)
			ids := gatedPrimary(t, s, sid, askGate, customGate)
			seq := lastSeq(t, s, sid)

			answers := []map[string]any{confirm(ids[0], "deny", nil), customResult(ids[1])}
			if first == "result" {
				answers[0], answers[1] = answers[1], answers[0]
			}
			sendEvents(t, s, sid, answers...)

			want := []string{"agent.tool_use", "agent.custom_tool_use", "user.tool_confirmation",
				"user.custom_tool_result", "session.status_running", "session.thread_status_running", "agent.tool_result"}
			if got := s.eventTypes(sid); !sameStrings(got, want) {
				t.Fatalf("event log = %v, want %v", got, want)
			}
			if n := countWhole(t, s, sid, "agent.tool_result"); n != 1 {
				t.Errorf("%d denial results, want exactly one", n)
			}
			if n := s.liveWork(sid, queue.ModelTurn); n != 1 {
				t.Errorf("live model_turn = %d, want 1", n)
			}
			stampsRunForward(t, s, sid, seq)
			pendingOnlyAtTail(t, s, sid, seq)
		})
	}
}

// A child's denial keeps its result beside the confirmation, ahead of the
// child's running event: the one recorded child denial lists them so on the
// child's own list (2026-09-12-console-followups/approvals-network.json, the
// child thread's events, idx 7 to 10: the confirmation, the denial's result,
// the child's session.thread_status_running, the request). The resume itself
// keeps item 2's shape: the child's own running event, behind a
// session.status_running when the fold moves, written by the send.
func TestAChildsDenialIsListedAheadOfItsResume(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	setThread(t, s, domain.PrimaryThreadID(domain.ID(sid)).String(), "idle", `{"type":"end_turn"}`)
	child := gatedChild(t, s, sid)
	askID := lastEventOfType(t, s, sid, "agent.tool_use")["id"].(string)
	seq := lastSeq(t, s, sid)

	sendEvents(t, s, sid, confirm(askID, "deny", nil))

	want := []string{"agent.tool_use", "user.tool_confirmation", "agent.tool_result",
		"session.status_running", "session.thread_status_running"}
	if got := wholeLogTypes(t, s, sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
	_, res := s.do("GET", "/v1/sessions/"+sid+"/threads/"+child+"/events", nil)
	var own []string
	for _, ev := range listData(t, res) {
		own = append(own, ev["type"].(string))
	}
	if want := []string{"agent.tool_use", "user.tool_confirmation", "agent.tool_result",
		"session.thread_status_running"}; !sameStrings(own, want) {
		t.Errorf("child's own list = %v, want %v", own, want)
	}
	if got := s.threadStatus(t, child); got != "running" {
		t.Errorf("child = %q, want resumed", got)
	}
	stampsRunForward(t, s, sid, seq)
}

// An answer that leaves a gate of its thread open resumes nothing, so the send
// writes no running pair. A denial of the first of two gated calls is
// processed, and the thread stays idle on the second, re-announcing the gate
// still open; its result stays beside its confirmation (ours: the reference
// writes it only once the thread resumes — docs/DIVERGENCES.md, the primary
// thread's entry). An allow of the second waits behind the first, unprocessed,
// at the tail.
func TestAnAnswerThatLeavesAGateOpenWritesNoPair(t *testing.T) {
	for _, tc := range []struct {
		name   string
		call   int
		result string
		want   []string
	}{
		{"deny the first", 0, "deny", []string{"agent.tool_use", "agent.tool_use", "user.tool_confirmation",
			"agent.tool_result", "session.thread_status_idle", "session.status_idle"}},
		{"allow the second", 1, "allow", []string{"agent.tool_use", "agent.tool_use", "user.tool_confirmation"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			sid := eventsFixture(t, s)
			ids := gatedPrimary(t, s, sid, askGate, askGate)
			seq := lastSeq(t, s, sid)

			sendEvents(t, s, sid, confirm(ids[tc.call], tc.result, nil))

			if got := s.eventTypes(sid); !sameStrings(got, tc.want) {
				t.Fatalf("event log = %v, want %v", got, tc.want)
			}
			if st := s.sessionStatus(sid); st != "idle" {
				t.Errorf("session = %q, want idle", st)
			}
			stampsRunForward(t, s, sid, seq)
			pendingOnlyAtTail(t, s, sid, seq)
		})
	}
}
