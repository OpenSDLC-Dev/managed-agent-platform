package anthropic_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

func TestGenerateEffort(t *testing.T) {
	for _, effort := range []domain.ModelEffort{"", "low", "medium", "high", "xhigh", "max"} {
		t.Run(string(effort), func(t *testing.T) {
			f := &fakeServer{sse: []string{
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			}}
			p := start(t, f)
			s, err := p.Generate(context.Background(), provider.Request{Effort: effort, Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}})
			if err != nil {
				t.Fatal(err)
			}
			collect(t, s)
			if effort == "" {
				if _, ok := f.gotBody["output_config"]; ok {
					t.Fatalf("omitted effort sent output_config: %v", f.gotBody)
				}
			} else {
				config, _ := f.gotBody["output_config"].(map[string]any)
				if config["effort"] != string(effort) {
					t.Fatalf("output_config = %v", config)
				}
			}
		})
	}
}
