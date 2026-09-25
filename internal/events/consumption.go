package events

import (
	"cmp"
	"encoding/json"
	"slices"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// Consumption order (#793 item 4). The log keeps an input at its receipt seq,
// but an input that lands while one of its thread's model requests is in
// flight — after that request's span.model_request_start, before its
// span.model_request_end — was not in that request: the thread's next request
// consumed it, after the in-flight request's reply and results. The grader's
// call on the primary is such a window too: a message posted while it ran
// never reached the verdict, which replay renders as the revision feedback. That is where
// the reference lists it (2026-09-02 batch2 sessT idx 78; 2026-09-03 batch2
// conflict.turn3 idx 18, dead-five-turns idx 30) and where it stamps its
// processed_at, 1 µs before the consuming start (StartModelRequestOn). What
// the model reads — the brain's replay, the grader's and the dream's
// transcripts — follows that order; the list and the stream keep seq.
//
// The rule is a pure function of a thread's rows, so every replay of a log
// rebuilds the same order. Its one premise is that a request consumes every
// row of its thread below its start that no earlier start consumed, which the
// brain makes true by reading its history only after its start commits,
// bounded by it (requestHistory). A log written before #793 can break it: an
// input that landed between an older brain's history read and its start was
// in the next request only, yet replays with the request whose start it
// precedes — one request early. That window was milliseconds wide, and no
// special case is kept for it (docs/plan/56_processing-order.md).

// ConsumedInput reports whether t is an input a model request consumes
// (domain.ConsumedInputs), and so one a consumption window can hold. Nothing
// else moves — a system.message goes to the system slot wherever it sits, an
// interrupt or a confirmation is not conversation, no tool result can land
// inside a window (its call is not committed until the request ends), and a
// grader verdict is written between requests.
func ConsumedInput(t domain.EventType) bool {
	return slices.Contains(domain.ConsumedInputs, t)
}

// RequestInputTypes are the rows a model request's start stamps processed
// (#793; AppendOptions.Consume): domain.ConsumedInputs, and a system.message.
// That one extra is the only input a request reads that is not a consumed
// input: the request folds it into its system prompt wherever it sits, so no
// window holds it and replay never moves it, but it is read, and the start
// that reads it stamps it. An answer — a confirmation or a tool result — is
// stamped where its thread's ordered processor takes it, and an interrupt is
// no input a request reads.
var RequestInputTypes = func() []string {
	out := make([]string, 0, len(domain.ConsumedInputs)+1)
	for _, t := range domain.ConsumedInputs {
		out = append(out, string(t))
	}
	return append(out, string(domain.EventSystemMessage))
}()

// ConsumptionOrderer is the streaming form of ConsumptionOrder, for a reader
// that pages the log rather than holding it (RenderDream). Windows are keyed
// by the row's own thread, so a session-wide read never lets one thread's
// request hold another's input. The zero value is ready to use.
type ConsumptionOrderer struct {
	open map[domain.ID]window // thread -> the call in flight on it
	held map[domain.ID][]domain.Event
	// closing marks a thread whose window has closed but whose held inputs
	// wait out the request's own results, written after its end.
	closing   map[domain.ID]bool
	heldBytes int
}

// window is one call in flight on a thread: a model request, or the grader's
// call on the primary, which a message posted meanwhile never reached either.
type window struct {
	start domain.ID
	end   domain.EventType // the row that closes it
	key   string           // the end's payload field naming the start
}

// windowOf is the window a row opens, if it opens one.
func windowOf(ev domain.Event) (window, bool) {
	switch ev.Type {
	case domain.EventSpanModelRequestStart:
		return window{ev.ID, domain.EventSpanModelRequestEnd, "model_request_start_id"}, true
	case domain.EventSpanOutcomeEvalStart:
		return window{ev.ID, domain.EventSpanOutcomeEvalEnd, "outcome_evaluation_start_id"}, true
	}
	return window{}, false
}

// Push takes the next row in seq order and appends the rows to emit now to
// out, in consumption order, returning the extended slice. It is append's
// contract, so a caller that drains each result before the next push can
// hand the same buffer back (buf[:0]) and a row costs no allocation of its
// own:
//   - a span.model_request_start, or the span.outcome_evaluation_start of the
//     grader's call, releases its thread's held inputs ahead of itself, then
//     opens a window. A start that never ended (a crash, a lost lease, an
//     interrupted request) is closed this way: the retried call saw what the
//     dead one held.
//   - a consumed input on a thread with a window open is held.
//   - the end that names the open start (model_request_start_id, or
//     outcome_evaluation_start_id) closes the window. An end naming another
//     start leaves the window open; one whose payload cannot say which start
//     it closes closes the open one, since a thread runs one call at a time.
//     What the window held is not released at the end but after the
//     request's own results that follow it — a delegated settle or an
//     executor answers the request's calls after its end — and ahead of the
//     thread's first row that is not one: where the next request consumed it,
//     and where the reference lists it (2026-09-02 batch2 sessT idx 78).
//   - everything else is emitted as it stands.
//
// It never appends a row it has not been pushed, so the rows appended stay
// at or behind the rows pushed — what lets ConsumptionOrder reorder a log in
// place.
func (o *ConsumptionOrderer) Push(out []domain.Event, ev domain.Event) []domain.Event {
	t := ev.ThreadID
	if o.closing[t] {
		if isResult(ev.Type) {
			return append(out, ev)
		}
		delete(o.closing, t)
		out = o.release(out, t)
	}
	w, open := o.open[t]
	if opened, ok := windowOf(ev); ok {
		out = append(o.release(out, t), ev)
		if o.open == nil {
			o.open = map[domain.ID]window{}
		}
		o.open[t] = opened
		return out
	}
	switch {
	case open && ConsumedInput(ev.Type):
		if o.held == nil {
			o.held = map[domain.ID][]domain.Event{}
		}
		o.held[t] = append(o.held[t], ev)
		o.heldBytes += heldWeight(ev)
		return out
	case open && ev.Type == w.end && closes(ev, w):
		delete(o.open, t)
		if len(o.held[t]) > 0 {
			if o.closing == nil {
				o.closing = map[domain.ID]bool{}
			}
			o.closing[t] = true
		}
	}
	return append(out, ev)
}

// isResult reports whether t answers a tool call — the rows a request's
// settlement, or the driver that ran its calls, writes after its end.
func isResult(t domain.EventType) bool {
	switch t {
	case domain.EventAgentToolResult, domain.EventAgentMCPToolResult,
		domain.EventUserToolResult, domain.EventUserCustomToolRes:
		return true
	}
	return false
}

// Flush appends every held input to out, all threads merged by seq, and
// closes every window. At the end of a log it is what a request still in
// flight held. Called early, to bound memory (HeldBytes), it degrades the
// windows it closes to seq order: what was held comes out now, and the rest
// of each window is emitted as it arrives.
func (o *ConsumptionOrderer) Flush(out []domain.Event) []domain.Event {
	from := len(out)
	for _, evs := range o.held {
		out = append(out, evs...)
	}
	slices.SortFunc(out[from:], func(a, b domain.Event) int { return cmp.Compare(a.Seq, b.Seq) })
	o.open, o.held, o.closing, o.heldBytes = nil, nil, nil, 0
	return out
}

// HeldBytes estimates what the inputs held back so far weigh: each one's
// body, and heldRowOverhead for the event it is held as.
func (o *ConsumptionOrderer) HeldBytes() int { return o.heldBytes }

// heldRowOverhead is what HeldBytes charges a held row beside its body: the
// domain.Event it is held as (128 bytes on a 64-bit platform) and the small
// allocations a row read from the log carries with it — its id, its type and
// its processed_at — rounded up for the held slice's spare capacity. An
// estimate rather than a measure, it keeps a window of many empty inputs as
// bounded as a window of a few large ones.
const heldRowOverhead = 256

func heldWeight(ev domain.Event) int { return len(ev.Body) + heldRowOverhead }

// release appends thread t's held inputs to out and forgets them.
func (o *ConsumptionOrderer) release(out []domain.Event, t domain.ID) []domain.Event {
	for _, ev := range o.held[t] {
		o.heldBytes -= heldWeight(ev)
		out = append(out, ev)
	}
	delete(o.held, t)
	return out
}

// closes reports whether an end closes window w. Every end this platform
// writes names its start (ModelRequest.EndEvent, OutcomeEvaluation.EndEvent);
// one that names none, or does not decode, is taken to close the open window
// rather than to hold its inputs forever.
func closes(end domain.Event, w window) bool {
	var p map[string]json.RawMessage
	if json.Unmarshal(end.Body, &p) != nil {
		return true
	}
	var startID domain.ID
	if json.Unmarshal(p[w.key], &startID) != nil || startID == "" {
		return true
	}
	return startID == w.start
}

// ConsumptionOrder appends history, a log in seq order, to dst in
// consumption order and returns the result. dst may be history[:0], which
// reorders history in place without a copy: Push never appends a row before
// it has read it, so the writes stay at or behind the reads. A caller that
// still reads history by position afterwards (a watermark, an outcome's
// start) passes a slice of its own instead and keeps history in seq order.
func ConsumptionOrder(dst, history []domain.Event) []domain.Event {
	var o ConsumptionOrderer
	for _, ev := range history {
		dst = o.Push(dst, ev)
	}
	return o.Flush(dst)
}
