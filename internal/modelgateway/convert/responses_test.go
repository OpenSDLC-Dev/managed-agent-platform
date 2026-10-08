package convert_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/convert"
	"github.com/openai/openai-go/v3/responses"
)

// responsesRequest converts body, a Responses request, and decodes the
// Messages request and the echo it made.
func responsesRequest(t *testing.T, body string) (map[string]any, map[string]any) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatal(err)
	}
	r, err := convert.ResponsesRequest(top)
	if err != nil {
		t.Fatalf("ResponsesRequest(%s): %v", body, err)
	}
	return decoded(t, string(mustJSON(t, r.Messages))).(map[string]any), decoded(t, string(mustJSON(t, r.Echo))).(map[string]any)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Each field ResponseNewParams names has its disposition: mapped to its
// Messages counterpart, echoed, or dropped.
func TestResponsesRequestFieldDispositions(t *testing.T) {
	got, echo := responsesRequest(t, `{"model":"alias","input":"hi","instructions":"be brief","max_output_tokens":64,
		"temperature":0.5,"top_p":0.9,"stream":true,"parallel_tool_calls":false,"tool_choice":"required",
		"tools":[{"type":"function","name":"get_time","description":"time","parameters":{"type":"object"},"strict":true}],
		"reasoning":{"effort":"minimal","summary":"auto"},"text":{"format":{"type":"json_schema","name":"t","schema":{"type":"object"},"strict":true},"verbosity":"low"},
		"metadata":{"k":"v"},"truncation":"disabled","store":true,"include":["reasoning.encrypted_content"],"top_logprobs":0,
		"max_tool_calls":3,"stream_options":{"include_obfuscation":false},"service_tier":"auto","prompt_cache_key":"p",
		"prompt_cache_retention":"24h","safety_identifier":"s","user":"u","background":false,"previous_response_id":null}`)
	want := decoded(t, `{"model":"alias","max_tokens":64,"temperature":0.5,"top_p":0.9,"stream":true,"system":"be brief",
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
		"tools":[{"name":"get_time","description":"time","input_schema":{"type":"object"},"strict":true}],
		"tool_choice":{"type":"any","disable_parallel_tool_use":true},"thinking":{"type":"adaptive"},
		"output_config":{"effort":"low","format":{"type":"json_schema","schema":{"type":"object"}}}}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
	for _, k := range []string{"instructions", "metadata", "tools", "tool_choice", "parallel_tool_calls", "temperature", "top_p",
		"max_output_tokens", "reasoning", "text", "truncation"} {
		if _, ok := echo[k]; !ok {
			t.Errorf("echo lacks %s: %v", k, echo)
		}
	}
	if echo["parallel_tool_calls"] != false || echo["tool_choice"] != "required" {
		t.Errorf("echo %v", echo)
	}
}

// A request that sets only model and input echoes the defaults a Response
// requires, and asks Messages for nothing it did not ask for.
func TestResponsesRequestDefaults(t *testing.T) {
	got, echo := responsesRequest(t, `{"model":"alias","input":"hi","instructions":null,"tools":null}`)
	want := decoded(t, `{"model":"alias","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
	wantEcho := decoded(t, `{"instructions":null,"metadata":{},"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"temperature":1,"top_p":1}`)
	if !reflect.DeepEqual(echo, wantEcho) {
		t.Errorf("echo %v\nwant %v", echo, wantEcho)
	}
}

// A tool choice, and the parallel calls a model may make, are asked for of a
// request offering tools. One offering none asks nothing of none or auto,
// nor of parallel_tool_calls, as nothing can be called; a call it requires
// stays, for the upstream to refuse.
func TestResponsesRequestToolChoice(t *testing.T) {
	const tools = `"tools":[{"type":"function","name":"f"}],`
	const tool = `"tools":[{"name":"f","input_schema":{"type":"object","properties":{}}}],`
	for in, want := range map[string]string{
		tools + `"tool_choice":"none"`:                             tool + `"tool_choice":{"type":"none"}`,
		tools + `"tool_choice":"auto"`:                             tool + `"tool_choice":{"type":"auto"}`,
		tools + `"tool_choice":{"type":"function","name":"f"}`:     tool + `"tool_choice":{"type":"tool","name":"f"}`,
		tools + `"parallel_tool_calls":false`:                      tool + `"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`,
		tools + `"parallel_tool_calls":false,"tool_choice":"none"`: tool + `"tool_choice":{"type":"none"}`,
		`"tool_choice":"none"`:                                     ``,
		`"tool_choice":"auto","parallel_tool_calls":false`:         ``,
		`"parallel_tool_calls":false`:                              ``,
		`"tools":[],"parallel_tool_calls":false`:                   ``,
		`"tool_choice":"required","parallel_tool_calls":false`:     `"tool_choice":{"type":"any"}`,
		`"tool_choice":{"type":"function","name":"f"}`:             `"tool_choice":{"type":"tool","name":"f"}`,
	} {
		check(t, in, want)
	}
}

// A function's parameters are its input_schema, an object schema: one that
// states no type, which OpenAI takes and Messages does not, is given it.
func TestResponsesRequestToolParameters(t *testing.T) {
	for in, want := range map[string]string{
		`"tools":[{"type":"function","name":"f","parameters":{}}]`:                                     `"tools":[{"name":"f","input_schema":{"type":"object"}}]`,
		`"tools":[{"type":"function","name":"f","parameters":{"properties":{"a":{"type":"string"}}}}]`: `"tools":[{"name":"f","input_schema":{"type":"object","properties":{"a":{"type":"string"}}}}]`,
		`"tools":[{"type":"function","name":"f","parameters":{"type":"object","required":[]}}]`:        `"tools":[{"name":"f","input_schema":{"type":"object","required":[]}}]`,
		`"tools":[{"type":"function","name":"f","parameters":{"type":"array"}}]`:                       `"tools":[{"name":"f","input_schema":{"type":"array"}}]`,
	} {
		check(t, in, want)
	}
}

// check converts a request whose input is "hi" and whose other fields are
// in, and wants the Messages request whose fields besides model and messages
// are want.
func check(t *testing.T, in, want string) {
	t.Helper()
	got, _ := responsesRequest(t, `{"model":"m","input":"hi",`+strings.TrimSuffix(in, ",")+`}`)
	w := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]`
	if want != "" {
		w += "," + strings.TrimSuffix(want, ",")
	}
	if !reflect.DeepEqual(got, decoded(t, w+"}")) {
		t.Errorf("%s: got %v, want %s}", in, got, w)
	}
}

func TestResponsesRequestEffort(t *testing.T) {
	for in, want := range map[string]string{
		`"reasoning":{"effort":"none"}`:                                                   `"thinking":{"type":"disabled"}`,
		`"reasoning":{"effort":"xhigh"}`:                                                  `"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"}`,
		`"reasoning":{"effort":null,"summary":"detailed"}`:                                ``,
		`"reasoning":{"context":"all_turns","mode":"standard","generate_summary":"auto"}`: ``,
		`"reasoning":{"context":"auto","mode":null}`:                                      ``,
		`"prompt_cache_options":{"mode":"explicit","ttl":"30m","prewarm":false}`:          ``,
		`"prompt_cache_options":{"comparison_response_id":null}`:                          ``,
		`"text":{"format":{"type":"text"},"verbosity":"high"}`:                            ``,
	} {
		got, _ := responsesRequest(t, `{"model":"m","input":"hi",`+in+`}`)
		w := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]`
		if want != "" {
			w += "," + want
		}
		if !reflect.DeepEqual(got, decoded(t, w+"}")) {
			t.Errorf("%s: got %v, want %s}", in, got, w)
		}
	}
}

