package openai_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modeltest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/openai"
)

// TestIntegrationRealEndpoint drives one real model turn against the
// OpenAI-compatible endpoint configured for the live tier. It runs only under
// RUN_LIVE_MODEL_TESTS (see internal/modeltest for the opt-in contract), so an
// ordinary `go test ./...` never spends money. modeltest resolves the endpoint
// without printing the credential; an error raised by the endpoint itself is
// logged as-is, and this adapter quotes a failing response body with the
// configured credentials redacted out of it.
func TestIntegrationRealEndpoint(t *testing.T) {
	// Gate first: short mode may decline to spend the time, but it must not
	// become a way to opt in and still not be told the configuration is broken.
	cfg := modeltest.Endpoint(t, modeltest.LiveEnv, "openai")
	if testing.Short() {
		t.Skip("short mode: skipping the real model call")
	}

	p, err := openai.New(provider.Config{
		Protocol: cfg.Protocol, Model: cfg.Model, BaseURL: cfg.BaseURL, APIKey: cfg.APIKey,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	stream, err := p.Generate(ctx, provider.Request{
		System:    "You answer with a single short word.",
		Messages:  []provider.Message{{Role: "user", Content: []byte(`"Reply with the word OK."`)}},
		MaxTokens: 4096,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	var done *provider.Chunk
	for stream.Next() {
		c := stream.Chunk()
		if c.Kind == provider.KindTextDelta {
			text.WriteString(c.Text)
		}
		if c.Kind == provider.KindDone {
			d := c
			done = &d
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if done == nil {
		t.Fatal("turn ended without a done chunk")
	}
	if text.Len() == 0 {
		t.Error("turn produced no text")
	}
	if done.Usage == nil || done.Usage.OutputTokens == 0 {
		t.Errorf("usage not populated: %+v", done.Usage)
	}
	if done.StopReason == "" {
		t.Error("stop reason missing")
	}
	t.Logf("real turn ok: %d output tokens, stop_reason=%s, text=%q",
		done.Usage.OutputTokens, done.StopReason, text.String())
}
