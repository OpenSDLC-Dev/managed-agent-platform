package brain_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

// --- scripted provider ---

type fakeStream struct {
	chunks []provider.Chunk
	i      int
	err    error
}

func (s *fakeStream) Next() bool {
	if s.i >= len(s.chunks) {
		return false
	}
	s.i++
	return true
}
func (s *fakeStream) Chunk() provider.Chunk { return s.chunks[s.i-1] }
func (s *fakeStream) Err() error            { return s.err }
func (s *fakeStream) Close() error          { return nil }

type fakeProvider struct {
	mu      sync.Mutex
	model   string
	scripts [][]provider.Chunk
	errs    []error // parallel to scripts; non-nil = stream error after chunks
	calls   []provider.Request
	// onGenerate runs after the request is captured (i.e. after replay),
	// keyed by call index — the hook for mid-turn interleaving tests.
	onGenerate func(call int)
}

func (f *fakeProvider) Generate(_ context.Context, req provider.Request) (provider.Stream, error) {
	f.mu.Lock()
	n := len(f.calls)
	f.calls = append(f.calls, req)
	hook := f.onGenerate
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	if n >= len(f.scripts) {
		return nil, errors.New("fake provider: no script for call")
	}
	var serr error
	if n < len(f.errs) {
		serr = f.errs[n]
	}
	return &fakeStream{chunks: f.scripts[n], err: serr}, nil
}

func done(stop string, out int64) provider.Chunk {
	return provider.Chunk{Kind: provider.KindDone, StopReason: stop,
		Usage: &domain.ModelUsage{InputTokens: 10, OutputTokens: out}}
}

func textChunk(idx int64, s string) provider.Chunk {
	return provider.Chunk{Kind: provider.KindTextDelta, Index: idx, Text: s}
}

// harness bundles one session's world.
type harness struct {
	pool      *pgxpool.Pool
	log       *events.Log
	queue     *queue.Queue
	provider  *fakeProvider
	brain     *brain.Brain
	sessionID domain.ID
	envID     domain.ID
}

