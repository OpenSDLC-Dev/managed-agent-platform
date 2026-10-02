package events

import (
	"context"
	"fmt"
	"slices"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// AwaitedResponse is one tool call a thread waits on a client's or a
// worker's response to: a confirmation, for an ask-gated call nobody has
// confirmed, a result, for a custom call or a self_hosted worker's built-in
// nobody has answered, or both.
type AwaitedResponse struct {
	ID           domain.ID
	Confirmation bool
	Result       bool
}

// ThreadWaits reads a thread's calls once for a caller that already holds the
// session's environment kind, and returns both the flow ThreadToolFlow would
// (the thread's resting place) and the responses it awaits: ToolFlow.Pending,
// less what was received already. An answer queued behind an earlier call is
// not awaited, nor is a call whose received confirmation denies it, the denial
// being its answer — reachable while the denial waits behind an earlier call
// for the walk that writes its result.
func ThreadWaits(ctx context.Context, q Querier, sid, tid domain.ID, kind string, platformOwned func(string) bool) (ToolFlow, []AwaitedResponse, error) {
	calls, err := threadCallsOf(ctx, q, sid, tid)
	if err != nil {
		return ToolFlow{}, nil, err
	}
	var awaited []AwaitedResponse
	for _, c := range calls {
		if c.confirmationID != "" && denies(c.confirmation) {
			continue
		}
		a := AwaitedResponse{ID: c.id,
			Confirmation: c.asks() && c.confirmationID == "",
			Result:       c.external(kind, platformOwned) && c.resultID == ""}
		if a.Confirmation || a.Result {
			awaited = append(awaited, a)
		}
	}
	return summarizeTools(calls, kind, platformOwned), awaited, nil
}

// WhileAwaitingError refuses a user.message or a user.define_outcome posted
// while the primary thread still waits on responses, in the reference's words
// (2026-09-12-followups budget-runtime.json #4 `rec.budget.message-over-budget`,
// a user.message while one agent.custom_tool_use awaited its result). Only
// that sentence's single-id rendering was recorded; IDs render as Go's %v
// renders a slice, space-separated, which is the inference for several.
type WhileAwaitingError struct {
	Type  domain.EventType
	Index int
	IDs   []domain.ID
}

func (e *WhileAwaitingError) Error() string {
	return fmt.Sprintf("Invalid %s event at events[%d]: waiting on responses to events %v; "+
		"only `user.tool_confirmation`, `user.custom_tool_result`, `user.tool_result`, or `user.interrupt` may be sent "+
		"(a `system.message` may trail a tool result)", e.Type, e.Index, e.IDs)
}

// CheckWhileAwaiting refuses a send whose user.message or user.define_outcome
// the primary thread could not read because it would still await a response
// once the send's own answers are in. Its caller applies it to a primary
// resting idle on those responses, the state the reference was recorded
// refusing in, where this platform used to queue the input behind the call.
// It returns a *WhileAwaitingError naming the first such event and what the
// primary would still await (ThreadWaits, less the send's answers). The
// answers count wherever the send carries them, a
// message posted ahead of the answer that frees the primary included, since
// the send is settled as one: a result or a denial answers its call, an allow
// confirms it — a custom or a worker call then awaiting its result still —
// and an interrupt reaching the primary, reachesPrimary(i), answers every
// call. A system.message is never refused itself: it may follow only a
// message or a result (NormalizeInbound), and a refused message ahead of it
// refuses the send first.
func CheckWhileAwaiting(awaited []AwaitedResponse, posted []NewEvent, reachesPrimary func(i int) bool) error {
	awaited = slices.Clone(awaited)
	first := -1
	for i, ev := range posted {
		switch ev.Type {
		case domain.EventUserToolResult, domain.EventUserCustomToolRes:
			ref := domain.ID(answerRef(ev))
			awaited = slices.DeleteFunc(awaited, func(a AwaitedResponse) bool { return a.ID == ref })
		case domain.EventUserToolConfirm:
			ref, deny := domain.ID(answerRef(ev)), denies(ev.Payload)
			for j := range awaited {
				if awaited[j].ID == ref {
					awaited[j].Confirmation = false
					awaited[j].Result = awaited[j].Result && !deny
				}
			}
			awaited = slices.DeleteFunc(awaited, func(a AwaitedResponse) bool { return !a.Confirmation && !a.Result })
		case domain.EventUserInterrupt:
			if reachesPrimary(i) {
				awaited = nil
			}
		case domain.EventUserMessage, domain.EventUserDefineOutcome:
			if first < 0 {
				first = i
			}
		}
	}
	if first < 0 || len(awaited) == 0 {
		return nil
	}
	ids := make([]domain.ID, len(awaited))
	for j, a := range awaited {
		ids[j] = a.ID
	}
	return &WhileAwaitingError{Type: posted[first].Type, Index: first, IDs: ids}
}
