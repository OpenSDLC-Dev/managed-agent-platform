package openai_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/openai"
)

// fakeServer speaks just enough of the OpenAI Chat Completions streaming
// protocol to prove the adapter sends a well-formed request and translates the
// stream faithfully. It captures the request and replays a scripted SSE body.
type fakeServer struct {
	t       *testing.T
	sse     []string // each becomes one `data: <s>` SSE frame; a final `data: [DONE]` is appended
	noDone  bool     // suppress the trailing `data: [DONE]` (simulates a cut-off stream)
	gotBody map[string]any
	gotHead http.Header
	status  int
	errBody string
}

func (f *fakeServer) handler(w http.ResponseWriter, r *http.Request) {
	f.t.Helper()
	if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}
	f.gotHead = r.Header.Clone()
	if err := json.NewDecoder(r.Body).Decode(&f.gotBody); err != nil {
		f.t.Errorf("decode request body: %v", err)
	}
	if f.status != 0 {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.errBody))
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	for _, data := range f.sse {
		_, _ = w.Write([]byte("data: " + data + "\n\n"))
	}
	if !f.noDone {
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
}

func start(t *testing.T, f *fakeServer) provider.Provider {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	p, err := openai.New(provider.Config{
		Protocol: "openai",
		Model:    "gpt-4o-mini",
		BaseURL:  srv.URL,
		APIKey:   "sk-test-123",
		Headers:  map[string]string{"x-gateway-route": "llm-pool-7"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func collect(t *testing.T, s provider.Stream) []provider.Chunk {
	t.Helper()
	var chunks []provider.Chunk
	for s.Next() {
		chunks = append(chunks, s.Chunk())
	}
	if err := s.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	return chunks
}

// TestGenerateFullTurn drives a turn that streams text then a tool call, and
// asserts both the request the adapter produced (Anthropic -> OpenAI) and the
// chunks it translated back (OpenAI -> Anthropic-native).
func TestGenerateFullTurn(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"bash","arguments":""}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":25,"completion_tokens":17,"total_tokens":42}}`,
	}}
	p := start(t, f)

	stream, err := p.Generate(context.Background(), provider.Request{
		System: "be terse",
		Messages: []provider.Message{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"run ls"}]`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"sure"},{"type":"tool_use","id":"call_prev","name":"bash","input":{"command":"pwd"}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_prev","content":"/home"}]`)},
		},
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"bash","description":"run a command","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}`),
		},
		MaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	chunks := collect(t, stream)

	// --- the request that actually left the adapter ---
	if got := f.gotHead.Get("Authorization"); got != "Bearer sk-test-123" {
		t.Errorf("Authorization = %q, want Bearer sk-test-123", got)
	}
	if got := f.gotHead.Get("x-gateway-route"); got != "llm-pool-7" {
		t.Errorf("extra header lost: %q", got)
	}
	if f.gotBody["model"] != "gpt-4o-mini" {
		t.Errorf("model = %v", f.gotBody["model"])
	}
	if f.gotBody["max_tokens"] != float64(512) {
		t.Errorf("max_tokens = %v", f.gotBody["max_tokens"])
	}
	if f.gotBody["stream"] != true {
		t.Errorf("stream = %v", f.gotBody["stream"])
	}
	if so, ok := f.gotBody["stream_options"].(map[string]any); !ok || so["include_usage"] != true {
		t.Errorf("stream_options = %v, want include_usage:true", f.gotBody["stream_options"])
	}
	msgs, _ := f.gotBody["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d, want 4 (system + user + assistant + tool)", len(msgs))
	}
	// system prepended
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "be terse" {
		t.Errorf("messages[0] = %v, want system/be terse", m0)
	}
	// user text
	m1 := msgs[1].(map[string]any)
	if m1["role"] != "user" || m1["content"] != "run ls" {
		t.Errorf("messages[1] = %v, want user/run ls", m1)
	}
	// assistant text + tool_use -> content + tool_calls (arguments is a JSON string)
	m2 := msgs[2].(map[string]any)
	if m2["role"] != "assistant" || m2["content"] != "sure" {
		t.Errorf("messages[2] role/content = %v", m2)
	}
	tcs, _ := m2["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("assistant tool_calls len = %d, want 1", len(tcs))
	}
	tc := tcs[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if tc["id"] != "call_prev" || tc["type"] != "function" || fn["name"] != "bash" {
		t.Errorf("tool_call = %v", tc)
	}
	if args, ok := fn["arguments"].(string); !ok || args != `{"command":"pwd"}` {
		t.Errorf("tool_call arguments = %v, want the object as a JSON string", fn["arguments"])
	}
	// tool_result -> tool role message keyed by tool_call_id
	m3 := msgs[3].(map[string]any)
	if m3["role"] != "tool" || m3["tool_call_id"] != "call_prev" || m3["content"] != "/home" {
		t.Errorf("messages[3] = %v, want tool/call_prev//home", m3)
	}
	// tools -> OpenAI function tools
	tools, _ := f.gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(tools))
	}
	tool0 := tools[0].(map[string]any)
	if tool0["type"] != "function" {
		t.Errorf("tool type = %v", tool0["type"])
	}
	tf := tool0["function"].(map[string]any)
	if tf["name"] != "bash" || tf["description"] != "run a command" {
		t.Errorf("tool function = %v", tf)
	}
	if _, ok := tf["parameters"].(map[string]any); !ok {
		t.Errorf("tool parameters missing (input_schema should map to parameters): %v", tf)
	}

	// --- the chunks the adapter translated back ---
	want := []provider.Chunk{
		{Kind: provider.KindTextDelta, Index: 0, Text: "Hel"},
		{Kind: provider.KindTextDelta, Index: 0, Text: "lo"},
	}
	if len(chunks) < 4 {
		t.Fatalf("chunks = %d, want text x2 + tool_use + done", len(chunks))
	}
	for i, w := range want {
		if chunks[i].Kind != w.Kind || chunks[i].Text != w.Text {
			t.Errorf("chunk[%d] = %+v, want %+v", i, chunks[i], w)
		}
	}
	tu := chunks[2]
	if tu.Kind != provider.KindToolUse || tu.ToolUse == nil {
		t.Fatalf("chunk[2] = %+v, want tool_use", tu)
	}
	if tu.ToolUse.ID != "call_9" || tu.ToolUse.Name != "bash" || string(tu.ToolUse.Input) != `{"command":"ls"}` {
		t.Errorf("tool_use = %+v (input %s)", tu.ToolUse, tu.ToolUse.Input)
	}
	done := chunks[len(chunks)-1]
	if done.Kind != provider.KindDone || done.StopReason != "tool_use" {
		t.Errorf("done = %+v, want stop_reason tool_use (mapped from finish_reason tool_calls)", done)
	}
	if done.Usage == nil || done.Usage.InputTokens != 25 || done.Usage.OutputTokens != 17 {
		t.Errorf("done usage = %+v, want in=25 out=17", done.Usage)
	}
}

