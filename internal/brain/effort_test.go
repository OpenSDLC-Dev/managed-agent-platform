package brain_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/anthropic"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

func TestEffortPrimaryChildAndGrader(t *testing.T) {
	var mu sync.Mutex
	var requests []map[string]any
	replies := []string{"coordinator done", "child done", "all done\nVERDICT: satisfied"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		requests = append(requests, body)
		if len(requests) > len(replies) {
			t.Error("unexpected extra model request")
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", replies[len(requests)-1])
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	pool := pgtest.NewPool(t)
	sid, envID := pgtest.NewSession(t, pool, "self_hosted")
	reg, err := provider.NewRegistry([]provider.Route{{Model: "*", Config: provider.Config{
		Protocol: "anthropic", Model: "upstream-model", BaseURL: srv.URL, APIKey: "test-key",
	}}}, map[string]provider.Factory{"anthropic": anthropic.New})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{pool: pool, log: events.NewLog(pool), queue: queue.New(pool),
		brain: brain.New(pool, reg, nil, brain.Config{}), sessionID: sid, envID: envID}
	ctx := context.Background()
	// Each turn must read its own persisted snapshot, never the coordinator's
	// setting for a child, nor an unconfigured request for the grader.
	if _, err := h.pool.Exec(ctx, `UPDATE sessions SET resolved_agent = jsonb_set(resolved_agent, '{model}', '{"id":"custom","effort":{"type":"high"}}') WHERE id=$1`, h.sessionID.String()); err != nil {
		t.Fatal(err)
	}
	h.wakeOutcome(t, "finish", 2)
	child := h.childTurn(t, "finish child")
	if _, err := h.pool.Exec(ctx, `UPDATE session_threads SET agent = jsonb_set(agent, '{model}', '{"id":"custom","effort":{"type":"low"}}') WHERE id=$1`, child.String()); err != nil {
		t.Fatal(err)
	}
	h.drain(t)
	if got := h.outcomes(t); len(got) != 1 || got[0].Result != domain.OutcomeResultSatisfied {
		t.Fatalf("outcomes = %+v, want satisfied", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("requests = %d, want primary + child + grader", len(requests))
	}
	for i, want := range []string{"high", "low", "high"} {
		config, _ := requests[i]["output_config"].(map[string]any)
		if got := config["effort"]; got != want {
			t.Errorf("request %d output_config.effort = %v, want %q", i, got, want)
		}
		if got := requests[i]["model"]; got != "upstream-model" {
			t.Errorf("request %d model = %v, want configured upstream model", i, got)
		}
	}
}
