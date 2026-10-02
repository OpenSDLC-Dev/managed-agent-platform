package events_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// TestCheckWhileAwaiting pins how a send is judged against what the primary
// awaits: its own answers count wherever they sit, a confirmation's allow
// leaves a call that needs a result awaiting it, an interrupt reaching the
// primary clears everything, and only a message or an outcome is refused,
// the first one, naming what would still be awaited in log order.
func TestCheckWhileAwaiting(t *testing.T) {
	ev := func(typ domain.EventType, payload string) events.NewEvent {
		return events.NewEvent{Type: typ, Payload: json.RawMessage(payload)}
	}
	msg := ev(domain.EventUserMessage, `{}`)
	outcome := ev(domain.EventUserDefineOutcome, `{}`)
	custom := ev(domain.EventUserCustomToolRes, `{"custom_tool_use_id":"sevt_a"}`)
	allowB := ev(domain.EventUserToolConfirm, `{"tool_use_id":"sevt_b","result":"allow"}`)
	denyB := ev(domain.EventUserToolConfirm, `{"tool_use_id":"sevt_b","result":"deny"}`)
	allowC := ev(domain.EventUserToolConfirm, `{"tool_use_id":"sevt_c","result":"allow"}`)
	interrupt := ev(domain.EventUserInterrupt, `{}`)
	awaited := []events.AwaitedResponse{
		{ID: "sevt_a", Result: true},                     // a custom call
		{ID: "sevt_b", Confirmation: true, Result: true}, // a gated worker call
		{ID: "sevt_c", Confirmation: true},               // a gated platform call
	}
	reaches := func(int) bool { return true }

	for _, tc := range []struct {
		name   string
		posted []events.NewEvent
		index  int
		ids    []domain.ID
	}{
		{"a lone message", []events.NewEvent{msg}, 0, []domain.ID{"sevt_a", "sevt_b", "sevt_c"}},
		{"an outcome", []events.NewEvent{outcome}, 0, []domain.ID{"sevt_a", "sevt_b", "sevt_c"}},
		{"the first of two", []events.NewEvent{custom, msg, outcome}, 1, []domain.ID{"sevt_b", "sevt_c"}},
		{"an allow leaves a result due", []events.NewEvent{msg, custom, allowB, allowC}, 0, []domain.ID{"sevt_b"}},
		{"answers only", []events.NewEvent{custom, allowC}, -1, nil},
		{"every answer in", []events.NewEvent{msg, custom, denyB, allowC}, -1, nil},
		{"an interrupt", []events.NewEvent{msg, interrupt}, -1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := events.CheckWhileAwaiting(awaited, tc.posted, reaches)
			if tc.index < 0 {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var w *events.WhileAwaitingError
			if !errors.As(err, &w) {
				t.Fatalf("err = %v, want a *WhileAwaitingError", err)
			}
			if w.Index != tc.index || w.Type != tc.posted[tc.index].Type || len(w.IDs) != len(tc.ids) {
				t.Fatalf("refused %s at %d naming %v, want %s at %d naming %v", w.Type, w.Index, w.IDs, tc.posted[tc.index].Type, tc.index, tc.ids)
			}
			for i := range tc.ids {
				if w.IDs[i] != tc.ids[i] {
					t.Errorf("ids = %v, want %v", w.IDs, tc.ids)
				}
			}
		})
	}

	// An interrupt that names a child does not reach the primary.
	if err := events.CheckWhileAwaiting(awaited, []events.NewEvent{interrupt, msg}, func(int) bool { return false }); err == nil {
		t.Error("an interrupt not reaching the primary cleared its wait")
	}
	// The caller's list is not consumed.
	if len(awaited) != 3 || !awaited[1].Confirmation {
		t.Errorf("awaited mutated: %+v", awaited)
	}
	// The sentence, with several ids as Go renders a slice.
	err := events.CheckWhileAwaiting(awaited[:2], []events.NewEvent{msg}, reaches)
	want := "Invalid user.message event at events[0]: waiting on responses to events [sevt_a sevt_b]; " +
		"only `user.tool_confirmation`, `user.custom_tool_result`, `user.tool_result`, or `user.interrupt` may be sent " +
		"(a `system.message` may trail a tool result)"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}
