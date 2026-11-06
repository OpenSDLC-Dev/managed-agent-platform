package brain_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// A session whose agent loses every tool after a call still sends the
// conversation the model had: the next request replays the call and its
// answer beside the one placeholder definition, under tool_choice none, rather
// than tool blocks with no tools (#1021). The turns before the change are
// unaffected.
func TestATurnAfterTheToolsAreRemovedOffersOnlyThePlaceholder(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{toolUseChunk("toolu_a", "lookup"), done("tool_use", 3)},
		{textChunk(0, "found it"), done("end_turn", 2)},
		{textChunk(0, "you're welcome"), done("end_turn", 2)},
	}, nil)
	h.customTool(t, "lookup")
	h.wake(t, "look it up")
	h.runOnce(t)
	uses, err := h.log.List(context.Background(), h.sessionID, events.ListQuery{Types: []string{"agent.custom_tool_use"}})
	if err != nil || len(uses) != 1 {
		t.Fatalf("tool intents = %d (%v), want 1", len(uses), err)
	}
	h.postToolResult(t, domain.EventUserCustomToolRes, map[string]any{
		"custom_tool_use_id": uses[0].ID.String(),
		"content":            []map[string]string{{"type": "text", "text": "42"}},
	})
	h.runOnce(t)

	// What a session update with agent.tools: [] stores.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = jsonb_set(resolved_agent, '{tools}', '[]'::jsonb) WHERE id = $1`,
		h.sessionID.String()); err != nil {
		t.Fatal(err)
	}
	h.wake(t, "thanks")
	h.runOnce(t)

	if n := len(h.provider.calls); n != 3 {
		t.Fatalf("model requests = %d, want 3", n)
	}
	names := func(req provider.Request) []string {
		var out []string
		for _, raw := range req.Tools {
			var d struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &d); err != nil {
				t.Fatal(err)
			}
			out = append(out, d.Name)
		}
		return out
	}
	for i, req := range h.provider.calls[:2] {
		if got := names(req); len(got) != 1 || got[0] != "lookup" || req.ToolChoiceNone {
			t.Errorf("request %d: tools = %v, tool_choice none = %v; want lookup, callable", i, got, req.ToolChoiceNone)
		}
	}
	last := h.provider.calls[2]
	if got := names(last); len(got) != 1 || got[0] != "no_tools_available" || !last.ToolChoiceNone {
		t.Fatalf("request after the removal: tools = %v, tool_choice none = %v; want the placeholder alone, uncallable",
			got, last.ToolChoiceNone)
	}
	var kinds []string
	for _, m := range last.Messages {
		var blocks []map[string]any
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			t.Fatal(err)
		}
		for _, b := range blocks {
			kinds = append(kinds, m.Role+":"+b["type"].(string))
		}
	}
	want := []string{"user:text", "assistant:tool_use", "user:tool_result", "assistant:text", "user:text"}
	if len(kinds) != len(want) {
		t.Fatalf("replayed blocks = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("replayed blocks = %v, want %v", kinds, want)
		}
	}
}
