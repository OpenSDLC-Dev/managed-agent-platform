package modelgateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	evlog "github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/anthropic"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// brainRoute is one way the brain reaches a vendor through the gateway.
type brainRoute struct {
	name, alias string
	rec         *recorder
	// refused is a route whose provider's profile says nothing of the vendor's
	// thinking: the brain keeps plan 60's rule there, as it did on every route
	// before plan 61.
	refused bool
}

// TestLiveBrainThinkingThroughTheGateway is #883 end to end
// (docs/plan/61_thinking-replay-via-gateway.md): the brain drives a real tool
// loop through the gateway to each named chat vendor — passthrough on its
// Anthropic endpoint, and converted on its OpenAI one — and a system.message
// folds into its prompt between the tool call and its result, which moves the
// prefix every block was produced under. The gateway's word must reach the
// brain, which stores the call's thinking under its route alone and sends it
// back after the system.message, and the vendor must answer. On DeepSeek the
// loop runs once more through a provider of the generic profile, which says
// nothing: the brain drops the thinking, and DeepSeek refuses the request that
// lost it — #883 itself, so the routes that pass do so by this change. A
// vendor that returns no thinking passes on its loop alone and says so.
func TestLiveBrainThinkingThroughTheGateway(t *testing.T) {
	vendors := chatVendors(t)
	e := newEnv(t)
	var routes []brainRoute
	add := func(r brainRoute, prof, model string, endpoints func(proxy string) map[profile.Protocol]string, base string) {
		proxy := recordingProxy(t, base, r.rec)
		p := e.provider(proxy, func(p *store.Provider) { p.Name, p.Profile, p.Endpoints = r.alias, prof, endpoints(proxy) })
		e.credential(p, liveKeyOf(t, prof, vendors), 1)
		e.alias(r.alias, target(e.deployment(p, model), 0))
		routes = append(routes, r)
	}
	anth := func(proxy string) map[profile.Protocol]string {
		return map[profile.Protocol]string{profile.Anthropic: proxy}
	}
	for _, v := range vendors {
		model := v.models[0]
		add(brainRoute{name: v.name + "/passthrough", alias: "brain-pass-" + v.name, rec: &recorder{}}, v.name, model, anth, v.base)
		if host := openAIHost(v); host != "" {
			add(brainRoute{name: v.name + "/converted", alias: "brain-conv-" + v.name, rec: &recorder{}}, v.name, model,
				func(proxy string) map[profile.Protocol]string {
					return map[profile.Protocol]string{profile.OpenAI: proxy}
				}, host)
		}
		if v.name == "deepseek" {
			r := brainRoute{name: "deepseek/generic", alias: "brain-generic-deepseek", rec: &recorder{}, refused: true}
			proxy := recordingProxy(t, v.base, r.rec)
			p := e.provider(proxy, func(p *store.Provider) { p.Name = r.alias })
			e.credential(p, v.keyEnv, 1)
			e.alias(r.alias, target(e.deployment(p, model), 0))
			routes = append(routes, r)
		}
	}
	key := e.key(everyAlias)
	e.start()

	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) { brainLoop(t, e, key, r) })
	}
}

// liveKeyOf is the key of the named vendor prof.
func liveKeyOf(t *testing.T, prof string, vendors []liveVendor) string {
	t.Helper()
	for _, v := range vendors {
		if v.name == prof {
			return v.keyEnv
		}
	}
	t.Fatalf("no key for %s", prof)
	return ""
}

