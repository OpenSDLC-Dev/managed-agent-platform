package openai_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// requestFor drives a minimal streamed turn and returns the request body the
// adapter produced, so a test can assert the Anthropic -> OpenAI conversion of
// req without caring about the response.
func requestFor(t *testing.T, req provider.Request) map[string]any {
	t.Helper()
	f := &fakeServer{sse: []string{
		`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_ = collect(t, stream)
	return f.gotBody
}

func messagesOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := body["messages"].([]any)
	out := make([]map[string]any, len(raw))
	for i, m := range raw {
		out[i] = m.(map[string]any)
	}
	return out
}

// A tool_result whose content is a block array (not a bare string) flattens to
// the joined text OpenAI's tool message carries.
func TestToolResultBlockArray(t *testing.T) {
	body := requestFor(t, provider.Request{
		Messages: []provider.Message{
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_x","content":[{"type":"text","text":"line1"},{"type":"text","text":"line2"}]}]`)},
		},
	})
	m := messagesOf(t, body)[0]
	if m["role"] != "tool" || m["tool_call_id"] != "call_x" || m["content"] != "line1line2" {
		t.Errorf("tool_result block array = %v, want tool/call_x/line1line2", m)
	}
}

// An empty tool_result content is valid and maps to an empty tool message.
func TestToolResultEmptyContent(t *testing.T) {
	body := requestFor(t, provider.Request{
		Messages: []provider.Message{
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_x","content":""}]`)},
		},
	})
	m := messagesOf(t, body)[0]
	if m["role"] != "tool" || m["content"] != "" {
		t.Errorf("empty tool_result = %v, want empty tool content", m)
	}
}

// An is_error tool_result still forwards its content (the platform embeds the
// failure text there); only the boolean flag is dropped, since OpenAI's tool
// message has no error field. This pins the documented lossy behavior.
func TestToolResultIsErrorContentForwarded(t *testing.T) {
	body := requestFor(t, provider.Request{
		Messages: []provider.Message{
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_e","is_error":true,"content":"boom: command failed"}]`)},
		},
	})
	m := messagesOf(t, body)[0]
	if m["role"] != "tool" || m["tool_call_id"] != "call_e" || m["content"] != "boom: command failed" {
		t.Errorf("is_error tool_result = %v, want the failure text forwarded on a tool message", m)
	}
	if _, present := m["is_error"]; present {
		t.Errorf("OpenAI tool message must not carry an is_error field, got %v", m["is_error"])
	}
}

// An assistant turn that is only a tool_use (no text) must omit content entirely
// — OpenAI accepts an assistant message carrying tool_calls and no content.
func TestAssistantToolUseOnly(t *testing.T) {
	body := requestFor(t, provider.Request{
		Messages: []provider.Message{
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"call_1","name":"bash","input":{"command":"ls"}}]`)},
		},
	})
	m := messagesOf(t, body)[0]
	if m["role"] != "assistant" {
		t.Fatalf("role = %v", m["role"])
	}
	if _, present := m["content"]; present {
		t.Errorf("assistant with only tool_use should omit content, got %v", m["content"])
	}
	if tcs, _ := m["tool_calls"].([]any); len(tcs) != 1 {
		t.Errorf("tool_calls = %v, want 1", m["tool_calls"])
	}
}

// A tool_use with no input serializes to an empty-object arguments string.
func TestToolUseEmptyInput(t *testing.T) {
	body := requestFor(t, provider.Request{
		Messages: []provider.Message{
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"call_1","name":"noargs"}]`)},
		},
	})
	tc := messagesOf(t, body)[0]["tool_calls"].([]any)[0].(map[string]any)
	if fn := tc["function"].(map[string]any); fn["arguments"] != "{}" {
		t.Errorf("empty tool input arguments = %v, want {}", tc["function"])
	}
}