// What a stateless gateway cannot do, or a Messages model cannot answer, is
// refused by the field that asks for it.
func TestResponsesRequestRefusals(t *testing.T) {
	for body, want := range map[string]string{
		`{"model":"m"}`: "input: Field required",
		`{"model":"m","input":"hi","previous_response_id":"resp_1"}`:                                                           "previous_response_id: the gateway stores no response",
		`{"model":"m","input":"hi","conversation":"conv_1"}`:                                                                   "conversation: the gateway stores no response",
		`{"model":"m","input":"hi","prompt":{"id":"pmpt_1"}}`:                                                                  "prompt: the gateway stores no prompt template",
		`{"model":"m","input":"hi","background":true}`:                                                                         "background:",
		`{"model":"m","input":"hi","truncation":"auto"}`:                                                                       "truncation: only disabled",
		`{"model":"m","input":"hi","top_logprobs":2}`:                                                                          "top_logprobs:",
		`{"model":"m","input":"hi","include":["message.output_text.logprobs"]}`:                                                "include: message.output_text.logprobs",
		`{"model":"m","input":"hi","text":{"format":{"type":"json_object"}}}`:                                                  "text.format: json_object",
		`{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`:                                                           `tools[0]: a "web_search" tool`,
		`{"model":"m","input":"hi","tools":[{"type":"function","name":"f","defer_loading":true}]}`:                             "tools[0].defer_loading:",
		`{"model":"m","input":"hi","tool_choice":{"type":"allowed_tools","mode":"auto","tools":[]}}`:                           `tool_choice.type: "allowed_tools"`,
		`{"model":"m","input":"hi","reasoning":{"effort":"extreme"}}`:                                                          `reasoning.effort: "extreme"`,
		`{"model":"m","input":"hi","max_output_tokens":0}`:                                                                     "max_output_tokens: must be a positive integer",
		`{"model":"m","input":"hi","context_management":[{"type":"compaction"}]}`:                                              "context_management: not supported",
		`{"model":"m","input":[{"type":"item_reference","id":"msg_1"}]}`:                                                       "input[0]: an item_reference names a stored item",
		`{"model":"m","input":[{"type":"web_search_call","id":"ws_1"}]}`:                                                       `input[0]: a "web_search_call" item`,
		`{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_id":"file_1"}]}]}`:                         `input[0].content[0]: an "input_file" part`,
		`{"model":"m","input":[{"role":"user","content":[{"type":"input_image","file_id":"file_1"}]}]}`:                        "input[0].content[0].file_id: the gateway stores no files",
		`{"model":"m","input":[{"role":"tool","content":"x"}]}`:                                                                `input[0].role: "tool"`,
		`{"model":"m","input":[{"type":"function_call","call_id":"c","name":"f","arguments":"[1]"}]}`:                          "input[0].arguments: must be a JSON object",
		`{"model":"m","input":[{"type":"function_call_output","output":"x"}]}`:                                                 "input[0].call_id: required",
		`{"model":"m","input":[{"type":"function_call_output","call_id":"c","output":[{"type":"input_file","file_id":"f"}]}]}`: `input[0].output[0]: an "input_file" part`,
		`{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":"ftp://x/a.png"}]}]}`:               "input[0].content[0].image_url: must be an http(s) or a data URL",
		`{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png,AAAA"}]}]}`:         "input[0].content[0].image_url: a data URL must be base64",
		`{"model":"m","input":[{"role":"system","content":[{"type":"input_image","image_url":"https://x/a.png"}]}]}`:           `input[0].content[0]: an "input_image" part in a system message`,
		`{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"t"}}}`:                                       "text.format.schema: required",
		`{"model":"m","input":"hi","text":{"format":{"type":"xml"}}}`:                                                          `text.format.type: "xml"`,
		`{"model":"m","input":"hi","tool_choice":"sometimes"}`:                                                                 `tool_choice: "sometimes"`,
		`{"model":"m","input":"hi","parallel_tool_calls":"no"}`:                                                                "parallel_tool_calls: must be a boolean",
		`{"model":"m","input":"hi","instructions":["x"]}`:                                                                      "instructions: must be a string",
		`{"model":"m","input":{"role":"user"}}`:                                                                                "input: must be a string or an array of objects",
		`{"model":"m","input":"hi","reasoning":{"context":"current_turn"}}`:                                                    `reasoning.context: "current_turn" is not supported`,
		`{"model":"m","input":"hi","reasoning":{"context":"recent"}}`:                                                          `reasoning.context: "recent" is not supported`,
		`{"model":"m","input":"hi","reasoning":{"mode":"pro"}}`:                                                                `reasoning.mode: "pro" is not supported`,
		`{"model":"m","input":"hi","reasoning":{"effort":"low","budget":1}}`:                                                   "reasoning.budget: not supported",
		`{"model":"m","input":"hi","prompt_cache_options":{"prewarm":true}}`:                                                   "prompt_cache_options.prewarm: the gateway always generates",
		`{"model":"m","input":"hi","prompt_cache_options":{"comparison_response_id":"resp_1"}}`:                                "prompt_cache_options.comparison_response_id: the gateway stores no response",
		`{"model":"m","input":"hi","prompt_cache_options":"x"}`:                                                                "prompt_cache_options: must be an object",
	} {
		var top map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &top); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		_, err := convert.ResponsesRequest(top)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", body, err, want)
		}
	}
}

