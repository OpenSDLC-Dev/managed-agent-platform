package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
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
		// A waking message is consumed at turn start and stamped when that
		// turn settles, so it is still queued when echoed.
		if echo[i]["processed_at"] != nil {
			t.Errorf("echo[%d] processed_at = %v, want null until its turn settles", i, echo[i]["processed_at"])
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

// runningSession marks the session running under whatever thread the caller
// planted running — the fold the platform's own transitions would have left.
func runningSession(t *testing.T, s *tserver, sid string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `UPDATE sessions SET status = 'running' WHERE id = $1`, sid); err != nil {
		t.Fatal(err)
	}
}

// lastSeq is the session's newest seq: what a send's own commit writes lies
// past it.
func lastSeq(t *testing.T, s *tserver, sid string) int64 {
	t.Helper()
	var seq int64
	if err := s.pool.QueryRow(context.Background(),
		`SELECT COALESCE(MAX(seq), 0) FROM events WHERE session_id = $1`, sid).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

// stampsRunForward fails unless the rows written past seq carry processed_at
// non-decreasing in list order, skipping the rows still pending: a commit is
// written in processing order, so its stamps must say the same (#539).
func stampsRunForward(t *testing.T, s *tserver, sid string, seq int64) {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT type, processed_at FROM events WHERE session_id = $1 AND seq > $2 ORDER BY seq`, sid, seq)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var prev *time.Time
	var prevType string
	for rows.Next() {
		var typ string
		var at *time.Time
		if err := rows.Scan(&typ, &at); err != nil {
			t.Fatal(err)
		}
		if at == nil {
			continue
		}
		if prev != nil && at.Before(*prev) {
			t.Errorf("%s processed at %s, before the %s listed ahead of it (%s)", typ,
				at.Format(time.RFC3339Nano), prevType, prev.Format(time.RFC3339Nano))
		}
		prev, prevType = at, typ
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// A notice follows the running pair of the thread it is delivered to, whichever
// arm woke that thread: here the message wakes the idle coordinator, and the
// interrupt of its running child — posted second, processed after the
// message's wake — tells a coordinator already running. The interrupt and the
// child's idle are consumed on receipt; the message and the notice are what
// the woken turn reads.
func TestANoticeFollowsTheWakeOfItsTarget(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	setThread(t, s, primary, "idle", `{"type":"end_turn"}`)
	child := insertChild(t, s, sid, "running")
	runningSession(t, s, sid)

	echo := sendEvents(t, s, sid, userMessage("go on"),
		map[string]any{"type": "user.interrupt", "session_thread_id": child})

	// The session stays running throughout under the fold, so the wake is the
	// primary's thread event alone (docs/DIVERGENCES.md, item 2's residual).
	want := []string{"user.interrupt", "session.thread_status_idle", "session.thread_status_running",
		"user.message", "agent.thread_message_received"}
	if got := s.eventTypes(sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
	if len(echo) != 2 || echo[0]["type"] != "user.message" || echo[1]["type"] != "user.interrupt" {
		t.Errorf("echo = %v, want the posted order", echo)
	}
}

// Two children interrupted in one send, where only the second's ending takes
// the last busy child a parked coordinator had: both notices are what the
// woken coordinator reads, so both follow its running event — the first one
// too, though its own arm woke nothing.
func TestEveryNoticeFollowsTheWakeTheLastEndingCauses(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	setThread(t, s, primary, "idle", `{"type":"end_turn"}`)
	first := insertChild(t, s, sid, "running")
	second := insertChild(t, s, sid, "running")
	runningSession(t, s, sid)
	seq := lastSeq(t, s, sid)

	sendEvents(t, s, sid,
		map[string]any{"type": "user.interrupt", "session_thread_id": first},
		map[string]any{"type": "user.interrupt", "session_thread_id": second})

	want := []string{"user.interrupt", "session.thread_status_idle", "user.interrupt", "session.thread_status_idle",
		"session.thread_status_running", "agent.thread_message_received", "agent.thread_message_received"}
	if got := s.eventTypes(sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
	_, list := s.do("GET", "/v1/sessions/"+sid+"/events", nil)
	evs := listData(t, list)
	for i, from := range []string{first, second} {
		if got := evs[5+i]["from_session_thread_id"]; got != from {
			t.Errorf("notice %d is from %v, want %s: notices in the order their interrupts were received", i, got, from)
		}
	}
	stampsRunForward(t, s, sid, seq)
}

// An input that wakes nothing is consumed later, by a turn already running,
// so it goes behind everything this send processes — here the interrupt of a
// child with a call outstanding, the result it synthesizes, the child's idle
// and the notice to the running coordinator — though it was posted after the
// interrupt, not before it.
func TestAMessageThatWakesNothingFollowsWhatTheSendProcessed(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	setThread(t, s, primary, "running", "")
	child := insertChild(t, s, sid, "running")
	appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentToolUse, allowBashCall)
	runningSession(t, s, sid)
	seq := lastSeq(t, s, sid)

	sendEvents(t, s, sid, map[string]any{"type": "user.interrupt", "session_thread_id": child}, userMessage("and then"))

	want := []string{"agent.tool_use", "agent.tool_result", "user.interrupt", "session.thread_status_idle",
		"agent.thread_message_received", "user.message"}
	if got := s.eventTypes(sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
	stampsRunForward(t, s, sid, seq)
}

// An answer is consumed on receipt, so it leads whatever the send's other
// events cause, whichever order they were posted in: a child's confirmation
// posted after the message that wakes the coordinator is still listed ahead of
// that wake, and its own resume follows.
func TestAnAnswerLeadsTheWakePostedBeforeIt(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	setThread(t, s, primary, "idle", `{"type":"end_turn"}`)
	child := gatedChild(t, s, sid)
	askID := lastEventOfType(t, s, sid, "agent.tool_use")["id"].(string)
	seq := lastSeq(t, s, sid)

	sendEvents(t, s, sid, userMessage("meanwhile"), confirm(askID, "allow", nil))

	want := []string{"agent.tool_use", "user.tool_confirmation", "session.status_running",
		"session.thread_status_running", "user.message", "session.thread_status_running"}
	if got := s.eventTypes(sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
	if got := lastEventOfType(t, s, sid, "session.thread_status_running")["session_thread_id"]; got != child {
		t.Errorf("last running = %v, want the confirmed child's resume", got)
	}
	stampsRunForward(t, s, sid, seq)
}

// A client's answer posted beside a redirect stays where it was consumed: ahead
// of the interrupt, its idle, the redirect's running pair and the message.
func TestAnAnswerPostedBesideARedirectLeadsIt(t *testing.T) {
	s := newTestServer(t)
	sid := selfHostedSession(t, s)
	useID := suspendedOnTool(t, s, sid, domain.EventAgentCustomToolUse)
	seq := lastSeq(t, s, sid)

	echo := sendEvents(t, s, sid, userMessage("instead"),
		map[string]any{"type": "user.custom_tool_result", "custom_tool_use_id": useID,
			"content": []any{map[string]any{"type": "text", "text": "done"}}},
		map[string]any{"type": "user.interrupt"})

	want := []string{"session.status_running", "session.thread_status_running", "user.message", "agent.custom_tool_use",
		"user.custom_tool_result", "user.interrupt", "session.thread_status_idle", "session.status_idle",
		"session.status_running", "session.thread_status_running", "user.message"}
	if got := s.eventTypes(sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
	if len(echo) != 3 || echo[0]["type"] != "user.message" || echo[1]["type"] != "user.custom_tool_result" ||
		echo[2]["type"] != "user.interrupt" {
		t.Errorf("echo = %v, want the posted order", echo)
	}
	if echo[1]["processed_at"] == nil {
		t.Errorf("answer echoed unprocessed; the interrupt consumes it in this commit")
	}
	stampsRunForward(t, s, sid, seq)
}

// An interrupt is processed after the results it synthesizes, and its stamp
// says so as its position does (#539): a child-scoped interrupt is stamped in
// the send's own commit, never ahead of the result listed before it.
func TestAnInterruptIsStampedAfterItsResults(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	setThread(t, s, primary, "running", "")
	child := insertChild(t, s, sid, "running")
	appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentToolUse, allowBashCall)
	appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentToolUse, allowBashCall)
	runningSession(t, s, sid)
	seq := lastSeq(t, s, sid)

	sendEvents(t, s, sid, map[string]any{"type": "user.interrupt", "session_thread_id": child})

	if got := typesAmong(s.eventTypes(sid), "agent.tool_result", "user.interrupt"); !sameStrings(got,
		[]string{"agent.tool_result", "agent.tool_result", "user.interrupt"}) {
		t.Fatalf("event log holds %v, want both results ahead of the interrupt", got)
	}
	stampsRunForward(t, s, sid, seq)
}

// customCall is a child's agent.custom_tool_use, cross-posted as the platform
// posts one a client must answer.
const customCall = `{"name":"decide","input":{},"session_thread_id":null}`

// customResult answers a custom tool call.
func customResult(useID string) map[string]any {
	return map[string]any{"type": "user.custom_tool_result", "custom_tool_use_id": useID,
		"content": []any{map[string]any{"type": "text", "text": "done"}}}
}

// An answer the send does not process is pending, so it goes to the tail with
// the other input no turn of this commit consumes, not where it was received:
// the ordered tool flow consumes a child's calls in the order the model made
// them, and a result for its second call waits behind the first. Here it is
// posted ahead of a message that wakes the coordinator, and is listed after
// the coordinator's running pair and the message the woken turn reads.
func TestAnAnswerQueuedBehindAnEarlierCallGoesToTheTail(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	setThread(t, s, domain.PrimaryThreadID(domain.ID(sid)).String(), "idle", `{"type":"end_turn"}`)
	child := insertChild(t, s, sid, "idle")
	first := appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentCustomToolUse, customCall)
	second := appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentCustomToolUse, customCall)
	setThread(t, s, child, "idle", `{"type":"requires_action","event_ids":["`+first+`","`+second+`"]}`)
	seq := lastSeq(t, s, sid)

	echo := sendEvents(t, s, sid, customResult(second), userMessage("meanwhile"))

	want := []string{"agent.custom_tool_use", "agent.custom_tool_use", "session.status_running",
		"session.thread_status_running", "user.message", "user.custom_tool_result"}
	if got := s.eventTypes(sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
	if echo[0]["processed_at"] != nil {
		t.Errorf("result echoed processed at %v, want null: it waits behind the first call", echo[0]["processed_at"])
	}
	stampsRunForward(t, s, sid, seq)
}

// Whether an answer is consumed is the ordered flow's to say, confirmation
// included: a result behind an allowed call waits for that call to run, so it
// is pending and goes to the tail; behind a denied call, which the denial
// answers, it is consumed and stays with the answers, after the denial's
// result, which the walk writes before it goes on to the next call.
func TestAnAnswerBehindAConfirmedCallIsPlacedByWhatTheConfirmationSays(t *testing.T) {
	for _, tc := range []struct {
		result    string
		want      []string
		processed bool
	}{
		{"allow", []string{"agent.tool_use", "agent.custom_tool_use", "user.tool_confirmation",
			"session.status_running", "session.thread_status_running", "user.message", "user.custom_tool_result",
			"session.thread_status_running"}, false},
		{"deny", []string{"agent.tool_use", "agent.custom_tool_use", "user.tool_confirmation", "agent.tool_result",
			"user.custom_tool_result", "session.status_running", "session.thread_status_running", "user.message",
			"session.thread_status_running"}, true},
	} {
		t.Run(tc.result, func(t *testing.T) {
			s := newTestServer(t)
			sid := eventsFixture(t, s)
			setThread(t, s, domain.PrimaryThreadID(domain.ID(sid)).String(), "idle", `{"type":"end_turn"}`)
			child := insertChild(t, s, sid, "idle")
			ask := appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentToolUse, askBashCall)
			custom := appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentCustomToolUse, customCall)
			setThread(t, s, child, "idle", `{"type":"requires_action","event_ids":["`+ask+`","`+custom+`"]}`)
			seq := lastSeq(t, s, sid)

			echo := sendEvents(t, s, sid, confirm(ask, tc.result, nil), customResult(custom), userMessage("meanwhile"))

			if got := wholeLogTypes(t, s, sid); !sameStrings(got, tc.want) {
				t.Fatalf("event log = %v, want %v", got, tc.want)
			}
			if got := echo[1]["processed_at"] != nil; got != tc.processed {
				t.Errorf("result processed = %v (%v), want %v: its place and its stamp must agree",
					got, echo[1]["processed_at"], tc.processed)
			}
			if echo[0]["processed_at"] == nil {
				t.Errorf("confirmation echoed unprocessed; the settlement consumes it in this commit")
			}
			stampsRunForward(t, s, sid, seq)
		})
	}
}

// A denial answers its call before the walk goes on to the next one, so its
// result comes before the answers it unlocks — and a thread's answers take
// the places they were received in, in the order its calls are processed, so
// the same holds whichever the client posted first.
func TestADenialIsListedAheadOfTheAnswersItUnlocks(t *testing.T) {
	for _, first := range []string{"deny", "result"} {
		t.Run(first+" first", func(t *testing.T) {
			s := newTestServer(t)
			sid := eventsFixture(t, s)
			setThread(t, s, domain.PrimaryThreadID(domain.ID(sid)).String(), "idle", `{"type":"end_turn"}`)
			child := insertChild(t, s, sid, "idle")
			ask := appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentToolUse, askBashCall)
			custom := appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentCustomToolUse, customCall)
			setThread(t, s, child, "idle", `{"type":"requires_action","event_ids":["`+ask+`","`+custom+`"]}`)
			seq := lastSeq(t, s, sid)

			answers := []map[string]any{confirm(ask, "deny", nil), customResult(custom)}
			if first == "result" {
				answers[0], answers[1] = answers[1], answers[0]
			}
			sendEvents(t, s, sid, answers...)

			want := []string{"agent.tool_use", "agent.custom_tool_use", "user.tool_confirmation", "agent.tool_result",
				"user.custom_tool_result", "session.status_running", "session.thread_status_running"}
			if got := wholeLogTypes(t, s, sid); !sameStrings(got, want) {
				t.Fatalf("event log = %v, want %v", got, want)
			}
			if n := countWhole(t, s, sid, "agent.tool_result"); n != 1 {
				t.Errorf("%d denial results, want exactly one", n)
			}
			stampsRunForward(t, s, sid, seq)
			pendingOnlyAtTail(t, s, sid, seq)
		})
	}
}

// An interrupt answers the calls it abandons with results stamped as they are
// written, so the settlement never walks those calls again: an answer to a
// later call of the same thread, posted beside the interrupt, is consumed in
// this commit and stays in its slot, not at the tail.
func TestAnAnswerBesideAnInterruptOfItsThreadIsConsumedInItsSlot(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	setThread(t, s, domain.PrimaryThreadID(domain.ID(sid)).String(), "running", "")
	child := insertChild(t, s, sid, "running")
	appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentCustomToolUse, customCall)
	second := appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentCustomToolUse, customCall)
	runningSession(t, s, sid)
	seq := lastSeq(t, s, sid)

	echo := sendEvents(t, s, sid, customResult(second), map[string]any{"type": "user.interrupt", "session_thread_id": child})

	want := []string{"agent.custom_tool_use", "agent.custom_tool_use", "user.custom_tool_result",
		"user.custom_tool_result", "user.interrupt", "session.thread_status_idle", "agent.thread_message_received"}
	if got := wholeLogTypes(t, s, sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
	evs, err := events.NewLog(s.pool).List(context.Background(), domain.ID(sid), events.ListQuery{Scope: events.ScopeAll})
	if err != nil {
		t.Fatal(err)
	}
	if got := evs[2].ID.String(); got != echo[0]["id"] {
		t.Errorf("row 2 is %s, want the posted result %v ahead of the one the interrupt synthesized", got, echo[0]["id"])
	}
	if echo[0]["processed_at"] == nil {
		t.Errorf("posted result echoed unprocessed; the interrupt's settlement consumes it")
	}
	stampsRunForward(t, s, sid, seq)
	pendingOnlyAtTail(t, s, sid, seq)
}

// pendingOnlyAtTail fails if a row written past seq is still pending while a
// processed row is listed after it: pending input goes to the tail.
func pendingOnlyAtTail(t *testing.T, s *tserver, sid string, seq int64) {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT type, processed_at IS NULL FROM events WHERE session_id = $1 AND seq > $2 ORDER BY seq`, sid, seq)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var pending string
	for rows.Next() {
		var typ string
		var isPending bool
		if err := rows.Scan(&typ, &isPending); err != nil {
			t.Fatal(err)
		}
		switch {
		case isPending && pending == "":
			pending = typ
		case !isPending && pending != "":
			t.Errorf("processed %s listed after the pending %s", typ, pending)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// countWhole counts a type across the whole log, a child's own rows included.
func countWhole(t *testing.T, s *tserver, sid, typ string) int {
	t.Helper()
	n := 0
	for _, got := range wholeLogTypes(t, s, sid) {
		if got == typ {
			n++
		}
	}
	return n
}

// The one placement out of reach, pinned rather than changed
// (docs/DIVERGENCES.md, the primary thread's entry): an answer that resumes
// the primary moves it in the settlement that runs after the send's append, so
// a message posted beside it, which the resumed turn consumes, is listed ahead
// of the resume's running pair. The rule would list it after.
func TestAMessageBesideAnAnswerThatResumesThePrimaryPrecedesTheResume(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	useID := appendOn(t, s, sid, "", false, domain.EventAgentCustomToolUse, customCall)
	setThread(t, s, domain.PrimaryThreadID(domain.ID(sid)).String(), "idle",
		`{"type":"requires_action","event_ids":["`+useID+`"]}`)

	sendEvents(t, s, sid, customResult(useID), userMessage("and also"))

	want := []string{"agent.custom_tool_use", "user.custom_tool_result", "user.message",
		"session.status_running", "session.thread_status_running"}
	if got := s.eventTypes(sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
}

// A thread two interrupts of one send both reach is ended by the first of them
// received: its results come before that interrupt and its idle after it, and
// the later one, which finds the thread already stopped, settles nothing. Here
// the session-wide interrupt arrives first, so the child's ending belongs to it
// and not to the child-scoped interrupt that follows.
func TestTheFirstInterruptReceivedEndsTheThread(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	setThread(t, s, primary, "running", "")
	child := insertChild(t, s, sid, "running")
	appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentToolUse, allowBashCall)
	runningSession(t, s, sid)
	seq := lastSeq(t, s, sid)

	sendEvents(t, s, sid, map[string]any{"type": "user.interrupt"},
		map[string]any{"type": "user.interrupt", "session_thread_id": child})

	want := []string{"agent.tool_use", "agent.tool_result", "user.interrupt",
		"session.thread_status_idle", "session.thread_status_idle", "session.status_idle", "user.interrupt"}
	if got := wholeLogTypes(t, s, sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
	_, list := s.do("GET", "/v1/sessions/"+sid+"/events", nil)
	if evs := listData(t, list); evs[len(evs)-1]["session_thread_id"] != child {
		t.Errorf("last event = %v, want the redundant child-scoped interrupt", evs[len(evs)-1])
	}
	stampsRunForward(t, s, sid, seq)
}

// The same two interrupts posted the other way round: the child-scoped one is
// first, so it ends the child, and the session-wide one ends the primary.
func TestTheFirstInterruptReceivedEndsTheThreadScopedFirst(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	setThread(t, s, primary, "running", "")
	child := insertChild(t, s, sid, "running")
	appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentToolUse, allowBashCall)
	runningSession(t, s, sid)
	seq := lastSeq(t, s, sid)

	sendEvents(t, s, sid, map[string]any{"type": "user.interrupt", "session_thread_id": child},
		map[string]any{"type": "user.interrupt"})

	want := []string{"agent.tool_use", "agent.tool_result", "user.interrupt", "session.thread_status_idle",
		"user.interrupt", "session.thread_status_idle", "session.status_idle"}
	if got := wholeLogTypes(t, s, sid); !sameStrings(got, want) {
		t.Fatalf("event log = %v, want %v", got, want)
	}
	stampsRunForward(t, s, sid, seq)
}

// Item 2's residual, deliberately (docs/DIVERGENCES.md, "Session threads — a
// child's resume of an idle session"): a message that wakes the coordinator
// while a child keeps the session running writes the coordinator's running
// event alone. The fold never left running, so no session.status_running is
// due; the reference, whose session reads idle while a child works, would
// write the pair.
func TestAWakeUnderARunningChildWritesTheThreadEventAlone(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	primary := domain.PrimaryThreadID(domain.ID(sid)).String()
	setThread(t, s, primary, "idle", `{"type":"end_turn"}`)
	insertChild(t, s, sid, "running")
	runningSession(t, s, sid)

	sendEvents(t, s, sid, userMessage("one more thing"))

	if got := s.eventTypes(sid); !sameStrings(got, []string{"session.thread_status_running", "user.message"}) {
		t.Fatalf("event log = %v, want the coordinator's running event and then the message", got)
	}
	if got := s.threadStatus(t, primary); got != "running" {
		t.Errorf("coordinator = %q, want woken", got)
	}
	if got := s.sessionStatus(sid); got != "running" {
		t.Errorf("session = %q, want running", got)
	}
}
