package anthropic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/anthropic"
)

// fakeServer speaks just enough Anthropic Messages protocol to prove the
// adapter sends the right request and translates the stream faithfully.
type fakeServer struct {
	t       *testing.T
	sse     []string // data payloads, event name = payload's type field
	gotBody map[string]any
	gotHead http.Header
	status  int
}

func (f *fakeServer) handler(w http.ResponseWriter, r *http.Request) {
	f.t.Helper()
	if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}
	f.gotHead = r.Header.Clone()
	if err := json.NewDecoder(r.Body).Decode(&f.gotBody); err != nil {
		f.t.Errorf("decode request body: %v", err)
	}
	if f.status != 0 {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(f.status)
		fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	for _, data := range f.sse {
		var m struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(data), &m)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.Type, data)
	}
}

func start(t *testing.T, f *fakeServer) provider.Provider {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	p, err := anthropic.New(provider.Config{
		Protocol: "anthropic",
		Model:    "upstream-model",
		BaseURL:  srv.URL,
		APIKey:   "test-key-123",
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

func TestGenerateFullTurn(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"upstream-model","content":[],"stop_reason":null,"usage":{"input_tokens":25,"output_tokens":1,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hel"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_9","name":"bash","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":17}}`,
		`{"type":"message_stop"}`,
	}}
	p := start(t, f)

	stream, err := p.Generate(context.Background(), provider.Request{
		System: "be terse",
		Messages: []provider.Message{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"run ls"}]`)},
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

	// The request that actually left the adapter.
	if f.gotHead.Get("x-api-key") != "test-key-123" {
		t.Errorf("x-api-key = %q", f.gotHead.Get("x-api-key"))
	}
	if f.gotHead.Get("anthropic-version") == "" {
		t.Error("anthropic-version header missing")
	}
	if f.gotHead.Get("x-gateway-route") != "llm-pool-7" {
		t.Errorf("extra header lost: %q", f.gotHead.Get("x-gateway-route"))
	}
	if f.gotBody["model"] != "upstream-model" {
		t.Errorf("model = %v", f.gotBody["model"])
	}
	if f.gotBody["max_tokens"] != float64(512) {
		t.Errorf("max_tokens = %v", f.gotBody["max_tokens"])
	}
	if f.gotBody["stream"] != true {
		t.Errorf("stream = %v", f.gotBody["stream"])
	}
	system := f.gotBody["system"].([]any)[0].(map[string]any)
	if system["text"] != "be terse" || system["type"] != "text" {
		t.Errorf("system = %v", system)
	}
	msg := f.gotBody["messages"].([]any)[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("message role = %v", msg["role"])
	}
	if msg["content"].([]any)[0].(map[string]any)["text"] != "run ls" {
		t.Errorf("message content did not round-trip: %v", msg["content"])
	}
	tool := f.gotBody["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "bash" || tool["input_schema"].(map[string]any)["type"] != "object" {
		t.Errorf("tool definition did not round-trip: %v", tool)
	}

	// The chunk translation.
	want := []struct {
		kind provider.ChunkKind
		text string
	}{
		{provider.KindThinkingDelta, "pondering"},
		{provider.KindTextDelta, "Hel"},
		{provider.KindTextDelta, "lo"},
		{provider.KindToolUse, ""},
		{provider.KindDone, ""},
	}
	if len(chunks) != len(want) {
		t.Fatalf("got %d chunks (%+v), want %d", len(chunks), chunks, len(want))
	}
	for i, w := range want {
		if chunks[i].Kind != w.kind {
			t.Errorf("chunk %d kind = %s, want %s", i, chunks[i].Kind, w.kind)
		}
		if w.text != "" && chunks[i].Text != w.text {
			t.Errorf("chunk %d text = %q, want %q", i, chunks[i].Text, w.text)
		}
	}
	if chunks[1].Index != 1 || chunks[0].Index != 0 {
		t.Errorf("delta indexes = %d,%d", chunks[0].Index, chunks[1].Index)
	}

	tu := chunks[3].ToolUse
	if tu == nil || tu.ID != "toolu_9" || tu.Name != "bash" {
		t.Fatalf("tool use = %+v", tu)
	}
	var input map[string]any
	if err := json.Unmarshal(tu.Input, &input); err != nil || input["command"] != "ls" {
		t.Errorf("accumulated tool input = %s (%v)", tu.Input, err)
	}

	done := chunks[4]
	if done.StopReason != "tool_use" {
		t.Errorf("stop reason = %q", done.StopReason)
	}
	u := done.Usage
	if u == nil || u.InputTokens != 25 || u.OutputTokens != 17 ||
		u.CacheCreationInputTokens != 2 || u.CacheReadInputTokens != 3 {
		t.Errorf("usage = %+v", u)
	}
}

