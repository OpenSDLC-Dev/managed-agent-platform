package openai_test

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
			f := &fakeServer{sse: []string{`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`}}
			p := start(t, f)
			s, err := p.Generate(context.Background(), provider.Request{Effort: effort, Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}})
			if err != nil {
				t.Fatal(err)
			}
			collect(t, s)
			got, present := f.gotBody["reasoning_effort"]
			if effort == "" {
				if present {
					t.Fatalf("omitted effort sent reasoning_effort: %v", got)
				}
			} else if got != string(effort) {
				t.Fatalf("reasoning_effort = %v, want %q", got, effort)
			}
		})
	}
}