func newHarness(t *testing.T, scripts [][]provider.Chunk, errs []error) *harness {
	t.Helper()
	pool := pgtest.NewPool(t)
	sid, envID := pgtest.NewSession(t, pool, "self_hosted")
	fake := &fakeProvider{scripts: scripts, errs: errs}
	reg, err := provider.NewRegistry(
		[]provider.Route{{Model: "*", Config: provider.Config{Protocol: "fake", BaseURL: "http://fake"}}},
		map[string]provider.Factory{"fake": func(cfg provider.Config) (provider.Provider, error) {
			fake.model = cfg.Model
			return fake, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{
		pool: pool, log: events.NewLog(pool), queue: queue.New(pool),
		provider: fake, brain: brain.New(pool, reg, brain.Config{}),
		sessionID: sid, envID: envID,
	}
}

// wake mimics the control plane's user.message trigger: append + flip to
// running + enqueue, one transaction.
func (h *harness) wake(t *testing.T, text string) {
	t.Helper()
	running := domain.SessionRunning
	payload, _ := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
	_, err := h.log.AppendWith(context.Background(), h.sessionID, []events.NewEvent{
		{Type: domain.EventUserMessage, Payload: payload},
		{Type: domain.EventSessionStatusRunning},
	}, events.AppendOptions{
		SetStatus: &running,
		Then: func(ctx context.Context, tx pgx.Tx) error {
			_, err := h.queue.Enqueue(ctx, tx, h.envID, h.sessionID, queue.ModelTurn)
			return err
		},
	})
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
}

// postToolResult mimics the control plane's tool-result trigger.
func (h *harness) postToolResult(t *testing.T, eventType domain.EventType, payload map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	_, err := h.log.AppendWith(context.Background(), h.sessionID, []events.NewEvent{
		{Type: eventType, Payload: raw},
	}, events.AppendOptions{
		Then: func(ctx context.Context, tx pgx.Tx) error {
			_, err := h.queue.Enqueue(ctx, tx, h.envID, h.sessionID, queue.ModelTurn)
			return err
		},
	})
	if err != nil {
		t.Fatalf("post tool result: %v", err)
	}
}

func (h *harness) runOnce(t *testing.T) {
	t.Helper()
	found, err := h.brain.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !found {
		t.Fatal("RunOnce found no work")
	}
}

func (h *harness) types(t *testing.T) []string {
	t.Helper()
	evs, err := h.log.List(context.Background(), h.sessionID, events.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = string(ev.Type)
	}
	return out
}

func (h *harness) status(t *testing.T) string {
	t.Helper()
	var s string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT status FROM sessions WHERE id = $1`, h.sessionID.String()).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func stopReasonType(t *testing.T, body []byte) string {
	t.Helper()
	var p struct {
		StopReason struct {
			Type string `json:"type"`
		} `json:"stop_reason"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("idle payload %s: %v", body, err)
	}
	return p.StopReason.Type
}

func typesEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// --- tests ---

func TestSimpleTurn(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		provider.Chunk{Kind: provider.KindThinkingDelta, Index: 0, Text: "hmm"},
		textChunk(1, "Hel"), textChunk(1, "lo"),
		done("end_turn", 7),
	}}, nil)
	h.wake(t, "hi")
	h.runOnce(t)

	want := []string{
		"user.message", "session.status_running",
		"span.model_request_start", "agent.thinking", "agent.message",
		"span.model_request_end", "session.status_idle",
	}
	if got := h.types(t); !typesEqual(got, want) {
		t.Errorf("event log:\n got %v\nwant %v", got, want)
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle", got)
	}

	// The turn's request came from replay; the model string passed through.
	req := h.provider.calls[0]
	if h.provider.model != "fixture-model" {
		t.Errorf("provider model = %q", h.provider.model)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("request messages = %+v", req.Messages)
	}
	var blocks []map[string]any
	_ = json.Unmarshal(req.Messages[0].Content, &blocks)
	if len(blocks) != 1 || blocks[0]["type"] != "text" || blocks[0]["text"] != "hi" {
		t.Errorf("request content = %s", req.Messages[0].Content)
	}

	// agent.message carries the accumulated text; idle carries end_turn.
	evs, _ := h.log.List(context.Background(), h.sessionID, events.ListQuery{Types: []string{"agent.message"}})
	var msg struct {
		Content []domain.ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(evs[0].Body, &msg); err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "Hello" {
		t.Errorf("agent.message content = %+v", msg.Content)
	}
	evs, _ = h.log.List(context.Background(), h.sessionID, events.ListQuery{Types: []string{"session.status_idle"}})
	if stop := stopReasonType(t, evs[0].Body); stop != "end_turn" {
		t.Errorf("idle stop_reason = %q, payload %s", stop, evs[0].Body)
	}

	// Usage folded into the session; consumed inbound stamped; work done.
	var usageRaw []byte
	_ = h.pool.QueryRow(context.Background(), `SELECT usage FROM sessions WHERE id=$1`, h.sessionID.String()).Scan(&usageRaw)
	var usage domain.Usage
	_ = json.Unmarshal(usageRaw, &usage)
	if usage.OutputTokens != 7 || usage.InputTokens != 10 {
		t.Errorf("session usage = %+v", usage)
	}
	evs, _ = h.log.List(context.Background(), h.sessionID, events.ListQuery{Types: []string{"user.message"}})
	if evs[0].ProcessedAt == nil {
		t.Error("consumed user.message not stamped processed")
	}
	var live int
	_ = h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM work_items WHERE session_id=$1 AND state != 'stopped'`, h.sessionID.String()).Scan(&live)
	if live != 0 {
		t.Errorf("%d work items still live after the turn", live)
	}
}

func TestToolUseSuspendsAndResumes(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{
			provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
				ID: "toolu_provider_side", Name: "lookup", Input: json.RawMessage(`{"q":"go"}`)}},
			done("tool_use", 3),
		},
		{textChunk(0, "It is a language."), done("end_turn", 5)},
	}, nil)

	// The agent has one custom tool; its definition reaches the model.
	agentJSON := fmt.Sprintf(`{"type":"agent","id":"agent_x","version":1,"name":"n",
		"model":{"id":"fixture-model"},"system":"answer tersely","description":"",
		"tools":[{"type":"custom","name":"lookup","description":"look things up","input_schema":{"type":"object"}},
		         {"type":"agent_toolset_20260401"}],
		"mcp_servers":[],"skills":[],"multiagent":null}`)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = $2 WHERE id = $1`, h.sessionID.String(), agentJSON); err != nil {
		t.Fatal(err)
	}

	h.wake(t, "what is go?")
	h.runOnce(t)

	// Suspended: tool intent on the log, session still running, no idle.
	want := []string{
		"user.message", "session.status_running",
		"span.model_request_start", "agent.custom_tool_use", "span.model_request_end",
	}
	if got := h.types(t); !typesEqual(got, want) {
		t.Fatalf("after tool turn:\n got %v\nwant %v", got, want)
	}
	if got := h.status(t); got != "running" {
		t.Errorf("status while awaiting tool = %q, want running", got)
	}
	req := h.provider.calls[0]
	if len(req.Tools) != 1 {
		t.Fatalf("model saw %d tools, want 1 (custom only): %s", len(req.Tools), req.Tools)
	}
	var tool map[string]any
	_ = json.Unmarshal(req.Tools[0], &tool)
	if tool["name"] != "lookup" || tool["type"] != nil {
		t.Errorf("tool def = %v", tool)
	}
	if req.System != "answer tersely" {
		t.Errorf("system = %q", req.System)
	}

	// The result resumes the turn.
	evs, _ := h.log.List(context.Background(), h.sessionID, events.ListQuery{Types: []string{"agent.custom_tool_use"}})
	toolEventID := evs[0].ID.String()
	h.postToolResult(t, domain.EventUserCustomToolRes, map[string]any{
		"custom_tool_use_id": toolEventID,
		"content":            []map[string]string{{"type": "text", "text": "a programming language"}},
	})
	h.runOnce(t)

	if got := h.status(t); got != "idle" {
		t.Errorf("status after resume = %q, want idle", got)
	}
	// The resumed request rebuilt the conversation: assistant tool_use block
	// under the EVENT id, then the user tool_result referencing it.
	req = h.provider.calls[1]
	if len(req.Messages) != 3 {
		t.Fatalf("resumed request has %d messages: %+v", len(req.Messages), req.Messages)
	}
	var assistant []map[string]any
	_ = json.Unmarshal(req.Messages[1].Content, &assistant)
	if req.Messages[1].Role != "assistant" || assistant[0]["type"] != "tool_use" ||
		assistant[0]["id"] != toolEventID || assistant[0]["name"] != "lookup" {
		t.Errorf("assistant turn = %v", assistant)
	}
	var user []map[string]any
	_ = json.Unmarshal(req.Messages[2].Content, &user)
	if req.Messages[2].Role != "user" || user[0]["type"] != "tool_result" || user[0]["tool_use_id"] != toolEventID {
		t.Errorf("tool_result turn = %v", user)
	}
}

