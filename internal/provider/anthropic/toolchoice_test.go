package anthropic_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// ToolChoiceNone goes out as tool_choice {"type": "none"}, beside the tools it
// keeps the model from calling (#1021); a request that does not ask sends no
// tool_choice, so the endpoint's default stands.
func TestGenerateToolChoiceNone(t *testing.T) {
	for _, none := range []bool{false, true} {
		f := &fakeServer{sse: []string{
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			`{"type":"message_stop"}`,
		}}
		p := start(t, f)
		s, err := p.Generate(context.Background(), provider.Request{
			Messages:       []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
			Tools:          []json.RawMessage{json.RawMessage(`{"name":"x","input_schema":{"type":"object"}}`)},
			ToolChoiceNone: none,
		})
		if err != nil {
			t.Fatal(err)
		}
		collect(t, s)
		choice, present := f.gotBody["tool_choice"]
		if !none {
			if present {
				t.Errorf("a request that did not ask sent tool_choice %v", choice)
			}
			continue
		}
		if c, _ := choice.(map[string]any); len(c) != 1 || c["type"] != "none" {
			t.Errorf("tool_choice = %v, want {\"type\": \"none\"}", choice)
		}
		if tools, _ := f.gotBody["tools"].([]any); len(tools) != 1 {
			t.Errorf("tools = %v, want the one definition kept", f.gotBody["tools"])
		}
	}
}