// Input items become Messages turns: system and developer text joins the
// instructions; consecutive items of one role share a turn, a function
// call's output leading its user turn; a reasoning item is the thinking
// block it was, and one with no encrypted_content is left out.
func TestResponsesRequestInput(t *testing.T) {
	got, _ := responsesRequest(t, `{"model":"m","instructions":"first","input":[
		{"role":"developer","content":"second"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},
			{"type":"input_image","image_url":"data:image/png;base64,AAAA","detail":"auto"},
			{"type":"input_image","image_url":"https://example.com/a.png"}]},
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"hmm"}],"encrypted_content":"mapgw1.sig"},
		{"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"mapgw1.data"},
		{"type":"reasoning","id":"rs_3","summary":[{"type":"summary_text","text":"foreign"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"calling","annotations":[]}]},
		{"type":"function_call","call_id":"toolu_1","name":"f","arguments":"{\"a\":1}"},
		{"type":"function_call","call_id":"toolu_2","name":"g","arguments":""},
		{"role":"user","content":"and then"},
		{"type":"function_call_output","call_id":"toolu_1","output":"one"},
		{"type":"function_call_output","call_id":"toolu_2","output":[{"type":"input_text","text":"two"}]},
		{"role":"system","content":[{"type":"input_text","text":"third"}]},
		{"role":"assistant","content":""}]}`)
	want := decoded(t, `{"model":"m","system":"first\n\nsecond\n\nthird","messages":[
		{"role":"user","content":[{"type":"text","text":"look"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
			{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"mapgw1.sig"},
			{"type":"redacted_thinking","data":"mapgw1.data"},{"type":"text","text":"calling"},
			{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}},{"type":"tool_use","id":"toolu_2","name":"g","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"one"},
			{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"two"}]},
			{"type":"text","text":"and then"}]}]}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// A function call's output may be a list, its text and images a
// tool_result's content blocks.
func TestResponsesRequestToolOutputList(t *testing.T) {
	got, _ := responsesRequest(t, `{"model":"m","input":[{"type":"function_call_output","call_id":"toolu_1",
		"output":[{"type":"input_text","text":"see"},{"type":"input_text","text":""},{"type":"input_image","image_url":"data:image/jpeg;base64,BBBB"}]}]}`)
	want := decoded(t, `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"see"},
		{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"BBBB"}}]}]}]`)
	if !reflect.DeepEqual(got["messages"], want) {
		t.Errorf("got  %v\nwant %v", got["messages"], want)
	}
}

// An input with no user or assistant item is no messages, an array still,
// for the upstream to refuse by the field the caller wrote.
func TestResponsesRequestNoMessages(t *testing.T) {
	for _, in := range []string{`[]`, `[{"role":"developer","content":"x"}]`} {
		got, _ := responsesRequest(t, `{"model":"m","input":`+in+`}`)
		if m, ok := got["messages"].([]any); !ok || len(m) != 0 {
			t.Errorf("%s: messages %#v", in, got["messages"])
		}
	}
}

// Calls a client records each beside its output join the turn they came
// from when their ids name one Response, as the gateway's ids do, so the
// turn keeps the thinking it began with; calls whose ids name different
// Responses, ids of any form the gateway does not write — a caller's own —
// or no id at all, are turns of their own.
func TestResponsesRequestInterleavedCalls(t *testing.T) {
	// Response ids as the gateway mints them: 24 characters of its alphabet.
	const a, b, c, d = "0123456789abcdefghjkmnpq", "rstvwxyz0123456789abcdef", "ghjkmnpqrstvwxyz01234567", "zyxwvtsrqpnmkjhgfedcba98"
	call := func(id, callID string) string {
		if id != "" {
			id = `"id":"` + id + `",`
		}
		return `{"type":"function_call",` + id + `"call_id":"` + callID + `","name":"f","arguments":"{}"},` +
			`{"type":"function_call_output","call_id":"` + callID + `","output":"` + callID + `"}`
	}
	got, _ := responsesRequest(t, `{"model":"m","input":[
		{"type":"reasoning","id":"rs_`+a+`_0","summary":[{"type":"summary_text","text":"hmm"}],"encrypted_content":"mapgw1.sig"},
		`+call("fc_"+a+"_1", "t1")+`,`+call("fc_"+a+"_2", "t2")+`,`+call("fc_"+b+"_0", "t3")+`,`+call("", "t4")+`,`+call("", "t5")+`,
		{"role":"user","content":"and"},`+call("fc_"+c+"_0", "t6")+`,{"role":"user","content":"then"},`+call("fc_"+c+"_1", "t7")+`,
		`+call("fc_"+d+"_a", "t8")+`,`+call("fc_"+d+"_b", "t9")+`,`+call("fc_history_0", "t10")+`,`+call("fc_history_1", "t11")+`,
		`+call("call_"+d+"_0", "t12")+`,`+call("call_"+d+"_1", "t13")+`,
		`+call("fc_"+strings.Repeat("u", 24)+"_0", "t14")+`,`+call("fc_"+strings.Repeat("u", 24)+"_1", "t15")+`,`+call("fc_abc_0", "t16")+`,`+call("fc_abc_1", "t17")+`,`+call("fc_"+a+"_-1", "t18")+`,`+call("fc_"+a+"_+2", "t19")+`]}`)
	use := func(id string) string { return `{"type":"tool_use","id":"` + id + `","name":"f","input":{}}` }
	result := func(id string) string {
		return `{"type":"tool_result","tool_use_id":"` + id + `","content":"` + id + `"}`
	}
	w := (`[
		{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"mapgw1.sig"},` + use("t1") + `,` + use("t2") + `]},
		{"role":"user","content":[` + result("t1") + `,` + result("t2") + `]},
		{"role":"assistant","content":[` + use("t3") + `]},{"role":"user","content":[` + result("t3") + `]},
		{"role":"assistant","content":[` + use("t4") + `]},{"role":"user","content":[` + result("t4") + `]},
		{"role":"assistant","content":[` + use("t5") + `]},{"role":"user","content":[` + result("t5") + `,{"type":"text","text":"and"}]},
		{"role":"assistant","content":[` + use("t6") + `]},{"role":"user","content":[` + result("t6") + `,{"type":"text","text":"then"}]},
		{"role":"assistant","content":[` + use("t7") + `]},{"role":"user","content":[` + result("t7") + `]}`)
	for _, id := range []string{"t8", "t9", "t10", "t11", "t12", "t13", "t14", "t15", "t16", "t17", "t18", "t19"} {
		w += `,{"role":"assistant","content":[` + use(id) + `]},{"role":"user","content":[` + result(id) + `]}`
	}
	want := decoded(t, w+`]`)
	if !reflect.DeepEqual(got["messages"], want) {
		t.Errorf("got  %v\nwant %v", got["messages"], want)
	}
}

var meta = convert.ResponseMeta{ID: "resp_abc", Model: "alias", CreatedAt: 1700000000,
	Echo: map[string]json.RawMessage{"instructions": json.RawMessage("null"), "metadata": json.RawMessage("{}"),
		"tools": json.RawMessage("[]"), "tool_choice": json.RawMessage(`"auto"`), "parallel_tool_calls": json.RawMessage("true"),
		"temperature": json.RawMessage("1"), "top_p": json.RawMessage("1")}}

// a Messages answer with every block a Response carries.
const fullAnswer = `{"id":"msg_1","type":"message","role":"assistant","model":"alias","stop_reason":"tool_use",
	"content":[{"type":"thinking","thinking":"let me see","signature":"mapgw1.sig"},{"type":"redacted_thinking","data":"mapgw1.data"},
	{"type":"text","text":"Checking."},{"type":"tool_use","id":"toolu_1","name":"get_time","input":{"tz": "UTC"}}],
	"usage":{"input_tokens":10,"output_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}`

// answer converts a whole Messages answer and decodes it as openai-go does.
func answer(t *testing.T, msg string) (responses.Response, map[string]any) {
	t.Helper()
	b, err := convert.ResponsesAnswer([]byte(msg), meta)
	if err != nil {
		t.Fatalf("ResponsesAnswer: %v", err)
	}
	var r responses.Response
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return r, decoded(t, string(b)).(map[string]any)
}

func TestResponsesAnswer(t *testing.T) {
	r, raw := answer(t, fullAnswer)
	if r.ID != "resp_abc" || r.Object != "response" || r.Model != "alias" || r.Status != "completed" || r.CreatedAt != 1700000000 {
		t.Errorf("response %+v", r)
	}
	if len(r.Output) != 4 {
		t.Fatalf("output %+v", r.Output)
	}
	for _, o := range r.Output {
		if o.Status != "completed" {
			t.Errorf("%s item %s: status %q", o.Type, o.ID, o.Status)
		}
	}
	if o := r.Output[0]; o.Type != "reasoning" || o.ID != "rs_abc_0" || len(o.Summary) != 1 || o.Summary[0].Text != "let me see" || o.EncryptedContent != "mapgw1.sig" {
		t.Errorf("thinking %+v", o)
	}
	if o := r.Output[1]; o.Type != "reasoning" || len(o.Summary) != 0 || o.EncryptedContent != "mapgw1.data" {
		t.Errorf("redacted %+v", o)
	}
	if o := r.Output[2]; o.Type != "message" || o.ID != "msg_abc_2" || o.Role != "assistant" || o.Status != "completed" ||
		len(o.Content) != 1 || o.Content[0].Type != "output_text" || o.Content[0].Text != "Checking." {
		t.Errorf("text %+v", o)
	}
	if o := r.Output[3]; o.Type != "function_call" || o.ID != "fc_abc_3" || o.CallID != "toolu_1" || o.Name != "get_time" || o.Arguments.OfString != `{"tz":"UTC"}` {
		t.Errorf("tool_use %+v", o)
	}
	if r.OutputText() != "Checking." {
		t.Errorf("output text %q", r.OutputText())
	}
	u := r.Usage
	if u.InputTokens != 15 || u.InputTokensDetails.CachedTokens != 3 || u.OutputTokens != 7 || u.TotalTokens != 22 {
		t.Errorf("usage %+v", u)
	}
	for _, k := range []string{"instructions", "metadata", "tools", "tool_choice", "parallel_tool_calls", "temperature", "top_p", "error", "incomplete_details"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("the Response lacks %s, which openai-go requires", k)
		}
	}
}

// The thinking tokens an upstream reports, as MiniMax does, are the
// Response's reasoning tokens, whole and streamed — a stream's last report
// standing; where it reports none they are 0.
func TestResponsesReasoningTokens(t *testing.T) {
	r, _ := answer(t, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],
		"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":9,"output_tokens_details":{"thinking_tokens":6}}}`)
	if got := r.Usage.OutputTokensDetails.ReasoningTokens; got != 6 {
		t.Errorf("whole: reasoning_tokens %d, want 6", got)
	}
	if r, _ := answer(t, fullAnswer); r.Usage.OutputTokensDetails.ReasoningTokens != 0 {
		t.Errorf("whole, none reported: reasoning_tokens %d", r.Usage.OutputTokensDetails.ReasoningTokens)
	}
	events := respEvents(t, respStream(t,
		`message_start {"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":0,"output_tokens_details":{"thinking_tokens":0}}}}`,
		`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`content_block_stop {"type":"content_block_stop","index":0}`,
		`message_delta {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5,"output_tokens_details":{"thinking_tokens":3}}}`,
		`message_stop {"type":"message_stop"}`))
	if got := events[len(events)-1].Response.Usage.OutputTokensDetails.ReasoningTokens; got != 3 {
		t.Errorf("streamed: reasoning_tokens %d, want 3", got)
	}
}

