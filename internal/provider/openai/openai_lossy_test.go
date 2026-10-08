package openai_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/openai"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
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

// A tool_result carrying search_result blocks (a web_search answer) flattens
// to text: title and source URL on a header line, then the result's own text
// content, newline-terminated so consecutive results stay separated. OpenAI's
// tool message is a string, so the structure is lossy by design — confined
// here and pinned, like every other lossy conversion in this package.
func TestToolResultSearchResultFlattens(t *testing.T) {
	body := requestFor(t, provider.Request{
		Messages: []provider.Message{
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_s","content":[` +
				`{"type":"search_result","title":"Go docs","source":"https://go.dev/doc/","content":[{"type":"text","text":"How to write Go."}],"citations":{"enabled":false}},` +
				`{"type":"text","text":"tail"}]}]`)},
		},
	})
	m := messagesOf(t, body)[0]
	want := "Go docs (https://go.dev/doc/)\nHow to write Go.\ntail"
	if m["role"] != "tool" || m["tool_call_id"] != "call_s" || m["content"] != want {
		t.Errorf("search_result tool_result = %v, want content %q", m, want)
	}
}

// A text block BEFORE a search_result gains a separating newline, so the
// header line cannot glue onto the text's last line (the reverse order is
// covered above, where the search block's own trailing newline separates).
func TestToolResultTextThenSearchResultSeparated(t *testing.T) {
	body := requestFor(t, provider.Request{
		Messages: []provider.Message{
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_s","content":[` +
				`{"type":"text","text":"preface"},` +
				`{"type":"search_result","title":"Go docs","source":"https://go.dev/doc/","content":[],"citations":{"enabled":false}}]}]`)},
		},
	})
	m := messagesOf(t, body)[0]
	want := "preface\nGo docs (https://go.dev/doc/)\n"
	if m["content"] != want {
		t.Errorf("content = %q, want %q", m["content"], want)
	}
}