// For a turn that carried no tool calls, finish_reason maps onto the Anthropic
// vocabulary: only "length" is a truncation; everything else — including a
// "tool_calls" that produced no actual tool call — is a completed turn. (A real
// tool call forces tool_use regardless of finish_reason; see
// TestToolCallForcesToolUse.)
func TestFinishReasonMapping(t *testing.T) {
	cases := map[string]string{"stop": "end_turn", "length": "max_tokens", "tool_calls": "end_turn", "content_filter": "end_turn"}
	for finish, wantStop := range cases {
		f := &fakeServer{sse: []string{
			`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"` + finish + `"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		}}
		p := start(t, f)
		stream, err := p.Generate(context.Background(), provider.Request{
			Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		})
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		chunks := collect(t, stream)
		done := chunks[len(chunks)-1]
		if done.Kind != provider.KindDone || done.StopReason != wantStop {
			t.Errorf("finish_reason %q -> %q, want %q", finish, done.StopReason, wantStop)
		}
	}
}

// String content (not a block array) must convert too.
func TestStringContentMessage(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"just a string"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_ = collect(t, stream)
	m0 := f.gotBody["messages"].([]any)[0].(map[string]any)
	if m0["role"] != "user" || m0["content"] != "just a string" {
		t.Errorf("string-content message = %v", m0)
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := openai.New(provider.Config{Protocol: "openai", Model: "m"}); err == nil {
		t.Error("New without base_url should fail")
	}
	if _, err := openai.New(provider.Config{Protocol: "openai", BaseURL: "http://x"}); err == nil {
		t.Error("New without model should fail")
	}
}

func TestUpstreamError(t *testing.T) {
	f := &fakeServer{status: http.StatusUnauthorized, errBody: `{"error":{"message":"bad key","type":"invalid_request_error"}}`}
	p := start(t, f)
	_, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err == nil {
		t.Fatal("Generate against a 401 should return an error")
	}
}

// A stream cut off mid-turn — the body ends with neither a finish_reason nor
// the [DONE] terminator — is a truncated turn, not a silent success.
func TestTruncatedStreamFails(t *testing.T) {
	f := &fakeServer{noDone: true, sse: []string{
		`{"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var chunks []provider.Chunk
	for stream.Next() {
		chunks = append(chunks, stream.Chunk())
	}
	if stream.Err() == nil {
		t.Error("a stream ending before finish_reason must surface an error")
	}
}