// Thinking blocks have no Chat Completions equivalent and are dropped, not
// errored — an assistant turn with only thinking yields no assistant message.
func TestThinkingBlockDropped(t *testing.T) {
	body := requestFor(t, provider.Request{
		System: "sys",
		Messages: []provider.Message{
			{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"hmm"}]`)},
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
	})
	msgs := messagesOf(t, body)
	if len(msgs) != 2 { // system + user; the thinking-only assistant turn drops out
		t.Fatalf("messages = %d, want 2 (thinking-only assistant dropped)", len(msgs))
	}
	if msgs[0]["role"] != "system" || msgs[1]["role"] != "user" {
		t.Errorf("roles = %v/%v, want system/user", msgs[0]["role"], msgs[1]["role"])
	}
}

// max_tokens is omitted from the wire request when the caller leaves it zero, so
// the endpoint applies its own default.
func TestMaxTokensOmittedWhenZero(t *testing.T) {
	body := requestFor(t, provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if _, present := body["max_tokens"]; present {
		t.Errorf("max_tokens should be omitted when zero, got %v", body["max_tokens"])
	}
}

func TestUnsupportedBlockErrors(t *testing.T) {
	f := &fakeServer{}
	p := start(t, f)
	_, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{
			{Role: "user", Content: json.RawMessage(`[{"type":"image","source":{}}]`)},
		},
	})
	if err == nil {
		t.Error("an unsupported content block should fail loudly, not silently drop")
	}
}

func TestEmptyContentErrors(t *testing.T) {
	f := &fakeServer{}
	p := start(t, f)
	_, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`   `)}},
	})
	if err == nil {
		t.Error("empty message content should be an error")
	}
}

func TestBadToolSchemaErrors(t *testing.T) {
	f := &fakeServer{}
	p := start(t, f)
	if _, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools:    []json.RawMessage{json.RawMessage(`not json`)},
	}); err == nil {
		t.Error("malformed tool definition should error")
	}
	if _, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools:    []json.RawMessage{json.RawMessage(`{"description":"no name"}`)},
	}); err == nil {
		t.Error("a tool without a name should error")
	}
}

// With no tool calls present, a "function_call" or unknown finish reason is a
// completed turn (end_turn) — tool_use is reserved for turns that actually
// carried a tool call.
func TestFinishReasonExtras(t *testing.T) {
	cases := map[string]string{"function_call": "end_turn", "surprise": "end_turn", "": "end_turn"}
	for finish, wantStop := range cases {
		f := &fakeServer{sse: []string{
			`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"` + finish + `"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		}}
		// An empty finish_reason never terminates the turn, so append an explicit stop.
		if finish == "" {
			f.sse = append(f.sse[:2:2], `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, f.sse[2])
		}
		p := start(t, f)
		stream, err := p.Generate(context.Background(), provider.Request{
			Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		})
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		chunks := collect(t, stream)
		if done := chunks[len(chunks)-1]; done.StopReason != wantStop {
			t.Errorf("finish_reason %q -> %q, want %q", finish, done.StopReason, wantStop)
		}
	}
}

// The critical invariant: whenever the stream carried a tool call, the turn's
// stop_reason MUST be tool_use — the only signal the brain acts on to run the
// tool — even when an OpenAI-compatible server ends the turn with "stop",
// "length", or just [DONE] instead of "tool_calls". Getting this wrong drops
// the tool (finish "stop") or durably commits an unanswered tool_use that
// poisons session replay (finish "length").
func TestToolCallForcesToolUse(t *testing.T) {
	toolDeltas := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":null}]}`,
	}
	for _, finish := range []string{"stop", "length", "tool_calls", ""} {
		name := finish
		if name == "" {
			name = "none([DONE])"
		}
		t.Run(name, func(t *testing.T) {
			f := &fakeServer{sse: append([]string{}, toolDeltas...)}
			if finish != "" {
				f.sse = append(f.sse, `{"choices":[{"index":0,"delta":{},"finish_reason":"`+finish+`"}]}`)
			}
			f.sse = append(f.sse, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			p := start(t, f)
			stream, err := p.Generate(context.Background(), provider.Request{
				Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"go"`)}},
			})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			chunks := collect(t, stream)
			var sawToolUse bool
			for _, c := range chunks {
				if c.Kind == provider.KindToolUse {
					sawToolUse = true
				}
			}
			if !sawToolUse {
				t.Errorf("finish %q: no tool_use chunk emitted", finish)
			}
			if done := chunks[len(chunks)-1]; done.StopReason != "tool_use" {
				t.Errorf("finish %q: stop_reason = %q, want tool_use (tools were called)", finish, done.StopReason)
			}
		})
	}
}

// A minimal OpenAI-compatible server that streams content and ends with [DONE]
// but never populates finish_reason is a complete turn, not a truncation.
func TestDoneWithoutFinishReason(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"choices":[{"index":0,"delta":{"content":"hi there"},"finish_reason":null}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	chunks := collect(t, stream) // collect fails the test on a stream error
	if done := chunks[len(chunks)-1]; done.Kind != provider.KindDone || done.StopReason != "end_turn" {
		t.Errorf("[DONE] without finish_reason should complete as end_turn, got %+v", done)
	}
}

// A finish_reason arriving without a trailing [DONE] (the body just ends) is a
// complete turn, not a truncation — the finish_reason is the completion signal.
func TestFinishThenEOFCompletes(t *testing.T) {
	f := &fakeServer{noDone: true, sse: []string{
		`{"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	chunks := collect(t, stream) // fails on a stream error
	if done := chunks[len(chunks)-1]; done.Kind != provider.KindDone || done.StopReason != "end_turn" {
		t.Errorf("finish_reason then EOF should complete as end_turn, got %+v", done)
	}
}

// A failure reported mid-stream as an error frame under HTTP 200 surfaces its
// message, not a generic truncation error.
func TestMidStreamErrorFrame(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"choices":[{"index":0,"delta":{"content":"par"},"finish_reason":null}]}`,
		`{"error":{"message":"context length exceeded","type":"invalid_request_error"}}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next() {
	}
	if stream.Err() == nil || !strings.Contains(stream.Err().Error(), "context length exceeded") {
		t.Errorf("mid-stream error should surface the upstream message, got %v", stream.Err())
	}
}

// A safety refusal streamed through delta.refusal is the assistant's reply and
// must reach the caller as text, not vanish into an empty successful turn.
func TestRefusalPreserved(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"choices":[{"index":0,"delta":{"refusal":"I can't help with that."},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
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
	var text string
	for _, c := range chunks {
		if c.Kind == provider.KindTextDelta {
			text += c.Text
		}
	}
	if text != "I can't help with that." {
		t.Errorf("refusal text = %q, want it surfaced as assistant text", text)
	}
}

// prompt_tokens counts cached tokens too; the cached subset splits out of
// InputTokens into CacheReadInputTokens, matching the Anthropic usage shape.
func TestCachedTokensSplit(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":8,"total_tokens":108,"prompt_tokens_details":{"cached_tokens":30}}}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	chunks := collect(t, stream)
	u := chunks[len(chunks)-1].Usage
	if u == nil || u.InputTokens != 70 || u.CacheReadInputTokens != 30 || u.OutputTokens != 8 {
		t.Errorf("usage = %+v, want input=70 cache_read=30 output=8", u)
	}
	// Closing a completed stream drains its tail (keep-alive reuse) and must not error.
	if err := stream.Close(); err != nil {
		t.Errorf("Close after a completed stream: %v", err)
	}
}

// A malformed server reporting more cached than total tokens clamps rather than
// folding a negative InputTokens into session usage.
func TestCachedTokensClamped(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":999}}}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	chunks := collect(t, stream)
	u := chunks[len(chunks)-1].Usage
	if u == nil || u.InputTokens != 0 || u.CacheReadInputTokens != 10 {
		t.Errorf("usage = %+v, want input=0 cache_read=10 (clamped)", u)
	}
}

// The deprecated function_call streaming format is rejected loudly rather than
// silently losing the tool call.
func TestLegacyFunctionCallRejected(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"choices":[{"index":0,"delta":{"function_call":{"name":"bash","arguments":"{}"}},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"function_call"}]}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next() {
	}
	if stream.Err() == nil || !strings.Contains(stream.Err().Error(), "function_call") {
		t.Errorf("a legacy function_call stream should fail loudly, got %v", stream.Err())
	}
}

// A non-text block inside a tool_result has no OpenAI representation and fails
// loudly, matching the top-level unsupported-block behavior (not a silent drop).
func TestToolResultNonTextErrors(t *testing.T) {
	f := &fakeServer{}
	p := start(t, f)
	_, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_x","content":[{"type":"image","source":{}}]}]`)},
		},
	})
	if err == nil {
		t.Error("an image block in a tool_result should fail loudly, not drop to empty content")
	}
}

// A tool call streamed with no arguments fragments yields an empty-object input.
func TestStreamToolCallNoArgs(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_z","type":"function","function":{"name":"ping"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"go"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	chunks := collect(t, stream)
	var tu *provider.Chunk
	for i := range chunks {
		if chunks[i].Kind == provider.KindToolUse {
			tu = &chunks[i]
		}
	}
	if tu == nil || tu.ToolUse.ID != "call_z" || string(tu.ToolUse.Input) != "{}" {
		t.Errorf("no-arg tool call = %+v, want id call_z input {}", tu)
	}
}

// A malformed SSE frame surfaces as a stream error rather than a silent stop.
func TestMalformedFrameErrors(t *testing.T) {
	f := &fakeServer{sse: []string{`{"choices": not-json`}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next() {
	}
	if stream.Err() == nil {
		t.Error("a malformed SSE frame must surface as a stream error")
	}
	if err := stream.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