func TestMidTurnMessageChainsIntoNextTurn(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{textChunk(0, "first answer"), done("end_turn", 2)},
		{textChunk(0, "second answer"), done("end_turn", 2)},
	}, nil)
	// The second message arrives after turn 1's replay (session already
	// running: the API would only append) — the settlement check must catch
	// it and chain a new turn instead of idling past it.
	h.provider.onGenerate = func(call int) {
		if call != 0 {
			return
		}
		payload, _ := json.Marshal(map[string]any{"content": "two"})
		if _, err := h.log.Append(context.Background(), h.sessionID, []events.NewEvent{
			{Type: domain.EventUserMessage, Payload: payload},
		}); err != nil {
			t.Errorf("mid-turn append: %v", err)
		}
	}
	h.wake(t, "one")
	h.runOnce(t)

	// Turn 1 settled without idling: the pending message chained a turn.
	if got := h.status(t); got != "running" {
		t.Errorf("status between chained turns = %q, want running", got)
	}
	var idles int
	for _, typ := range h.types(t) {
		if typ == "session.status_idle" {
			idles++
		}
	}
	if idles != 0 {
		t.Errorf("session idled past a pending message (%d idle events)", idles)
	}

	h.runOnce(t)
	if got := h.status(t); got != "idle" {
		t.Errorf("status after chained turn = %q, want idle", got)
	}
	// One flip, one idle: the chain never re-announced running.
	var running int
	for _, typ := range h.types(t) {
		if typ == "session.status_running" {
			running++
		}
	}
	if running != 1 {
		t.Errorf("session.status_running count = %d, want 1", running)
	}
	// Turn 2's replay saw both messages. On the log, "two" landed before
	// the first answer (that is the true chronology), so the adjacent user
	// events merge into one user turn ahead of the assistant's reply.
	req := h.provider.calls[1]
	if len(req.Messages) != 2 || req.Messages[0].Role != "user" || req.Messages[1].Role != "assistant" {
		t.Fatalf("chained request messages = %d: %+v", len(req.Messages), req.Messages)
	}
	var merged []map[string]any
	_ = json.Unmarshal(req.Messages[0].Content, &merged)
	if len(merged) != 2 || merged[0]["text"] != "one" || merged[1]["text"] != "two" {
		t.Errorf("merged user turn = %v", merged)
	}
	// Everything consumed is stamped.
	evs, _ := h.log.List(context.Background(), h.sessionID, events.ListQuery{Types: []string{"user.message"}})
	for i, ev := range evs {
		if ev.ProcessedAt == nil {
			t.Errorf("user.message[%d] not stamped", i)
		}
	}
}