// A count the ledger would not read — below zero, or past 2^32 — is not
// reported to the caller either.
func TestResponsesUsageBounds(t *testing.T) {
	r, _ := answer(t, `{"content":[],"stop_reason":"end_turn","usage":{"input_tokens":-5,"output_tokens":4294967297,
		"cache_read_input_tokens":3,"output_tokens_details":{"thinking_tokens":-1}}}`)
	if u := r.Usage; u.InputTokens != 3 || u.OutputTokens != 0 || u.OutputTokensDetails.ReasoningTokens != 0 || u.TotalTokens != 3 {
		t.Errorf("usage %+v", u)
	}
}

func TestResponsesAnswerStatus(t *testing.T) {
	for stop, want := range map[string][2]string{
		"end_turn": {"completed", ""}, "stop_sequence": {"completed", ""}, "tool_use": {"completed", ""},
		"max_tokens": {"incomplete", "max_output_tokens"}, "model_context_window_exceeded": {"incomplete", "max_output_tokens"},
		"refusal": {"incomplete", "content_filter"},
	} {
		r, _ := answer(t, `{"content":[{"type":"text","text":"x"},{"type":"tool_use","id":"t","name":"f","input":{}}],"stop_reason":"`+stop+`","usage":{"input_tokens":1,"output_tokens":1}}`)
		if string(r.Status) != want[0] || r.IncompleteDetails.Reason != want[1] {
			t.Errorf("%s: status %s, incomplete %q", stop, r.Status, r.IncompleteDetails.Reason)
		}
		// The item the answer stopped in is as unfinished as the answer.
		if r.Output[0].Status != "completed" || string(r.Output[1].Status) != want[0] {
			t.Errorf("%s: item statuses %s, %s", stop, r.Output[0].Status, r.Output[1].Status)
		}
	}
	if _, err := convert.ResponsesAnswer([]byte(`{"content":[{"type":"server_tool_use","id":"s"}]}`), meta); err == nil {
		t.Error("a server tool's block converted")
	}
	// A tool_use with no input calls with no arguments: an object, not null.
	if r, _ := answer(t, `{"content":[{"type":"tool_use","id":"t","name":"f","input":null}],"stop_reason":"tool_use","usage":{}}`); r.Output[0].Arguments.OfString != "{}" {
		t.Errorf("arguments %q", r.Output[0].Arguments.OfString)
	}
}

