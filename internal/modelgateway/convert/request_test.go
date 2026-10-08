package convert_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/convert"
)

// request converts body, a Messages request, for the model "up" with
// DeepSeek's thinking words, and decodes what it made.
func request(t *testing.T, body string) map[string]any {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatal(err)
	}
	b, err := convert.Request(top, "up", map[string]string{"enabled": "enabled", "adaptive": "enabled", "disabled": "disabled"})
	if err != nil {
		t.Fatalf("Request(%s): %v", body, err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func decoded(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("%s: %v", s, err)
	}
	return v
}

const hi = `"messages":[{"role":"user","content":"hi"}]`

// Each field the Messages API takes at the SDK's pin has its disposition:
// mapped to its Chat Completions counterpart, or dropped as a vendor may
// ignore it.
func TestRequestFieldDispositions(t *testing.T) {
	got := request(t, `{"model":"alias","max_tokens":64,`+hi+`,"system":"be brief","temperature":0.5,"top_p":0.9,
		"top_k":40,"stop_sequences":["END"],"stream":true,"metadata":{"user_id":"u"},"service_tier":"auto",
		"inference_geo":"us","container":"c","cache_control":{"type":"ephemeral"},"thinking":{"type":"adaptive"},
		"output_config":{"effort":"high"},"tool_choice":{"type":"auto","disable_parallel_tool_use":true},
		"tools":[{"name":"get_time","description":"time","input_schema":{"type":"object"},"strict":true,"cache_control":{"type":"ephemeral"}}]}`)
	want := decoded(t, `{"model":"up","max_tokens":64,"temperature":0.5,"top_p":0.9,"stop":["END"],"stream":true,
		"stream_options":{"include_usage":true},"thinking":{"type":"enabled"},"reasoning_effort":"high",
		"tool_choice":"auto","parallel_tool_calls":false,
		"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"get_time","description":"time","parameters":{"type":"object"},"strict":true}}]}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// What a request leaves out, or sets null, it does not ask for.
func TestRequestNullsAreAbsent(t *testing.T) {
	got := request(t, `{"model":"alias","max_tokens":8,`+hi+`,"system":null,"stream":null,"thinking":null,"tools":null,"tool_choice":null,"metadata":null,"output_config":{"effort":null}}`)
	want := decoded(t, `{"model":"up","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

func TestRequestToolChoice(t *testing.T) {
	for in, want := range map[string]string{
		`{"type":"auto"}`:                   `"auto"`,
		`{"type":"any"}`:                    `"required"`,
		`{"type":"none"}`:                   `"none"`,
		`{"type":"tool","name":"get_time"}`: `{"type":"function","function":{"name":"get_time"}}`,
	} {
		got := request(t, `{"model":"a","max_tokens":8,`+hi+`,"tool_choice":`+in+`}`)
		if !reflect.DeepEqual(got["tool_choice"], decoded(t, want)) || got["parallel_tool_calls"] != nil {
			t.Errorf("tool_choice %s became %v, parallel_tool_calls %v; want %s and none", in, got["tool_choice"], got["parallel_tool_calls"], want)
		}
	}
}

// A vendor's own word for each thinking type goes upstream, its budget
// with no counterpart; a vendor naming none is sent no thinking.
func TestRequestThinking(t *testing.T) {
	minimax := map[string]string{"enabled": "adaptive", "adaptive": "adaptive", "disabled": "disabled"}
	for _, c := range []struct {
		in     string
		vendor map[string]string
		want   string
	}{
		{`{"type":"enabled","budget_tokens":1024}`, minimax, `{"type":"adaptive"}`},
		{`{"type":"disabled"}`, minimax, `{"type":"disabled"}`},
		{`{"type":"enabled","budget_tokens":1024}`, nil, ``},
	} {
		var top map[string]json.RawMessage
		_ = json.Unmarshal([]byte(`{"model":"a","max_tokens":8,`+hi+`,"thinking":`+c.in+`}`), &top)
		b, err := convert.Request(top, "up", c.vendor)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]json.RawMessage
		_ = json.Unmarshal(b, &out)
		if string(out["thinking"]) != c.want {
			t.Errorf("thinking %s for %v became %s, want %s", c.in, c.vendor, out["thinking"], c.want)
		}
		if got := convert.Thinking(json.RawMessage(c.in), c.vendor); string(got) != c.want {
			t.Errorf("Thinking(%s, %v) = %s, want %s", c.in, c.vendor, got, c.want)
		}
	}
}

// System blocks join a line apart, as Anthropic's compatibility layer joins
// the system messages it hoists.
func TestRequestSystemBlocks(t *testing.T) {
	got := request(t, `{"model":"a","max_tokens":8,`+hi+`,"system":[{"type":"text","text":"one"},{"type":"text","text":"two","cache_control":{"type":"ephemeral"}}]}`)
	msgs := got["messages"].([]any)
	if sys := msgs[0].(map[string]any); sys["role"] != "system" || sys["content"] != "one\ntwo" {
		t.Errorf("system message = %v, want one\\ntwo", sys)
	}
}

// A conversation's turns: images as image_url parts, tool calls and
// results, unsigned thinking back as reasoning_content and signed thinking
// dropped.
func TestRequestMessages(t *testing.T) {
	got := request(t, `{"model":"a","max_tokens":8,"messages":[
		{"role":"user","content":[{"type":"text","text":"look: "},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"image","source":{"type":"url","url":"https://a.example/i.png"}}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":""},{"type":"text","text":"calling"},{"type":"tool_use","id":"call_1","name":"get_time","input":{"tz": "UTC"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"noon"}],"is_error":true},{"type":"text","text":"thanks"},{"type":"text","text":" again"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"signed","signature":"mapgw1.gwdep_x.sig"},{"type":"redacted_thinking","data":"x"},{"type":"text","text":"done"}]}]}`)
	want := decoded(t, `[
		{"role":"user","content":[{"type":"text","text":"look: "},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"image_url","image_url":{"url":"https://a.example/i.png"}}]},
		{"role":"assistant","content":"calling","reasoning_content":"hmm","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\"}"}}]},
		{"role":"tool","content":"noon","tool_call_id":"call_1"},
		{"role":"user","content":"thanks again"},
		{"role":"assistant","content":"done"}]`)
	if !reflect.DeepEqual(got["messages"], want) {
		t.Errorf("messages\ngot  %v\nwant %v", got["messages"], want)
	}
}

// What has no Chat Completions counterpart, and changes what the caller can
// rely on, is refused, naming the field as the caller wrote it — and so is
// a field the conversion does not know.
func TestRequestRefusals(t *testing.T) {
	for body, want := range map[string]string{
		`{"model":"a","max_tokens":8,` + hi + `,"output_config":{"format":{"type":"json_schema","schema":{}}}}`:                                                   "output_config.format",
		`{"model":"a","max_tokens":8,` + hi + `,"tools":[{"type":"web_search_20250305","name":"web_search"}]}`:                                                    "tools[0]",
		`{"model":"a","max_tokens":8,` + hi + `,"mcp_servers":[]}`:                                                                                                "mcp_servers",
		`{"model":"a","max_tokens":8,` + hi + `,"thinking":{"type":"sometimes"}}`:                                                                                 "thinking.type",
		`{"model":"a","max_tokens":8,` + hi + `,"tool_choice":{"type":"tool"}}`:                                                                                   "tool_choice.name",
		`{"model":"a","max_tokens":8,` + hi + `,"tool_choice":{"type":"auto","disable_parallel_tool_use":"yes"}}`:                                                 "tool_choice.disable_parallel_tool_use",
		`{"model":"a","max_tokens":8,` + hi + `,"system":[{"type":"image","source":{}}]}`:                                                                         "system[0]",
		`{"model":"a","max_tokens":8,` + hi + `,"system":[{"type":"text","text":"a"},{"type":"document","text":"b"}]}`:                                            "system[1]",
		`{"model":"a","max_tokens":8,"messages":[{"role":"user","content":[{"type":"document","source":{}}]}]}`:                                                   "messages[0].content[0]",
		`{"model":"a","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"image","source":{"type":"url","url":"u"}}]}]}`:                           "messages[0].content[0]",
		`{"model":"a","max_tokens":8,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"file","file_id":"f"}}]}]}`:                           "messages[0].content[0].source",
		`{"model":"a","max_tokens":8,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":[{"type":"image","source":{}}]}]}]}`: "messages[0].content[0].content",
		`{"model":"a","max_tokens":8,"messages":"hi"}`:                                                                                                            "messages",
	} {
		var top map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &top); err != nil {
			t.Fatal(err)
		}
		_, err := convert.Request(top, "up", nil)
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("Request(%s) = %v, want an error naming %s", body, err, want)
		}
	}
}
