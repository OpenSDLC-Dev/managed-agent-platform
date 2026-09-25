package events_test

import (
	"slices"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// A tiny log builder for the consumption-order tests: each row gets the next
// seq, a readable id, and the thread it is written on.
type logBuilder struct {
	evs []domain.Event
}

func (b *logBuilder) add(id string, typ domain.EventType, thread domain.ID, body string) *logBuilder {
	if body == "" {
		body = "{}"
	}
	b.evs = append(b.evs, domain.Event{
		ID: domain.ID(id), Seq: int64(len(b.evs) + 1), Type: typ, ThreadID: thread, Body: []byte(body),
	})
	return b
}

// start opens a request on the thread; end closes the one named by start id.
func (b *logBuilder) start(id string, thread domain.ID) *logBuilder {
	return b.add(id, domain.EventSpanModelRequestStart, thread, "")
}

func (b *logBuilder) end(id, startID string, thread domain.ID) *logBuilder {
	return b.add(id, domain.EventSpanModelRequestEnd, thread,
		`{"is_error":false,"model_request_start_id":"`+startID+`","model_usage":{}}`)
}

func ids(evs []domain.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = string(ev.ID)
	}
	return out
}

func wantOrder(t *testing.T, got []domain.Event, want ...string) {
	t.Helper()
	if !slices.Equal(ids(got), want) {
		t.Errorf("order = %v\n want %v", ids(got), want)
	}
}

const (
	evUser     = domain.EventUserMessage
	evAgentMsg = domain.EventAgentMessage
	evToolUse  = domain.EventAgentToolUse
	evToolRes  = domain.EventAgentToolResult
	evReceived = domain.EventAgentThreadMessageReceived
	evOutcome  = domain.EventUserDefineOutcome
)

// An input that lands while a request is in flight was not in that request:
// it moves after the request's end, where the next request consumed it — the
// reference lists it there (2026-09-03 batch2 conflict.turn3 idx 18), and an
// end_turn reply therefore no longer ends the chained request on an
// assistant turn.
func TestConsumptionOrderEndTurnWindow(t *testing.T) {
	b := (&logBuilder{}).
		add("one", evUser, "", "").start("s1", "").
		add("two", evUser, "", "").
		add("reply", evAgentMsg, "", "").end("e1", "s1", "").
		start("s2", "")
	wantOrder(t, events.ConsumptionOrder(b.evs), "one", "s1", "reply", "e1", "two", "s2")
}

// On a tool turn the input moves after the call and its result is written
// after it; replay merges the two into one user turn, results first
// (2026-09-02 batch2 sessT idx 78: after agent.tool_result, before the next
// start).
func TestConsumptionOrderToolWindow(t *testing.T) {
	b := (&logBuilder{}).
		add("one", evUser, "", "").start("s1", "").
		add("two", evUser, "", "").
		add("call", evToolUse, "", "").end("e1", "s1", "").
		add("result", evToolRes, "", "").start("s2", "")
	wantOrder(t, events.ConsumptionOrder(b.evs), "one", "s1", "call", "e1", "two", "result", "s2")
}

// A delegated settle commits the end and then the inline answers in one
// batch: the input is released at the end, ahead of those answers on the
// log, and replay still sorts the results first in the merged user turn.
func TestConsumptionOrderDelegatedSettle(t *testing.T) {
	b := (&logBuilder{}).
		add("one", evUser, "", "").start("s1", "").
		add("report", evReceived, "", "").
		add("call", evToolUse, "", "").end("e1", "s1", "").
		add("answer", evToolRes, "", "").add("spawned", domain.EventAgentThreadMessageSent, "", "")
	wantOrder(t, events.ConsumptionOrder(b.evs), "one", "s1", "call", "e1", "report", "answer", "spawned")
}

// A start with no end — a crash, a lost lease, an interrupted request — is
// closed by the thread's next start: the retried request saw what the dead
// one held, because it was below the retry's start.
func TestConsumptionOrderDanglingStart(t *testing.T) {
	b := (&logBuilder{}).
		add("one", evUser, "", "").start("s1", "").
		add("two", evUser, "", "").
		start("s2", "").add("reply", evAgentMsg, "", "").end("e2", "s2", "")
	wantOrder(t, events.ConsumptionOrder(b.evs), "one", "s1", "two", "s2", "reply", "e2")
}

// A redirect after a dangling start — the interrupt, its idle pair, the
// running pair, the new message — keeps its seq order: every row the
// redirect wrote is emitted as it stands except the message, which is
// released just before the next start, where it already was.
func TestConsumptionOrderRedirectAfterDanglingStart(t *testing.T) {
	b := (&logBuilder{}).
		add("one", evUser, "", "").start("s1", "").
		add("stop", domain.EventUserInterrupt, "", "").
		add("idle", domain.EventSessionStatusIdle, "", "").
		add("running", domain.EventSessionStatusRunning, "", "").
		add("redirect", evUser, "", "").
		start("s2", "")
	wantOrder(t, events.ConsumptionOrder(b.evs), "one", "s1", "stop", "idle", "running", "redirect", "s2")
}