// What a Response says goes back as input unchanged and is the assistant
// turn it came from: every block, the signature and redacted data intact.
func TestResponsesRoundTrip(t *testing.T) {
	b, err := convert.ResponsesAnswer([]byte(fullAnswer), meta)
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(append(resp.Output, json.RawMessage(`{"type":"function_call_output","call_id":"toolu_1","output":"12:00"}`)))
	got, _ := responsesRequest(t, `{"model":"m","input":`+string(input)+`}`)
	var msg struct {
		Content []any `json:"content"`
	}
	if err := json.Unmarshal([]byte(fullAnswer), &msg); err != nil {
		t.Fatal(err)
	}
	want := []any{map[string]any{"role": "assistant", "content": msg.Content},
		decoded(t, `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"12:00"}]}`)}
	if !reflect.DeepEqual(got["messages"], want) {
		t.Errorf("got  %v\nwant %v", got["messages"], want)
	}
}

// respStream feeds a Messages stream's events, each "name data", through
// the conversion.
func respStream(t *testing.T, events ...string) []byte {
	t.Helper()
	s := convert.NewResponsesStream(meta, 1<<20)
	var out bytes.Buffer
	for _, e := range events {
		name, data, _ := strings.Cut(e, " ")
		b, err := s.Event(name, []byte(data))
		if err != nil {
			t.Fatalf("Event(%s): %v", e, err)
		}
		out.Write(b)
	}
	if !s.Done() {
		t.Error("the stream is not done")
	}
	return out.Bytes()
}

