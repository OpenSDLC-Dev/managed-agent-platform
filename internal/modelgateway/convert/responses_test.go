package convert_test

import (
	"bytes"
	"encoding/json"
	"reflect"
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

func TestResponsesRequestToolChoiceAndEffort(t *testing.T) {
	for in, want := range map[string]string{
		`"tool_choice":"none"`:                                                            `"tool_choice":{"type":"none"}`,
		`"tool_choice":"auto"`:                                                            `"tool_choice":{"type":"auto"}`,
		`"tool_choice":{"type":"function","name":"f"}`:                                    `"tool_choice":{"type":"tool","name":"f"}`,
		`"parallel_tool_calls":false`:                                                     `"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`,
		`"parallel_tool_calls":false,"tool_choice":"none"`:                                `"tool_choice":{"type":"none"}`,
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

func TestResponsesAnswerStatus(t *testing.T) {
	for stop, want := range map[string][2]string{
		"end_turn": {"completed", ""}, "stop_sequence": {"completed", ""}, "tool_use": {"completed", ""},
		"max_tokens": {"incomplete", "max_output_tokens"}, "model_context_window_exceeded": {"incomplete", "max_output_tokens"},
		"refusal": {"incomplete", "content_filter"},
	} {
		r, _ := answer(t, `{"content":[{"type":"text","text":"x"}],"stop_reason":"`+stop+`","usage":{"input_tokens":1,"output_tokens":1}}`)
		if string(r.Status) != want[0] || r.IncompleteDetails.Reason != want[1] {
			t.Errorf("%s: status %s, incomplete %q", stop, r.Status, r.IncompleteDetails.Reason)
		}
	}
	if _, err := convert.ResponsesAnswer([]byte(`{"content":[{"type":"server_tool_use","id":"s"}]}`), meta); err == nil {
		t.Error("a server tool's block converted")
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
	s := convert.NewResponsesStream(meta)
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

func TestResponsesStreamEnds(t *testing.T) {
	start := `message_start {"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":0}}}`
	events := respEvents(t, respStream(t, start,
		`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"cut"}}`,
		`content_block_stop {"type":"content_block_stop","index":0}`,
		`message_delta {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":4}}`,
		`message_stop {"type":"message_stop"}`))
	if last := events[len(events)-1]; last.Type != "response.incomplete" || last.Response.IncompleteDetails.Reason != "max_output_tokens" ||
		last.Response.OutputText() != "cut" {
		t.Errorf("last event %s %+v", last.Type, last.Response)
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
	s := convert.NewResponsesStream(meta)
	events = respEvents(t, s.Failure("rate_limit_error", "slow down"))
	if len(events) != 3 || events[2].Response.Error.Code != "rate_limit_exceeded" || s.Failure("x", "y") != nil {
		t.Errorf("failure before any event: %+v", events)
	}
	if _, err := convert.NewResponsesStream(meta).Event("content_block_start",
		[]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"s"}}`)); err == nil {
		t.Error("a server tool's block converted")
	}
}
