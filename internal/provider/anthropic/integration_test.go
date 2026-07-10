package anthropic_test

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/anthropic"
)

// TestIntegrationRealEndpoint drives one real model turn against the
// endpoint configured in the environment (MODEL_PROTOCOL / MODEL_BASE_URL /
// MODEL_API_KEY / MODEL_ID, optionally loaded from the repo-root .env). It
// skips — never fails — when no endpoint is configured, so CI without
// credentials is unaffected. Credential values are never logged.
func TestIntegrationRealEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping the real model call")
	}
	loadDotEnv(t)
	baseURL, apiKey, model := os.Getenv("MODEL_BASE_URL"), os.Getenv("MODEL_API_KEY"), os.Getenv("MODEL_ID")
	if os.Getenv("MODEL_PROTOCOL") != "anthropic" || baseURL == "" || apiKey == "" || model == "" {
		t.Skip("no anthropic-protocol model endpoint configured (MODEL_* env)")
	}

	p, err := anthropic.New(provider.Config{
		Protocol: "anthropic", Model: model, BaseURL: baseURL, APIKey: apiKey,
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

// loadDotEnv fills MODEL_* from the repo root .env when unset (the file is
// gitignored; only these four keys are read, values never printed).
func loadDotEnv(t *testing.T) {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", ".env"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") || !strings.HasPrefix(key, "MODEL_") {
			continue
		}
		value = strings.TrimSpace(value)
		// A quoted value is unwrapped as-is; an unquoted one loses any
		// trailing inline comment.
		if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
			value = value[1 : n-1]
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}