// The three consumed inputs are held; two in one window keep their seq order.
func TestConsumptionOrderHoldsEveryConsumedInput(t *testing.T) {
	b := (&logBuilder{}).
		start("s1", "").
		add("msg", evUser, "", "").
		add("goal", evOutcome, "", "").
		add("report", evReceived, "", "").
		add("reply", evAgentMsg, "", "").end("e1", "s1", "")
	wantOrder(t, events.ConsumptionOrder(b.evs), "s1", "reply", "e1", "msg", "goal", "report")
	for _, typ := range []domain.EventType{evUser, evOutcome, evReceived} {
		if !events.ConsumedInput(typ) {
			t.Errorf("ConsumedInput(%q) = false, want true", typ)
		}
	}
}

// Every set built on what a request consumes derives from
// domain.ConsumedInputs (#793), each with at most one stated extra: the
// consumption rule holds exactly those, a start stamps them and a
// system.message, and the brain's pending probe is pinned beside its own
// derivation. Pinning each set whole makes a drift from the source — a
// hand-kept copy, or a source changed without the sets being re-read — fail
// here.
func TestTheConsumedInputSetsShareOneSource(t *testing.T) {
	consumed := []domain.EventType{evUser, evOutcome, evReceived}
	if !slices.Equal(domain.ConsumedInputs, consumed) {
		t.Fatalf("domain.ConsumedInputs = %v, want %v", domain.ConsumedInputs, consumed)
	}
	for _, typ := range consumed {
		if !events.ConsumedInput(typ) {
			t.Errorf("ConsumedInput(%q) = false", typ)
		}
	}
	want := []string{"user.message", "user.define_outcome", "agent.thread_message_received", "system.message"}
	if !slices.Equal(events.RequestInputTypes, want) {
		t.Errorf("RequestInputTypes = %v, want %v", events.RequestInputTypes, want)
	}
	// What a start stamps must be written unprocessed, or it is stamped at
	// write and no start ever consumes it.
	for _, typ := range events.RequestInputTypes {
		if !domain.EventType(typ).StampedOnConsumption() {
			t.Errorf("%q is stamped at write, so no request's start can consume it", typ)
		}
	}
}

// Nothing else is held: a system.message goes to the system slot, an
// interrupt and a confirmation are not conversation, a tool result cannot be
// in a window, and a grader verdict is written between requests.
func TestConsumptionOrderHoldsNothingElse(t *testing.T) {
	never := []domain.EventType{
		domain.EventSystemMessage, domain.EventUserInterrupt, domain.EventUserToolConfirm,
		domain.EventUserToolResult, domain.EventUserCustomToolRes, evToolRes,
		domain.EventSpanOutcomeEvalEnd, domain.EventSessionStatusIdle, domain.EventAgentThreadMessageSent,
	}
	for _, typ := range never {
		if events.ConsumedInput(typ) {
			t.Errorf("ConsumedInput(%q) = true, want false", typ)
		}
		b := (&logBuilder{}).start("s1", "").add("x", typ, "", "").add("reply", evAgentMsg, "", "").end("e1", "s1", "")
		wantOrder(t, events.ConsumptionOrder(b.evs), "s1", "x", "reply", "e1")
	}
}

// An input between windows — posted while nothing was in flight — stays put.
func TestConsumptionOrderInputBetweenWindows(t *testing.T) {
	b := (&logBuilder{}).
		add("one", evUser, "", "").start("s1", "").add("reply", evAgentMsg, "", "").end("e1", "s1", "").
		add("two", evUser, "", "").start("s2", "")
	wantOrder(t, events.ConsumptionOrder(b.evs), "one", "s1", "reply", "e1", "two", "s2")
}

// Windows are per thread: the primary's open request never holds a child's
// row and a child's never holds the primary's, and each window releases at
// its own end.
func TestConsumptionOrderIsPerThread(t *testing.T) {
	const child domain.ID = "sthr_child"
	b := (&logBuilder{}).
		start("p1", "").
		add("task", evReceived, child, "").
		start("c1", child).
		add("msg", evUser, "", "").
		add("more", evReceived, child, "").
		add("creply", evAgentMsg, child, "").end("ce1", "c1", child).
		add("preply", evAgentMsg, "", "").end("pe1", "p1", "")
	wantOrder(t, events.ConsumptionOrder(b.evs),
		"p1", "task", "c1", "creply", "ce1", "more", "preply", "pe1", "msg")
}