// respEvents decodes a Responses stream as openai-go does, checking each
// event line names its data's type, the sequence numbers count from zero,
// and no data carries a top-level error key, which openai-go reads as the
// stream failing.
func respEvents(t *testing.T, stream []byte) []responses.ResponseStreamEventUnion {
	t.Helper()
	var out []responses.ResponseStreamEventUnion
	for i, ev := range strings.Split(strings.TrimSuffix(string(stream), "\n\n"), "\n\n") {
		name, data, ok := strings.Cut(ev, "\ndata: ")
		if !ok || !strings.HasPrefix(name, "event: ") {
			t.Fatalf("not an event: %q", ev)
		}
		var e responses.ResponseStreamEventUnion
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			t.Fatalf("%s: %v", data, err)
		}
		var top map[string]json.RawMessage
		_ = json.Unmarshal([]byte(data), &top)
		if _, bad := top["error"]; bad {
			t.Errorf("event %s carries a top-level error key", data)
		}
		if e.Type != strings.TrimPrefix(name, "event: ") || e.SequenceNumber != int64(i) {
			t.Errorf("event %d: %s carries type %s, sequence %d", i, name, e.Type, e.SequenceNumber)
		}
		out = append(out, e)
	}
	return out
}

// A stream converts to the event flow OpenAI streams, and ends with the
// Response the whole answer converts to.
func TestResponsesStream(t *testing.T) {
	stream := respStream(t,
		`message_start {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"alias","content":[],"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}}`,
		`ping {"type":"ping"}`,
		`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`content_block_delta {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let "}}`,
		`content_block_delta {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"me see"}}`,
		`content_block_delta {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"mapgw1.sig"}}`,
		`content_block_stop {"type":"content_block_stop","index":0}`,
		`content_block_start {"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"mapgw1.data"}}`,
		`content_block_stop {"type":"content_block_stop","index":1}`,
		`content_block_start {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
		`content_block_delta {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"Check"}}`,
		`content_block_delta {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"ing."}}`,
		`content_block_stop {"type":"content_block_stop","index":2}`,
		`content_block_start {"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_time","input":{}}}`,
		`content_block_delta {"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"tz\":"}}`,
		`content_block_delta {"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":" \"UTC\"}"}}`,
		`content_block_stop {"type":"content_block_stop","index":3}`,
		`message_delta {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
		`message_stop {"type":"message_stop"}`)
	events := respEvents(t, stream)
	var types []string
	text, args := "", ""
	for _, e := range events {
		types = append(types, e.Type)
		switch e.Type {
		case "response.output_text.delta":
			text += e.Delta
		case "response.function_call_arguments.delta":
			args += e.Delta
		case "response.output_item.added":
			if e.Item.Status != "in_progress" {
				t.Errorf("added %s item: status %q", e.Item.Type, e.Item.Status)
			}
		case "response.output_item.done":
			if e.Item.Status != "completed" {
				t.Errorf("done %s item: status %q", e.Item.Type, e.Item.Status)
			}
		}
	}
	wantTypes := []string{"response.created", "response.in_progress",
		"response.output_item.added", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done",
		"response.output_item.added", "response.output_item.done",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done",
		"response.completed"}
	if !reflect.DeepEqual(types, wantTypes) {
		t.Errorf("events %v\nwant   %v", types, wantTypes)
	}
	if text != "Checking." || args != `{"tz": "UTC"}` {
		t.Errorf("deltas %q, %q", text, args)
	}
	last := events[len(events)-1].Response
	whole, _ := answer(t, fullAnswer)
	got, _ := json.Marshal(last.Output)
	want, _ := json.Marshal(whole.Output)
	if string(got) != strings.ReplaceAll(string(want), `{\"tz\":\"UTC\"}`, `{\"tz\": \"UTC\"}`) {
		t.Errorf("streamed output %s\nwhole output    %s", got, want)
	}
	if last.Status != "completed" || last.Usage.InputTokens != 15 || last.Usage.OutputTokens != 7 || last.ID != "resp_abc" {
		t.Errorf("final response %+v", last)
	}
	if first := events[0].Response; first.Status != "in_progress" || len(first.Output) != 0 {
		t.Errorf("response.created %+v", first)
	}
}