func brainLoop(t *testing.T, e *env, key string, r brainRoute) {
	ctx := context.Background()
	pool := e.pool
	sid, envID := pgtest.NewSession(t, pool, "self_hosted")
	reg, err := provider.NewRegistry(
		[]provider.Route{{Model: "*", Config: provider.Config{Protocol: "anthropic", Model: r.alias, BaseURL: e.url, APIKey: key}}},
		map[string]provider.Factory{"anthropic": anthropic.New})
	if err != nil {
		t.Fatal(err)
	}
	b := brain.New(pool, reg, nil, brain.Config{})
	log, q := evlog.NewLog(pool), queue.New(pool)
	agent, err := json.Marshal(map[string]any{
		"type": "agent", "id": "agent_x", "version": 1, "name": "n",
		"model": map[string]string{"id": r.alias}, "description": "",
		"system": "You decide picnics. Call exactly one tool per turn and reason before each call.",
		"tools": []map[string]any{{"type": "custom", "name": "get_weather", "description": "Today's weather for a city.",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{
				"city": map[string]string{"type": "string"}}, "required": []string{"city"}}}},
		"mcp_servers": []any{}, "skills": []any{}, "multiagent": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET resolved_agent = $2 WHERE id = $1`, sid.String(), agent); err != nil {
		t.Fatal(err)
	}
	ofType := func(typ string) []domain.Event {
		t.Helper()
		evs, err := log.List(ctx, sid, evlog.ListQuery{Types: []string{typ}})
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}
	run := func() {
		t.Helper()
		if found, err := b.RunOnce(ctx); err != nil || !found {
			t.Fatalf("RunOnce: found %v, %v", found, err)
		}
	}

	payload, _ := json.Marshal(map[string]any{"content": []map[string]string{{"type": "text",
		"text": "Should I hold a picnic in Paris today? Check today's weather first, then decide."}}})
	if _, err := log.AppendTransition(ctx, sid,
		[]evlog.NewEvent{{Type: domain.EventUserMessage, Payload: payload}},
		[]evlog.ThreadTransition{{Status: domain.SessionRunning}},
		evlog.AppendOptions{Then: func(ctx context.Context, tx pgx.Tx) error {
			_, err := q.Enqueue(ctx, tx, envID, sid, queue.ModelTurn)
			return err
		}}); err != nil {
		t.Fatal(err)
	}
	run()
	if errs := ofType("session.error"); len(errs) != 0 {
		t.Fatalf("the first request failed: %s", errs[0].Body)
	}
	calls := ofType("agent.custom_tool_use")
	if len(calls) != 1 {
		t.Fatalf("the first turn made %d tool calls, want 1", len(calls))
	}
	kept, err := log.ThinkingBlocks(ctx, sid, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) == 0 {
		t.Logf("%s returned no thinking with its tool call: the loop runs, but there is nothing to replay", r.name)
	}
	for _, k := range kept {
		if got := strings.HasPrefix(k.PrefixDigest, "any:"); got == r.refused {
			t.Fatalf("a block stored under %q: the gateway's word did not reach the brain as this route says", k.PrefixDigest)
		}
	}

	// The prefix moves: every block before this point was produced under
	// another system prompt.
	if _, err := log.Append(ctx, sid, []evlog.NewEvent{{Type: domain.EventSystemMessage,
		Payload: json.RawMessage(`{"content":[{"type":"text","text":"Answer in one short sentence."}]}`)}}); err != nil {
		t.Fatal(err)
	}
	sent := r.rec.count()
	postResult(t, pool, sid, calls[0].ID, "18C, sunny, light breeze")
	run()

	errs := ofType("session.error")
	if r.refused {
		if len(kept) == 0 {
			t.Skipf("%s returned no thinking: nothing for the brain to drop", r.name)
		}
		if len(errs) == 0 || !strings.Contains(string(errs[len(errs)-1].Body), "must be passed back") {
			t.Fatalf("the generic route was not refused for the thinking it dropped: %d session.error events", len(errs))
		}
		t.Logf("%s: the brain dropped the call's thinking at the system.message, and the vendor refused the request (#883)", r.name)
		return
	}
	if len(errs) != 0 {
		t.Fatalf("the request after the system.message failed: %s", errs[0].Body)
	}
	if len(kept) == 0 {
		return
	}
	out := r.rec.since(sent)
	if len(out) == 0 || !carriesThinking(t, out[len(out)-1].sent) {
		t.Fatalf("the request the vendor answered after the system.message carried no thinking")
	}
	t.Logf("%s: %d thinking blocks kept under the route alone, sent back after the system.message, and answered", r.name, len(kept))
}

// postResult answers a custom tool call through the control plane, as a
// client does.
func postResult(t *testing.T, pool *pgxpool.Pool, sid, callID domain.ID, text string) {
	t.Helper()
	if err := api.EnsureAPIKey(context.Background(), pool, "live-brain", "live-brain-key"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"events": []any{map[string]any{
		"type": string(domain.EventUserCustomToolRes), "custom_tool_use_id": callID.String(),
		"content": []map[string]string{{"type": "text", "text": text}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sid.String()+"/events", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "live-brain-key")
	rec := httptest.NewRecorder()
	api.NewHandler(pool, nil, nil, nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("post result: %d %s", rec.Code, rec.Body.String())
	}
}

// carriesThinking reports whether a request the gateway sent a vendor carries
// an assistant turn's thinking: a thinking block on the Messages API, the
// assistant message's reasoning_content on Chat Completions.
func carriesThinking(t *testing.T, body []byte) bool {
	t.Helper()
	var req struct {
		Messages []struct {
			Role             string          `json:"role"`
			Content          json.RawMessage `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request body: %v", err)
	}
	for _, m := range req.Messages {
		if m.Role != "assistant" {
			continue
		}
		if m.ReasoningContent != "" {
			return true
		}
		var blocks []struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(m.Content, &blocks)
		for _, b := range blocks {
			if b.Type == "thinking" || b.Type == "redacted_thinking" {
				return true
			}
		}
	}
	return false
}