// A search_result whose inner content holds a non-text block still fails
// loudly: the wire union says its content is text blocks only, so anything
// else is an upstream bug, not something to silently drop. The inner block is
// chosen to DECODE cleanly (a bare unknown type), so the failure proves the
// type guard fired, not a JSON error upstream of it.
func TestToolResultSearchResultRejectsNonTextInner(t *testing.T) {
	f := &fakeServer{}
	p := start(t, f)
	_, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_s","content":[` +
				`{"type":"search_result","title":"t","source":"https://a.example/","content":[{"type":"image","source":{}}]}]}]`)},
		},
	})
	if err == nil {
		t.Fatal("a non-text block inside search_result content should fail loudly")
	}
	if !strings.Contains(err.Error(), `unsupported block "image" inside search_result`) {
		t.Errorf("error = %v, want the inner type guard's message, not a decode error", err)
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

// A signed thinking block is another protocol's and drops out, not errored,
// so an assistant turn of signed thinking alone yields no assistant message;
// an unsigned one is reasoning a Chat Completions endpoint produced, and goes
// back as reasoning_content, the turn's content empty.
func TestThinkingBlocks(t *testing.T) {
	body := requestFor(t, provider.Request{
		System: "sys",
		Messages: []provider.Message{
			{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"signed","signature":"sig"}]`)},
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"hmm"}]`)},
			{Role: "user", Content: json.RawMessage(`"go on"`)},
		},
	})
	want := []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "hi"},
		{"role": "assistant", "content": "", "reasoning_content": "hmm"}, {"role": "user", "content": "go on"}}
	if got := messagesOf(t, body); !reflect.DeepEqual(got, want) {
		t.Errorf("messages = %v, want %v", got, want)
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

// A user turn's image reaches the endpoint as an image_url part, a base64
// source as a data URL, beside the turn's text.
func TestImageBlockBecomesAnImageURLPart(t *testing.T) {
	body := requestFor(t, provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"what is this"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]`)}},
	})
	got := messagesOf(t, body)[0]["content"]
	want := []any{map[string]any{"type": "text", "text": "what is this"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("content = %v, want %v", got, want)
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

// sentFunctions indexes a request's function tools by name, so an assertion
// about one tool does not lean on where the list happened to put it.
func sentFunctions(t *testing.T, body map[string]any) map[string]map[string]any {
	t.Helper()
	sent, _ := body["tools"].([]any)
	out := make(map[string]map[string]any, len(sent))
	for i, raw := range sent {
		fn, _ := raw.(map[string]any)["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			t.Fatalf("tools[%d] carries no function name: %v", i, raw)
		}
		out[name] = fn
	}
	return out
}

// The built-in web tools' input schemas carry three keywords as the reference
// was recorded sending them — "format": "uri" on url, "minLength": 2 on query,
// and "additionalProperties": false on both objects (#682) — and the anthropic
// adapter sends them on. This one strips all three from those built-ins (owner
// decision, #682): an OpenAI-compatible backend that accepts only part of JSON
// Schema can reject the whole tool list over one of them, and both web tools
// are on by default. Everything else in the schema arrives as the definition
// wrote it, and no "strict" is set.
//
// The six sandbox tools carry two of them since #822 — additionalProperties on
// all six, minLength on edit's old_string — and lose them the same way, so
// every one of the eight is checked.
func TestBuiltinToolParametersLoseFormatMinLengthAndAdditionalProperties(t *testing.T) {
	defs, err := toolset.Tools(json.RawMessage(`{"type":"agent_toolset_20260401"}`), time.Now())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	type definition struct {
		Name        string         `json:"name"`
		InputSchema map[string]any `json:"input_schema"`
	}
	byName := map[string]definition{}
	builtins := map[string]bool{}
	for _, raw := range defs {
		// A fresh value per decode: decoding into a reused one merges the
		// earlier tool's schema keys into this one's.
		var d definition
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("definition: %v", err)
		}
		byName[d.Name], builtins[d.Name] = d, true
	}
	if len(byName) != 8 {
		t.Fatalf("built-ins = %v, want all eight enabled", builtins)
	}
	// The definitions must carry what the adapter is meant to strip, or this
	// test passes over schemas that never had it.
	for _, tc := range []struct{ tool, prop, key string }{
		{"web_fetch", "url", "format"},
		{"web_search", "query", "minLength"},
		{"edit", "old_string", "minLength"},
	} {
		prop := byName[tc.tool].InputSchema["properties"].(map[string]any)[tc.prop].(map[string]any)
		if _, ok := prop[tc.key]; !ok {
			t.Fatalf("%s.%s = %v, want %s to strip", tc.tool, tc.prop, prop, tc.key)
		}
	}

	fns := sentFunctions(t, requestFor(t, provider.Request{
		Messages:     []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools:        defs,
		BuiltinTools: builtins,
	}))
	for name, def := range byName {
		if def.InputSchema["additionalProperties"] != false {
			t.Fatalf("%s input_schema.additionalProperties = %v, want false to strip", name, def.InputSchema["additionalProperties"])
		}
		want := def.InputSchema
		delete(want, "additionalProperties")
		for _, p := range want["properties"].(map[string]any) {
			delete(p.(map[string]any), "format")
			delete(p.(map[string]any), "minLength")
		}
		fn, ok := fns[name]
		if !ok {
			t.Fatalf("no %s function was sent", name)
		}
		if !reflect.DeepEqual(fn["parameters"], want) {
			t.Errorf("%s parameters = %v, want the input_schema without format, minLength and additionalProperties: %v",
				name, fn["parameters"], want)
		}
		if _, ok := fn["strict"]; ok {
			t.Errorf("%s carries strict = %v, want none", name, fn["strict"])
		}
	}
}

// The strip is schema-aware, not a key search. It removes the keywords from
// every subschema of a built-in definition — properties, items, the
// anyOf/oneOf/allOf branches, $defs, draft-07's schema-form dependencies,
// contentSchema — and unevaluatedProperties with additionalProperties; it
// leaves a property that is merely *named* format, a dependencies entry that
// is a list of names, and instance data — enum, default, const, examples —
// whatever keys that data holds. No built-in carries one of the keywords in a
// nested subschema today; this pins the walk for one that does.
func TestToolParametersStripTheKeywordsAtEveryDepth(t *testing.T) {
	in := `{"type":"object","additionalProperties":false,"required":["format"],` +
		`"properties":{` +
		`"format":{"type":"string","enum":["json","text"],"minLength":1},` +
		`"when":{"type":"string","format":"date-time","default":"now"},` +
		`"tags":{"type":"array","items":{"type":"string","minLength":2}},` +
		`"meta":{"type":"object","additionalProperties":{"type":"string","format":"email"}},` +
		`"choice":{"anyOf":[{"type":"string","format":"uri"},{"type":"integer"}]},` +
		`"opts":{"type":"object","properties":{"n":{"type":"integer"}},"additionalProperties":false,` +
		`"default":{"format":"kept","minLength":3},"const":{"additionalProperties":false},"examples":[{"format":"kept"}]}},` +
		`"$defs":{"link":{"type":"string","format":"uri"}},` +
		`"dependencies":{"when":{"properties":{"zone":{"type":"string","minLength":1}}},"tags":["when"]},` +
		`"contentSchema":{"type":"object","additionalProperties":false,"unevaluatedProperties":false},` +
		`"unevaluatedProperties":false}`
	want := `{"type":"object","required":["format"],` +
		`"properties":{` +
		`"format":{"type":"string","enum":["json","text"]},` +
		`"when":{"type":"string","default":"now"},` +
		`"tags":{"type":"array","items":{"type":"string"}},` +
		`"meta":{"type":"object"},` +
		`"choice":{"anyOf":[{"type":"string"},{"type":"integer"}]},` +
		`"opts":{"type":"object","properties":{"n":{"type":"integer"}},` +
		`"default":{"format":"kept","minLength":3},"const":{"additionalProperties":false},"examples":[{"format":"kept"}]}},` +
		`"$defs":{"link":{"type":"string"}},` +
		`"dependencies":{"when":{"properties":{"zone":{"type":"string"}}},"tags":["when"]},` +
		`"contentSchema":{"type":"object"}}`

	body := requestFor(t, provider.Request{
		Messages:     []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools:        []json.RawMessage{json.RawMessage(`{"name":"nested","description":"d","input_schema":` + in + `}`)},
		BuiltinTools: map[string]bool{"nested": true},
	})
	fns := sentFunctions(t, body)

	var wantSchema map[string]any
	if err := json.Unmarshal([]byte(want), &wantSchema); err != nil {
		t.Fatal(err)
	}
	if got := fns["nested"]["parameters"]; !reflect.DeepEqual(got, wantSchema) {
		gotJSON, _ := json.Marshal(got)
		t.Errorf("nested parameters =\n%s\nwant\n%s", gotJSON, want)
	}
}

// Only the platform's built-ins are stripped, and which those are is the
// request's BuiltinTools — provenance the brain records, never a name the
// adapter recognizes. A custom tool may take a built-in's name once that
// built-in is disabled, and an MCP tool's schema is its server's: both are
// contracts their authors set, so both reach the endpoint with every keyword
// they wrote, beside a built-in that loses them. The comparison is of content:
// encoding/json compacts (and HTML-escapes) a schema on its way out, as it did
// before any of this.
func TestUserToolSchemasPassThroughUntouched(t *testing.T) {
	custom := `{"type":"object","properties":{"query":{"type":"string","minLength":3,"format":"hostname"}},` +
		`"required":["query"],"additionalProperties":false}`
	mcp := `{"type":"object","properties":{"since":{"type":"string","format":"date-time"},` +
		`"tags":{"type":"object","additionalProperties":{"type":"string","minLength":1}}},"additionalProperties":false}`
	entry := json.RawMessage(`{"type":"agent_toolset_20260401","default_config":{"enabled":false},` +
		`"configs":[{"name":"web_fetch","enabled":true}]}`)
	builtins, err := toolset.Tools(entry, time.Now())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	body := requestFor(t, provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: append(builtins,
			json.RawMessage(`{"name":"web_search","description":"ours","input_schema":`+custom+`}`),
			json.RawMessage(`{"name":"mcp__docs__search","description":"theirs","input_schema":`+mcp+`}`)),
		BuiltinTools: map[string]bool{"web_fetch": true},
	})
	fns := sentFunctions(t, body)

	for name, schema := range map[string]string{"web_search": custom, "mcp__docs__search": mcp} {
		var want map[string]any
		if err := json.Unmarshal([]byte(schema), &want); err != nil {
			t.Fatal(err)
		}
		if got := fns[name]["parameters"]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s parameters = %v, want the schema as written: %s", name, got, schema)
		}
	}
	// The built-in beside them still loses its keywords, so the request did
	// carry provenance and the pass-through above is not the strip switched off.
	fetch := fns["web_fetch"]["parameters"].(map[string]any)
	if _, ok := fetch["additionalProperties"]; ok {
		t.Errorf("built-in web_fetch parameters = %v, want additionalProperties stripped", fetch)
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

// An OpenAI-compatible endpoint that ignores stream_options.include_usage
// sends no usage frame. That silence must reach the brain as "no reading"
// rather than as a turn that cost nothing (#90).
func TestNoUsageFrameReportsNoUsage(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
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
	if done.Kind != provider.KindDone || done.StopReason != "end_turn" {
		t.Fatalf("done = %+v", done)
	}
	if done.Usage != nil {
		t.Errorf("usage = %+v, want nil: the endpoint sent no usage frame", done.Usage)
	}
}

// The mirror of the case above: a usage frame that reports zeroes is a reading
// like any other. Only silence yields nil, so a turn that genuinely spent
// nothing still lands in the token metric.
func TestZeroedUsageFrameIsStillAReading(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
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
	if u == nil {
		t.Fatal("usage = nil, want a zeroed reading: the endpoint sent a usage frame")
	}
	if u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Errorf("usage = %+v, want zeroes", u)
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

// The route config's max_tokens is the default output cap for this endpoint:
// applied when the request sets none, overridden by a request that does. With
// neither set the field stays omitted (TestMaxTokensOmittedWhenZero).
func TestConfigMaxTokensDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  int64
		want float64
	}{
		{"request unset takes the config cap", 0, 4096},
		{"request wins over the config cap", 512, 512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeServer{t: t, sse: []string{
				`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			}}
			srv := httptest.NewServer(http.HandlerFunc(f.handler))
			t.Cleanup(srv.Close)
			p, err := openai.New(provider.Config{
				Protocol:  "openai",
				Model:     "gpt-4o-mini",
				BaseURL:   srv.URL,
				APIKey:    testAPIKey,
				MaxTokens: 4096,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			stream, err := p.Generate(context.Background(), provider.Request{
				Messages:  []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
				MaxTokens: tc.req,
			})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			_ = collect(t, stream)
			if got := f.gotBody["max_tokens"]; got != tc.want {
				t.Errorf("max_tokens = %v, want %v", got, tc.want)
			}
		})
	}
}