func TestProviderErrorFailsTurnVisibly(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{textChunk(0, "partial")},
	}, []error{errors.New("upstream 529")})
	h.wake(t, "hi")
	h.runOnce(t)

	types := h.types(t)
	want := []string{
		"user.message", "session.status_running", "span.model_request_start",
		"span.model_request_end", "session.error", "session.status_idle",
	}
	if !typesEqual(types, want) {
		t.Errorf("event log:\n got %v\nwant %v", types, want)
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle", got)
	}

	evs, _ := h.log.List(context.Background(), h.sessionID, events.ListQuery{Types: []string{"session.error"}})
	var errPayload struct {
		Error struct {
			Type        string `json:"type"`
			Message     string `json:"message"`
			RetryStatus struct {
				Type string `json:"type"`
			} `json:"retry_status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(evs[0].Body, &errPayload); err != nil {
		t.Fatal(err)
	}
	if errPayload.Error.Type != "model_request_failed_error" || errPayload.Error.RetryStatus.Type != "exhausted" {
		t.Errorf("session.error payload = %s", evs[0].Body)
	}
	evs, _ = h.log.List(context.Background(), h.sessionID, events.ListQuery{Types: []string{"session.status_idle"}})
	if stop := stopReasonType(t, evs[0].Body); stop != "retries_exhausted" {
		t.Errorf("idle stop_reason = %q, payload %s", stop, evs[0].Body)
	}
	// The span closed as an error before the failure was recorded.
	evs, _ = h.log.List(context.Background(), h.sessionID, events.ListQuery{Types: []string{"span.model_request_end"}})
	var span struct {
		IsError bool `json:"is_error"`
	}
	_ = json.Unmarshal(evs[0].Body, &span)
	if !span.IsError {
		t.Error("span end not marked is_error")
	}
	// The failed turn is finished work, not a retry loop.
	var live int
	_ = h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM work_items WHERE session_id=$1 AND state != 'stopped'`, h.sessionID.String()).Scan(&live)
	if live != 0 {
		t.Errorf("%d work items live after failed turn", live)
	}
}

func TestUnroutedModelFailsTurn(t *testing.T) {
	pool := pgtest.NewPool(t)
	sid, envID := pgtest.NewSession(t, pool, "cloud")
	reg, err := provider.NewRegistry(
		[]provider.Route{{Model: "some-other-model", Config: provider.Config{Protocol: "fake", BaseURL: "http://x"}}},
		map[string]provider.Factory{"fake": func(provider.Config) (provider.Provider, error) {
			t.Fatal("factory must not be called")
			return nil, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{pool: pool, log: events.NewLog(pool), queue: queue.New(pool),
		brain: brain.New(pool, reg, brain.Config{}), sessionID: sid, envID: envID}
	h.wake(t, "hi")
	h.runOnce(t)

	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle", got)
	}
	evs, _ := h.log.List(context.Background(), sid, events.ListQuery{Types: []string{"session.error"}})
	if len(evs) != 1 {
		t.Fatalf("session.error events = %d", len(evs))
	}
}

func TestReclaimedTurnSurfacesRecovery(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{textChunk(0, "recovered"), done("end_turn", 1)},
	}, nil)
	h.wake(t, "hi")

	// A previous brain claimed the turn and died: claim with a tiny lease
	// and let it expire.
	item, err := h.queue.Claim(context.Background(), queue.ModelTurn, 30*time.Millisecond)
	if err != nil || item == nil {
		t.Fatalf("pre-claim: %+v %v", item, err)
	}
	time.Sleep(40 * time.Millisecond)

	h.runOnce(t)

	types := h.types(t)
	want := []string{
		"user.message", "session.status_running",
		"session.status_rescheduled", "session.status_running",
		"span.model_request_start", "agent.message", "span.model_request_end",
		"session.status_idle",
	}
	if !typesEqual(types, want) {
		t.Errorf("event log:\n got %v\nwant %v", types, want)
	}
}

func TestRunDrainsAndStops(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{textChunk(0, "ok"), done("end_turn", 1)},
	}, nil)
	h.wake(t, "hi")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		// Stop the loop once the session idles.
		for ctx.Err() == nil {
			if h.status(t) == "idle" {
				cancel()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	if err := h.brain.Run(ctx); !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run returned %v", err)
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status after Run = %q", got)
	}
}