// A tool call whose input arrives whole at its start, with no deltas after,
// has that input as its arguments.
func TestResponsesStreamToolInputAtStart(t *testing.T) {
	events := respEvents(t, respStream(t,
		`message_start {"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"f","input":{"a": 1}}}`,
		`content_block_stop {"type":"content_block_stop","index":0}`,
		`message_delta {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`,
		`message_stop {"type":"message_stop"}`))
	if out := events[len(events)-1].Response.Output; len(out) != 1 || out[0].Arguments.OfString != `{"a":1}` {
		t.Errorf("output %+v", out)
	}
}

// What a stream holds for its Response comes from its blocks' starts and
// deltas, so their size is bounded, whatever they hold — a call's id, blocks
// with nothing in them, text — and the event that passes the bound fails.
func TestResponsesStreamBound(t *testing.T) {
	const limit = 400
	blockStart := func(i int, blk string) string {
		return `content_block_start {"type":"content_block_start","index":` + strconv.Itoa(i) + `,"content_block":` + blk + `}`
	}
	blockStop := func(i int) string {
		return `content_block_stop {"type":"content_block_stop","index":` + strconv.Itoa(i) + `}`
	}
	var empty, text []string
	for i := 0; i < 10; i++ {
		empty = append(empty, blockStart(i, `{"type":"text","text":""}`), blockStop(i))
		text = append(text, `content_block_delta {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+strings.Repeat("x", 50)+`"}}`)
	}
	for name, events := range map[string][]string{
		"a call's id":  {blockStart(0, `{"type":"tool_use","id":"`+strings.Repeat("i", limit)+`","name":"f","input":{}}`)},
		"empty blocks": empty,
		"text":         append([]string{blockStart(0, `{"type":"text","text":""}`)}, text...),
	} {
		s := convert.NewResponsesStream(meta, limit)
		if _, err := s.Event("message_start", []byte(`{"type":"message_start","message":{"usage":{}}}`)); err != nil {
			t.Fatal(err)
		}
		size, failed := 0, false
		for _, e := range events {
			n, data, _ := strings.Cut(e, " ")
			if n != "content_block_stop" {
				size += len(data)
			}
			_, err := s.Event(n, []byte(data))
			if want := size > limit; (err != nil) != want || want && err.Error() != "the answer passes the gateway's bound of 400 bytes" {
				t.Errorf("%s: at %d bytes: %v", name, size, err)
			}
			if err != nil {
				failed = true
				break
			}
		}
		if !failed {
			t.Errorf("%s: never passed the bound", name)
		}
	}
}

// An event the conversion cannot carry fails it, and the events it made
// first — the stream's start, a finished item's done — come back with the
// error, so the failure the caller then sends follows them in sequence.
func TestResponsesStreamKeepsWhatAFailureFollows(t *testing.T) {
	server := `content_block_start {"type":"content_block_start","index":%d,"content_block":{"type":"server_tool_use","id":"s"}}`
	for name, events := range map[string][]string{
		"the first event": {strings.Replace(server, "%d", "0", 1)},
		"after an item": {`message_start {"type":"message_start","message":{"usage":{}}}`,
			`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"x"}}`,
			`content_block_stop {"type":"content_block_stop","index":0}`, strings.Replace(server, "%d", "1", 1)},
	} {
		s := convert.NewResponsesStream(meta, 1<<20)
		var all []byte
		for _, e := range events {
			n, data, _ := strings.Cut(e, " ")
			b, err := s.Event(n, []byte(data))
			all = append(all, b...)
			if err != nil {
				all = append(all, s.Failure("api_error", err.Error())...)
				break
			}
		}
		got := respEvents(t, all) // which checks the sequence runs unbroken from 0
		if got[0].Type != "response.created" || got[len(got)-1].Type != "response.failed" {
			t.Errorf("%s: events %+v", name, got)
		}
		added, done := 0, 0
		for _, e := range got {
			switch e.Type {
			case "response.output_item.added":
				added++
			case "response.output_item.done":
				done++
			}
		}
		if added != done {
			t.Errorf("%s: %d items added, %d done", name, added, done)
		}
	}
}

// The item an incomplete answer stopped in is its last in output order, as a
// whole answer's is, however the blocks' stops interleave; and the stop
// reason that decides it is the answer's last.
func TestResponsesStreamIncompleteItem(t *testing.T) {
	start := `message_start {"type":"message_start","message":{"usage":{}}}`
	events := respEvents(t, respStream(t, start,
		`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`content_block_start {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t","name":"f","input":{}}}`,
		`content_block_delta {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\":"}}`,
		`content_block_stop {"type":"content_block_stop","index":1}`,
		`content_block_delta {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"mapgw1.sig"}}`,
		`content_block_stop {"type":"content_block_stop","index":0}`,
		`message_delta {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":3}}`,
		`message_stop {"type":"message_stop"}`))
	last := events[len(events)-1]
	if out := last.Response.Output; len(out) != 2 || out[0].Status != "completed" || out[1].Status != "incomplete" {
		t.Errorf("output %+v", out)
	}
	for _, e := range events {
		if e.Type == "response.output_item.done" && string(e.Item.Status) != map[string]string{"reasoning": "completed", "function_call": "incomplete"}[e.Item.Type] {
			t.Errorf("done %s item: %s", e.Item.Type, e.Item.Status)
		}
	}
	events = respEvents(t, respStream(t, start,
		`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"x"}}`,
		`content_block_stop {"type":"content_block_stop","index":0}`,
		`message_delta {"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{}}`,
		`message_delta {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{}}`,
		`message_stop {"type":"message_stop"}`))
	if last := events[len(events)-1]; last.Type != "response.completed" || last.Response.Output[0].Status != "completed" || events[len(events)-2].Item.Status != "completed" {
		t.Errorf("a later stop reason: %+v", events)
	}
}

func TestResponsesStreamEnds(t *testing.T) {
	start := `message_start {"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":0}}}`
	events := respEvents(t, respStream(t, start,
		`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"cut"}}`,
		`content_block_stop {"type":"content_block_stop","index":0}`,
		`message_delta {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":4}}`,
		`message_stop {"type":"message_stop"}`))
	if last := events[len(events)-1]; last.Type != "response.incomplete" || last.Response.IncompleteDetails.Reason != "max_output_tokens" ||
		last.Response.OutputText() != "cut" || last.Response.Output[0].Status != "incomplete" {
		t.Errorf("last event %s %+v", last.Type, last.Response)
	}
	// The item the stream stopped in is done as incomplete, which its block's
	// stop, before the stop reason arrives, cannot yet say.
	for _, e := range events {
		if e.Type == "response.output_item.done" && e.Item.Status != "incomplete" {
			t.Errorf("done item status %q", e.Item.Status)
		}
	}
	events = respEvents(t, respStream(t, start,
		`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`content_block_delta {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half"}}`,
		`error {"type":"error","error":{"type":"overloaded_error","message":"busy"}}`))
	last := events[len(events)-1]
	if last.Type != "response.failed" || last.Response.Status != "failed" || last.Response.Error.Code != "server_error" ||
		last.Response.Error.Message != "busy" || len(last.Response.Output) != 0 {
		t.Errorf("last event %s %+v", last.Type, last.Response)
	}
	// An item finished before the failure is done, completed, before it.
	events = respEvents(t, respStream(t, start,
		`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"whole"}}`,
		`content_block_stop {"type":"content_block_stop","index":0}`,
		`error {"type":"error","error":{"type":"overloaded_error","message":"busy"}}`))
	if done := events[len(events)-2]; done.Type != "response.output_item.done" || done.Item.Status != "completed" ||
		events[len(events)-1].Response.OutputText() != "whole" {
		t.Errorf("events %+v", events)
	}
	// A stream that names no stop reason ends its last item completed.
	events = respEvents(t, respStream(t, start,
		`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"whole"}}`,
		`content_block_stop {"type":"content_block_stop","index":0}`,
		`message_stop {"type":"message_stop"}`))
	if done := events[len(events)-2]; done.Type != "response.output_item.done" || done.Item.Status != "completed" {
		t.Errorf("events %+v", events)
	}
	s := convert.NewResponsesStream(meta, 1<<20)
	events = respEvents(t, s.Failure("rate_limit_error", "slow down"))
	if len(events) != 3 || events[2].Response.Error.Code != "rate_limit_exceeded" || s.Failure("x", "y") != nil {
		t.Errorf("failure before any event: %+v", events)
	}
	if _, err := convert.NewResponsesStream(meta, 1<<20).Event("content_block_start",
		[]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"s"}}`)); err == nil {
		t.Error("a server tool's block converted")
	}
	// A stream stopping with a block still open has lost that block's
	// content, which the caller was streamed: it does not end completed.
	s = convert.NewResponsesStream(meta, 1<<20)
	for _, ev := range []string{start,
		`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`content_block_delta {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		`message_delta {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`} {
		name, data, _ := strings.Cut(ev, " ")
		if _, err := s.Event(name, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := s.Event("message_stop", []byte(`{"type":"message_stop"}`)); err == nil || bytes.Contains(b, []byte("response.completed")) {
		t.Errorf("a stream stopping with a block open: %s, %v", b, err)
	}
}
