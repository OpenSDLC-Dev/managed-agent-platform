package brain_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modeltest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/anthropic"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// TestLiveThinkingReplay drives a real tool loop through the brain — stream,
// settlement, replay — against the anthropic-protocol endpoint the live tier
// configures, through a local proxy that keeps every request body as sent, and
// checks that each request carries back, as stored, every thinking block the
// turns before it kept, and that the endpoint answers it (#67,
// docs/plan/60_thinking-replay.md). The bodies are read off the wire because
// an endpoint that does not enforce replay answers just the same without the
// blocks. The system prompt and tools never change in the loop, so every kept
// block is still valid and none may be dropped. A model that returns no
// thinking passes on its loop alone and says so: there was nothing to replay.
// RUN_LIVE_MODEL_TESTS opts in (internal/modeltest).
func TestLiveThinkingReplay(t *testing.T) {
	cfg := modeltest.Endpoint(t, modeltest.LiveEnv, "anthropic")
	if testing.Short() {
		t.Skip("short mode: skipping the real model calls")
	}

	upstream, err := url.Parse(cfg.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var bodies [][]byte
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream) }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/messages") {
			mu.Lock()
			bodies = append(bodies, body)
			mu.Unlock()
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		proxy.ServeHTTP(w, r)
	}))
	defer srv.Close()

	pool := pgtest.NewPool(t)
	sid, envID := pgtest.NewSession(t, pool, "self_hosted")
	reg, err := provider.NewRegistry(
		[]provider.Route{{Model: "*", Config: provider.Config{
			Protocol: cfg.Protocol, Model: cfg.Model, BaseURL: srv.URL, APIKey: cfg.APIKey}}},
		map[string]provider.Factory{"anthropic": anthropic.New})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		pool: pool, log: events.NewLog(pool), queue: queue.New(pool),
		brain: brain.New(pool, reg, nil, brain.Config{}), registry: reg, sessionID: sid, envID: envID,
	}
	agent, err := json.Marshal(map[string]any{
		"type": "agent", "id": "agent_x", "version": 1, "name": "n",
		"model": map[string]string{"id": cfg.Model}, "description": "",
		"system": "You decide picnics. Call exactly one tool per turn and reason before each call.",
		"tools": []map[string]any{
			{"type": "custom", "name": "get_weather", "description": "Today's weather for a city.",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{
					"city": map[string]string{"type": "string"}}, "required": []string{"city"}}},
			{"type": "custom", "name": "get_forecast", "description": "Tomorrow's forecast for a city.",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{
					"city": map[string]string{"type": "string"}}, "required": []string{"city"}}},
		},
		"mcp_servers": []any{}, "skills": []any{}, "multiagent": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = $2 WHERE id = $1`, sid.String(), agent); err != nil {
		t.Fatal(err)
	}

	answers := []string{"18C, light rain, wind 30 km/h", "24C, sunny, light breeze", "unchanged"}
	h.wake(t, "Should I hold a picnic in Paris today or tomorrow? Check today's weather first, "+
		"then tomorrow's forecast, then decide.")
	var keptBefore []int
	for turn := 0; turn < 4; turn++ {
		keptBefore = append(keptBefore, h.thinkingRows(t))
		h.runOnce(t)
		if errs, err := h.log.List(context.Background(), sid, events.ListQuery{Types: []string{"session.error"}}); err != nil || len(errs) != 0 {
			mu.Lock()
			last := bodies[len(bodies)-1] // a body carries no credential: that is a header
			mu.Unlock()
			t.Fatalf("turn %d: the request failed (%v): %s\nrequest body: %s", turn+1, err, errs[0].Body, last)
		}
		tools := h.countType(t, "agent.custom_tool_use")
		results := h.countType(t, "user.custom_tool_result")
		if tools == results {
			break // the turn ended without a new call
		}
		h.answerLookup(t, answers[min(results, len(answers)-1)])
	}

	kept, err := h.log.ThinkingBlocks(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	var stored []map[string]any
	for _, b := range kept {
		var m map[string]any
		if err := json.Unmarshal(b.Block, &m); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, m)
	}
	if len(bodies) != len(keptBefore) {
		t.Fatalf("%d request bodies for %d turns", len(bodies), len(keptBefore))
	}
	for i, body := range bodies {
		var req struct {
			Messages []struct {
				Role    string           `json:"role"`
				Content []map[string]any `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("request %d body: %v", i+1, err)
		}
		replayed := 0
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b["type"] != "thinking" && b["type"] != "redacted_thinking" {
					continue
				}
				replayed++
				if m.Role != "assistant" || !containsBlock(stored, b) {
					t.Errorf("request %d sent a %s block in a %s message that is not one stored: %v", i+1, b["type"], m.Role, b)
				}
			}
		}
		if replayed != keptBefore[i] {
			t.Errorf("request %d carried %d thinking blocks, want the %d kept before it", i+1, replayed, keptBefore[i])
		}
	}
	if t.Failed() {
		return
	}
	if len(kept) == 0 {
		t.Logf("%s returned no thinking in %d requests: the loop ran, but there was nothing to replay", cfg.Model, len(bodies))
		return
	}
	t.Logf("%s: %d requests, %d thinking blocks kept, each sent as stored in every later request body, every request answered",
		cfg.Model, len(bodies), len(kept))
}

func containsBlock(stored []map[string]any, b map[string]any) bool {
	for _, s := range stored {
		if reflect.DeepEqual(s, b) {
			return true
		}
	}
	return false
}
