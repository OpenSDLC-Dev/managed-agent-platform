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
// consumed it, after the in-flight request's reply and results. That is where
// the reference lists it (2026-09-02 batch2 sessT idx 78; 2026-09-03 batch2
// conflict.turn3 idx 18, dead-five-turns idx 30) and where it stamps its
// processed_at, 1 µs before the consuming start (StartModelRequestOn). What
// the model reads — the brain's replay, the grader's and the dream's
// transcripts — follows that order; the list and the stream keep seq.
//
// The rule is a pure function of a thread's rows, so every replay of a log
// rebuilds the same order. Its one premise is the replay snapshot's
// invariant: a request consumes every row of its thread below its start that
// no earlier start consumed (the brain's topUpHistory closes the gap between
// its history read and the start).

// ConsumedInput reports whether t is an input a model request consumes, and
// so one a consumption window can hold: user.message, user.define_outcome and
// agent.thread_message_received. Nothing else moves — a system.message goes
// to the system slot wherever it sits, an interrupt or a confirmation is not
// conversation, no tool result can land inside a window (its call is not
// committed until the request ends), and a grader verdict is written between
// requests.
func ConsumedInput(t domain.EventType) bool {
	switch t {
	case domain.EventUserMessage, domain.EventUserDefineOutcome, domain.EventAgentThreadMessageReceived:
		return true
	}
	return false
}

// ConsumptionOrderer is the streaming form of ConsumptionOrder, for a reader
// that pages the log rather than holding it (RenderDream). Windows are keyed
// by the row's own thread, so a session-wide read never lets one thread's
// request hold another's input. The zero value is ready to use.
type ConsumptionOrderer struct {
	open      map[domain.ID]domain.ID      // thread -> start id of its request in flight
	held      map[domain.ID][]domain.Event // thread -> inputs that landed in that window, seq order
	heldBytes int
}

// Push takes the next row in seq order and returns the rows to emit now, in
// consumption order:
//   - a span.model_request_start releases its thread's held inputs ahead of
//     itself, then opens a window. A start that never ended (a crash, a lost
//     lease, an interrupted request) is closed this way: the retried request
//     saw what the dead one held.
//   - a consumed input on a thread with a window open is held.
//   - the span.model_request_end that names the open start (its
//     model_request_start_id) closes the window: the end, then what it held.
//     An end naming another start leaves the window open; one whose payload
//     cannot say which start it closes closes the open one, since a thread
//     runs one request at a time.
//   - everything else is emitted as it stands.
func (o *ConsumptionOrderer) Push(ev domain.Event) []domain.Event {
	t := ev.ThreadID
	start, open := o.open[t]
	switch {
	case ev.Type == domain.EventSpanModelRequestStart:
		out := append(o.release(t), ev)
		if o.open == nil {
			o.open = map[domain.ID]domain.ID{}
		}
		o.open[t] = ev.ID
		return out
	case open && ConsumedInput(ev.Type):
		if o.held == nil {
			o.held = map[domain.ID][]domain.Event{}
		}
		o.held[t] = append(o.held[t], ev)
		o.heldBytes += len(ev.Body)
		return nil
	case open && ev.Type == domain.EventSpanModelRequestEnd && endsRequest(ev, start):
		delete(o.open, t)
		return append([]domain.Event{ev}, o.release(t)...)
	}
	return []domain.Event{ev}
}

// Flush releases every held input, all threads merged by seq, and closes
// every window. At the end of a log it is what a request still in flight
// held. Called early, to bound memory (HeldBytes), it degrades the windows
// it closes to seq order: what was held comes out now, and the rest of each
// window is emitted as it arrives.
func (o *ConsumptionOrderer) Flush() []domain.Event {
	var out []domain.Event
	for _, evs := range o.held {
		out = append(out, evs...)
	}
	slices.SortFunc(out, func(a, b domain.Event) int { return cmp.Compare(a.Seq, b.Seq) })
	o.open, o.held, o.heldBytes = nil, nil, 0
	return out
}

// HeldBytes is the body size of the inputs held back so far.
func (o *ConsumptionOrderer) HeldBytes() int { return o.heldBytes }

func (o *ConsumptionOrderer) release(t domain.ID) []domain.Event {
	out := o.held[t]
	for _, ev := range out {
		o.heldBytes -= len(ev.Body)
	}
	delete(o.held, t)
	return out
}

// endsRequest reports whether a span.model_request_end closes the request
// that start opened. Every end this platform writes names its start
// (ModelRequest.EndEvent); one that names none, or does not decode, is
// taken to close the open request rather than to hold its inputs forever.
func endsRequest(end domain.Event, start domain.ID) bool {
	var p struct {
		StartID domain.ID `json:"model_request_start_id"`
	}
	if json.Unmarshal(end.Body, &p) != nil || p.StartID == "" {
		return true
	}
	return p.StartID == start
}

// ConsumptionOrder returns history, a log in seq order, in consumption order.
// It builds a new slice and leaves history as it was: callers that also read
// the log by position (a watermark, an outcome's start) keep reading seq.
func ConsumptionOrder(history []domain.Event) []domain.Event {
	var o ConsumptionOrderer
	out := make([]domain.Event, 0, len(history))
	for _, ev := range history {
		out = append(out, o.Push(ev)...)
	}
	return append(out, o.Flush()...)
}