func TestGenerateEmptyToolInputAndStringContent(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_0","name":"noop","input":{}}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	}}
	p := start(t, f)

	// String-form content is valid Anthropic shorthand; the SDK
	// canonicalizes it to the equivalent single text block on the wire.
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"do nothing"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	chunks := collect(t, stream)
	if len(chunks) != 2 || chunks[0].Kind != provider.KindToolUse {
		t.Fatalf("chunks = %+v", chunks)
	}
	if string(chunks[0].ToolUse.Input) != "{}" {
		t.Errorf("empty tool input = %s, want {}", chunks[0].ToolUse.Input)
	}
	// Default max_tokens applied (the wire field is required).
	if f.gotBody["max_tokens"] == nil || f.gotBody["max_tokens"] == float64(0) {
		t.Errorf("max_tokens defaulted to %v", f.gotBody["max_tokens"])
	}
	content := f.gotBody["messages"].([]any)[0].(map[string]any)["content"]
	block, _ := content.([]any)[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "do nothing" {
		t.Errorf("string content should canonicalize to one text block, got %v", content)
	}
}

func TestGenerateToolInputOnStartBlock(t *testing.T) {
	// Non-Anthropic gateways may deliver the complete tool input on the
	// start block with no input_json_delta frames at all.
	f := &fakeServer{sse: []string{
		`{"type":"message_start","message":{"id":"msg_3","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_5","name":"read","input":{"path":"/tmp/x"}}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"read it"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	chunks := collect(t, stream)
	if len(chunks) != 2 || chunks[0].Kind != provider.KindToolUse {
		t.Fatalf("chunks = %+v", chunks)
	}
	var input map[string]any
	if err := json.Unmarshal(chunks[0].ToolUse.Input, &input); err != nil || input["path"] != "/tmp/x" {
		t.Errorf("start-block tool input dropped: %s", chunks[0].ToolUse.Input)
	}
}

func TestGenerateUpstreamError(t *testing.T) {
	f := &fakeServer{status: http.StatusUnauthorized}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err == nil {
		// Some SDK versions defer the error to the first stream read.
		if stream.Next() {
			t.Fatal("stream yielded a chunk from a 401 response")
		}
		err = stream.Err()
	}
	if err == nil {
		t.Fatal("401 upstream produced no error")
	}
}

func TestGenerateParallelToolUseInputsStayIntact(t *testing.T) {
	// Two tool_use blocks in one turn (parallel tool use): the first
	// emitted ToolUse.Input must survive the second block's accumulation —
	// the accumulator's backing array is reused between blocks.
	f := &fakeServer{sse: []string{
		`{"type":"message_start","message":{"id":"msg_5","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_a","name":"bash","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls -la\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_b","name":"bash","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"pwd\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"go"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	chunks := collect(t, stream) // full drain BEFORE inspecting inputs
	if len(chunks) != 3 {
		t.Fatalf("chunks = %+v", chunks)
	}
	if got := string(chunks[0].ToolUse.Input); got != `{"command":"ls -la"}` {
		t.Errorf("first tool input corrupted after drain: %s", got)
	}
	if got := string(chunks[1].ToolUse.Input); got != `{"command":"pwd"}` {
		t.Errorf("second tool input = %s", got)
	}
}

func TestGenerateTruncatedStream(t *testing.T) {
	// A stream that drains without message_stop is a truncated turn and
	// must surface as an error, never as a quiet success.
	f := &fakeServer{sse: []string{
		`{"type":"message_start","message":{"id":"msg_4","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cut o"}}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var sawDone bool
	for stream.Next() {
		if stream.Chunk().Kind == provider.KindDone {
			sawDone = true
		}
	}
	if sawDone {
		t.Error("truncated stream produced a done chunk")
	}
	if stream.Err() == nil {
		t.Error("truncated stream must surface an error")
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := anthropic.New(provider.Config{Model: "m"}); err == nil {
		t.Error("missing base_url should error — no silent api.anthropic.com fallback")
	}
	if _, err := anthropic.New(provider.Config{BaseURL: "http://x"}); err == nil {
		t.Error("missing model should error")
	}
}