// Only the end that names the open start closes it; an end naming another
// start leaves the window open, and one whose payload cannot say closes it.
func TestConsumptionOrderEndMatching(t *testing.T) {
	b := (&logBuilder{}).
		start("s1", "").add("msg", evUser, "", "").
		end("stale", "s0", "").
		add("reply", evAgentMsg, "", "").end("e1", "s1", "")
	wantOrder(t, events.ConsumptionOrder(b.evs), "s1", "stale", "reply", "e1", "msg")

	b = (&logBuilder{}).
		start("s1", "").add("msg", evUser, "", "").
		add("e1", domain.EventSpanModelRequestEnd, "", `not json`).
		add("after", evAgentMsg, "", "")
	wantOrder(t, events.ConsumptionOrder(b.evs), "s1", "e1", "msg", "after")

	b = (&logBuilder{}).
		start("s1", "").add("msg", evUser, "", "").
		add("e1", domain.EventSpanModelRequestEnd, "", `{"is_error":true}`).
		add("after", evAgentMsg, "", "")
	wantOrder(t, events.ConsumptionOrder(b.evs), "s1", "e1", "msg", "after")
}

// A window still open when the log ends — a request in flight right now — is
// flushed at the end, so nothing is lost, and every thread's held inputs come
// out merged by seq.
func TestConsumptionOrderFlushesOpenWindows(t *testing.T) {
	const child domain.ID = "sthr_child"
	b := (&logBuilder{}).
		start("p1", "").start("c1", child).
		add("a", evUser, "", "").add("b", evReceived, child, "").add("c", evUser, "", "")
	wantOrder(t, events.ConsumptionOrder(b.evs), "p1", "c1", "a", "b", "c")
}

// The function reorders a copy: the grader's watermark and outcome lookups
// read the same slice afterwards and must see it in seq order.
func TestConsumptionOrderLeavesItsInputAlone(t *testing.T) {
	b := (&logBuilder{}).
		add("one", evUser, "", "").start("s1", "").add("two", evUser, "", "").
		add("reply", evAgentMsg, "", "").end("e1", "s1", "")
	before := ids(b.evs)
	_ = events.ConsumptionOrder(b.evs)
	if !slices.Equal(ids(b.evs), before) {
		t.Errorf("input reordered to %v", ids(b.evs))
	}
	if got := events.ConsumptionOrder(nil); len(got) != 0 {
		t.Errorf("ConsumptionOrder(nil) = %v", got)
	}
}

// The streaming form is the same rule: pushing a log in any chunking and
// flushing at the end yields exactly ConsumptionOrder's result.
func TestConsumptionOrdererStreamsTheSameOrder(t *testing.T) {
	const child domain.ID = "sthr_child"
	b := (&logBuilder{}).
		add("one", evUser, "", "").start("s1", "").add("two", evUser, "", "").
		start("c1", child).add("task", evReceived, child, "").
		add("call", evToolUse, "", "").end("e1", "s1", "").add("result", evToolRes, "", "").
		start("s2", "").add("three", evOutcome, "", "").add("reply", evAgentMsg, "", "").end("e2", "s2", "").
		start("s3", "").add("four", evUser, "", "")
	want := ids(events.ConsumptionOrder(b.evs))
	for size := 1; size <= len(b.evs); size++ {
		var o events.ConsumptionOrderer
		var got []domain.Event
		for i := 0; i < len(b.evs); i += size {
			for _, ev := range b.evs[i:min(i+size, len(b.evs))] {
				got = append(got, o.Push(ev)...)
			}
		}
		got = append(got, o.Flush()...)
		if !slices.Equal(ids(got), want) {
			t.Errorf("chunk %d: %v\n want %v", size, ids(got), want)
		}
	}
}

// HeldBytes is what the held inputs weigh, so a streaming caller can bound
// its memory; a flush releases them and resets it. An early flush degrades
// the open window to seq order: what was held comes out now, and the rest of
// that window is emitted as it arrives.
func TestConsumptionOrdererHeldBytes(t *testing.T) {
	var o events.ConsumptionOrderer
	if got := o.Push(domain.Event{ID: "s1", Seq: 1, Type: domain.EventSpanModelRequestStart, Body: []byte("{}")}); len(got) != 1 {
		t.Fatalf("start emitted %v", ids(got))
	}
	if got := o.Push(domain.Event{ID: "m1", Seq: 2, Type: evUser, Body: []byte("0123456789")}); len(got) != 0 {
		t.Fatalf("held input emitted %v", ids(got))
	}
	o.Push(domain.Event{ID: "m2", Seq: 3, Type: evUser, Body: []byte("01234")})
	if got := o.HeldBytes(); got != 15 {
		t.Errorf("HeldBytes = %d, want 15", got)
	}
	wantOrder(t, o.Flush(), "m1", "m2")
	if got := o.HeldBytes(); got != 0 {
		t.Errorf("HeldBytes after flush = %d, want 0", got)
	}
	wantOrder(t, o.Push(domain.Event{ID: "m3", Seq: 4, Type: evUser, Body: []byte("{}")}), "m3")
	wantOrder(t, o.Push(domain.Event{ID: "e1", Seq: 5, Type: domain.EventSpanModelRequestEnd,
		Body: []byte(`{"model_request_start_id":"s1"}`)}), "e1")
}
