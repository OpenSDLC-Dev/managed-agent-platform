package convert_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/convert"
)

// geminiRequest converts body, a Messages request, to a generateContent one
// and decodes what it made.
func geminiRequest(t *testing.T, body string) map[string]any {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatal(err)
	}
	b, err := convert.GeminiRequest(top)
	if err != nil {
		t.Fatalf("GeminiRequest(%s): %v", body, err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Each field the Messages API takes at the SDK's pin has its disposition:
// mapped to its generateContent counterpart, or dropped as a vendor may
// ignore it. A budget wins over an effort, as Gemini refuses both at once.
func TestGeminiRequestFieldDispositions(t *testing.T) {
	got := geminiRequest(t, `{"model":"alias","max_tokens":64,`+hi+`,"system":"be brief","temperature":0.5,"top_p":0.9,
		"top_k":40,"stop_sequences":["END"],"stream":true,"metadata":{"user_id":"u"},"service_tier":"auto",
		"inference_geo":"us","container":"c","cache_control":{"type":"ephemeral"},"thinking":{"type":"enabled","budget_tokens":2048},
		"output_config":{"effort":"high"},"tool_choice":{"type":"auto","disable_parallel_tool_use":true},
		"tools":[{"name":"get_time","description":"time","input_schema":{"type":"object"},"strict":false,"cache_control":{"type":"ephemeral"},
			"eager_input_streaming":true,"input_examples":[{"tz":"UTC"}],"defer_loading":false,"allowed_callers":["direct"],"type":"custom"}]}`)
	want := decoded(t, `{"systemInstruction":{"parts":[{"text":"be brief"}]},
		"contents":[{"role":"user","parts":[{"text":"hi"}]}],
		"tools":[{"functionDeclarations":[{"name":"get_time","description":"time","parametersJsonSchema":{"type":"object"}}]}],
		"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}},
		"generationConfig":{"maxOutputTokens":64,"temperature":0.5,"topP":0.9,"topK":40,"stopSequences":["END"],
			"thinkingConfig":{"includeThoughts":true,"thinkingBudget":2048}}}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// What a request leaves out, or sets null, it does not ask for.
func TestGeminiRequestNullsAreAbsent(t *testing.T) {
	got := geminiRequest(t, `{"model":"alias","max_tokens":8,`+hi+`,"system":null,"stream":null,"thinking":null,"tools":null,
		"tool_choice":null,"metadata":null,"output_config":{"effort":null},"temperature":null}`)
	want := decoded(t, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":8}}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// A system prompt's text blocks stay parts of their own.
func TestGeminiRequestSystemBlocks(t *testing.T) {
	got := geminiRequest(t, `{"max_tokens":8,`+hi+`,"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b"}]}`)
	if want := decoded(t, `{"parts":[{"text":"a"},{"text":"b"}]}`); !reflect.DeepEqual(got["systemInstruction"], want) {
		t.Errorf("systemInstruction = %v, want %v", got["systemInstruction"], want)
	}
	// An empty prompt is none, in either form: a part needs a field set.
	for _, system := range []string{`""`, `[{"type":"text","text":""}]`} {
		if got := geminiRequest(t, `{"max_tokens":8,`+hi+`,"system":`+system+`}`); got["systemInstruction"] != nil {
			t.Errorf("system %s became %v", system, got["systemInstruction"])
		}
	}
}

// Consecutive assistant messages are one turn, as the Messages API reads
// them: a signature in one goes on the turn's first call in the next.
func TestGeminiRequestSignatureAcrossSplitAssistantMessages(t *testing.T) {
	const (
		thinking = `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"gemini:U0lH"}]}`
		text     = `{"role":"assistant","content":[{"type":"text","text":"calling"}]}`
		calls    = `{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}},{"type":"tool_use","id":"t2","name":"f","input":{}}]}`
	)
	for name, turn := range map[string]string{"signature first": thinking + "," + text + "," + calls, "signature after text": text + "," + thinking + "," + calls} {
		got := geminiRequest(t, `{"max_tokens":8,"messages":[{"role":"user","content":"go"},`+turn+`,
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1"},{"type":"tool_result","tool_use_id":"t2"}]}]}`)
		want := decoded(t, `[{"role":"model","parts":[{"text":"calling"},{"functionCall":{"id":"t1","name":"f","args":{}},"thoughtSignature":"U0lH"},
			{"functionCall":{"id":"t2","name":"f","args":{}}}]}]`)
		if contents := got["contents"].([]any); len(contents) != 3 || !reflect.DeepEqual(contents[1:2], want) {
			t.Errorf("%s: got %v\nwant %v at [1]", name, got["contents"], want)
		}
	}
}

func TestGeminiRequestToolChoice(t *testing.T) {
	const tools = `,"tools":[{"name":"get_time","input_schema":{"type":"object"}}]`
	for in, want := range map[string]string{
		`{"type":"auto"}`:                                 `{"mode":"AUTO"}`,
		`{"type":"any"}`:                                  `{"mode":"ANY"}`,
		`{"type":"none"}`:                                 `{"mode":"NONE"}`,
		`{"type":"tool","name":"get_time"}`:               `{"mode":"ANY","allowedFunctionNames":["get_time"]}`,
		`{"type":"any","disable_parallel_tool_use":true}`: `{"mode":"ANY"}`,
	} {
		got := geminiRequest(t, `{"max_tokens":8,`+hi+tools+`,"tool_choice":`+in+`}`)
		if want := decoded(t, `{"functionCallingConfig":`+want+`}`); !reflect.DeepEqual(got["toolConfig"], want) {
			t.Errorf("tool_choice %s became %v, want %v", in, got["toolConfig"], want)
		}
	}
	// Without a declaration there is nothing to choose among.
	for _, body := range []string{`,"tool_choice":{"type":"any"}`, `,"tools":[],"tool_choice":{"type":"none"}`} {
		if got := geminiRequest(t, `{"max_tokens":8,`+hi+body+`}`); got["toolConfig"] != nil || got["tools"] != nil {
			t.Errorf("%s made tools %v and toolConfig %v", body, got["tools"], got["toolConfig"])
		}
	}
}

// Thinking: a budget where one is set, otherwise the effort as a level;
// summaries unless the caller omits them; nothing for disabled, which Gemini
// cannot do.
func TestGeminiRequestThinking(t *testing.T) {
	for in, want := range map[string]string{
		`"thinking":{"type":"adaptive"}`:                                                      `{"includeThoughts":true}`,
		`"thinking":{"type":"adaptive","display":"summarized"}`:                               `{"includeThoughts":true}`,
		`"thinking":{"type":"adaptive","display":"omitted"}`:                                  ``,
		`"thinking":{"type":"enabled","budget_tokens":1024,"display":"omitted"}`:              `{"thinkingBudget":1024}`,
		`"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"}`:                  `{"includeThoughts":true,"thinkingLevel":"MEDIUM"}`,
		`"thinking":{"type":"enabled","budget_tokens":1024},"output_config":{"effort":"low"}`: `{"includeThoughts":true,"thinkingBudget":1024}`,
		`"thinking":{"type":"disabled"}`:                                                      ``,
		`"thinking":{"type":"disabled"},"output_config":{"effort":"low"}`:                     `{"thinkingLevel":"LOW"}`,
		`"output_config":{"effort":"high"}`:                                                   `{"thinkingLevel":"HIGH"}`,
		`"output_config":{"effort":"xhigh"}`:                                                  `{"thinkingLevel":"HIGH"}`,
		`"output_config":{"effort":"max"}`:                                                    `{"thinkingLevel":"HIGH"}`,
	} {
		got := geminiRequest(t, `{"max_tokens":8,`+hi+`,`+in+`}`)
		cfg := got["generationConfig"].(map[string]any)["thinkingConfig"]
		if want == "" {
			if cfg != nil {
				t.Errorf("%s became %v, want no thinkingConfig", in, cfg)
			}
			continue
		}
		if !reflect.DeepEqual(cfg, decoded(t, want)) {
			t.Errorf("%s became %v, want %s", in, cfg, want)
		}
	}
}

// A tool loop: images inline, calls with their ids, results under the
// called tool's name, an error result as error, an image in a result as a
// part of the response, and the Gemini signature a thinking block carries
// set on the turn's first call — the block itself, and its summary, sent
// nowhere.
func TestGeminiRequestToolLoop(t *testing.T) {
	got := geminiRequest(t, `{"max_tokens":8,"messages":[
		{"role":"user","content":[{"type":"text","text":"Weather?"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBO"}}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"I should call","signature":"gemini:SIG"},{"type":"text","text":"Checking."},
			{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}},
			{"type":"tool_use","id":"toolu_2","name":"get_weather","input":{"city":"Tokyo"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"18C"},
			{"type":"tool_result","tool_use_id":"toolu_2","is_error":true,"content":[{"type":"text","text":"no such city"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},
			{"type":"text","text":"thanks"}]}]}`)
	want := decoded(t, `[
		{"role":"user","parts":[{"text":"Weather?"},{"inlineData":{"mimeType":"image/png","data":"iVBO"}}]},
		{"role":"model","parts":[{"text":"Checking."},
			{"functionCall":{"id":"toolu_1","name":"get_weather","args":{"city":"Paris"}},"thoughtSignature":"SIG"},
			{"functionCall":{"id":"toolu_2","name":"get_weather","args":{"city":"Tokyo"}}}]},
		{"role":"user","parts":[{"functionResponse":{"id":"toolu_1","name":"get_weather","response":{"output":"18C"}}},
			{"functionResponse":{"id":"toolu_2","name":"get_weather","response":{"error":"no such city"},
				"parts":[{"inlineData":{"mimeType":"image/png","data":"AAAA"}}]}},
			{"text":"thanks"}]}]`)
	if !reflect.DeepEqual(got["contents"], want) {
		t.Errorf("got  %v\nwant %v", got["contents"], want)
	}
}

// A call no Gemini signature survives for carries the sentinel Gemini
// accepts in its place: history another protocol produced, a block carrying
// none, or none at all. Thinking that is not Gemini's goes nowhere.
func TestGeminiRequestSignatureSentinel(t *testing.T) {
	for name, thinking := range map[string]string{
		"no thinking":         ``,
		"an empty signature":  `{"type":"thinking","thinking":"","signature":"gemini:"},`,
		"another's signature": `{"type":"thinking","thinking":"hm","signature":"EqQBCkYIBRgCKkB"},`,
		"unsigned reasoning":  `{"type":"thinking","thinking":"hm","signature":""},`,
		"redacted":            `{"type":"redacted_thinking","data":"abc"},`,
	} {
		t.Run(name, func(t *testing.T) {
			got := geminiRequest(t, `{"max_tokens":8,"messages":[{"role":"user","content":"go"},
				{"role":"assistant","content":[`+thinking+`{"type":"tool_use","id":"toolu_1","name":"f","input":{}},{"type":"tool_use","id":"toolu_2","name":"f"}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1"},{"type":"tool_result","tool_use_id":"toolu_2","content":[]}]}]}`)
			want := decoded(t, `[{"role":"user","parts":[{"text":"go"}]},
				{"role":"model","parts":[{"functionCall":{"id":"toolu_1","name":"f","args":{}},"thoughtSignature":"skip_thought_signature_validator"},
					{"functionCall":{"id":"toolu_2","name":"f","args":{}}}]},
				{"role":"user","parts":[{"functionResponse":{"id":"toolu_1","name":"f","response":{"output":""}}},
					{"functionResponse":{"id":"toolu_2","name":"f","response":{"output":""}}}]}]`)
			if !reflect.DeepEqual(got["contents"], want) {
				t.Errorf("got  %v\nwant %v", got["contents"], want)
			}
		})
	}
}

// A search_result in a tool result reaches Gemini as the text the brain's
// flatten_search_results renders.
func TestGeminiRequestSearchResult(t *testing.T) {
	got := geminiRequest(t, `{"max_tokens":8,"messages":[{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"search","input":{"q":"x"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"found:"},
			{"type":"search_result","title":"T","source":"https://s.example","content":[{"type":"text","text":"body"}]}]}]}]}`)
	resp := got["contents"].([]any)[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)["response"].(map[string]any)
	if out, _ := resp["output"].(string); !strings.HasPrefix(out, "found:\n") || !strings.Contains(out, "https://s.example") || !strings.Contains(out, "body") {
		t.Errorf("output = %q", out)
	}
}

// Turns of one role in a row are one turn, as the Messages API reads them;
// so are those a dropped turn leaves adjacent — an assistant turn holding
// only thinking contributes no part.
func TestGeminiRequestJoinsSameRoleTurns(t *testing.T) {
	got := geminiRequest(t, `{"max_tokens":8,"messages":[{"role":"user","content":"a"},{"role":"user","content":[{"type":"text","text":"b"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"gemini:S"}]},{"role":"user","content":"c"},
		{"role":"assistant","content":"d"},{"role":"user","content":""},{"role":"user","content":[{"type":"text","text":""},{"type":"text","text":"e"}]}]}`)
	want := decoded(t, `[{"role":"user","parts":[{"text":"a"},{"text":"b"},{"text":"c"}]},{"role":"model","parts":[{"text":"d"}]},
		{"role":"user","parts":[{"text":"e"}]}]`)
	if !reflect.DeepEqual(got["contents"], want) {
		t.Errorf("got  %v\nwant %v", got["contents"], want)
	}
}

// The conversion writes no HTML escapes and is deterministic.
func TestGeminiRequestEncoding(t *testing.T) {
	var top map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"max_tokens":8,"messages":[{"role":"user","content":"<a & b>"}],"top_k":1,"temperature":1,"top_p":1}`), &top)
	a, err := convert.GeminiRequest(top)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := convert.GeminiRequest(top)
	if !strings.Contains(string(a), "<a & b>") || string(a) != string(b) {
		t.Errorf("a = %s\nb = %s", a, b)
	}
}

// An image Gemini would have to fetch is refused as such: Gemini fetches no
// URL a caller names.
func TestGeminiRequestRefusesAURLImage(t *testing.T) {
	var top map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"max_tokens":8,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://x.example/a.png"}}]}]}`), &top)
	if _, err := convert.GeminiRequest(top); err == nil || !strings.Contains(err.Error(), `a "url" source has no Gemini counterpart`) {
		t.Errorf("err = %v", err)
	}
}

// What Gemini has no counterpart for, and the caller relies on, is refused,
// the error naming the field as the caller wrote it.
func TestGeminiRequestRefusals(t *testing.T) {
	for body, field := range map[string]string{
		`{` + hi + `}`: "max_tokens",
		`{"max_tokens":8,` + hi + `,"mcp_servers":[]}`: "mcp_servers",
		`{"max_tokens":8,"messages":{}}`:               "messages",
		`{"max_tokens":8,"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`:                                                                                                                                            "messages[1]",
		`{"max_tokens":8,"messages":[{"role":"system","content":"a"}]}`:                                                                                                                                                                             "messages[0].role",
		`{"max_tokens":8,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://x.example/a.png"}}]}]}`:                                                                                                        "messages[0].content[0].source",
		`{"max_tokens":8,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","data":"x"}}]}]}`:                                                                                                                         "messages[0].content[0]",
		`{"max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":1}]}]}`:                                                                                                                                                        "messages[0].content[0].text",
		`{"max_tokens":8,"messages":[{"role":"user","content":7}]}`:                                                                                                                                                                                 "messages[0].content",
		`{"max_tokens":8,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"nope","content":"x"}]}]}`:                                                                                                                       "messages[0].content[0].tool_use_id",
		`{"max_tokens":8,"messages":[{"role":"user","content":"a"},{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":[1]}]},{"role":"user","content":"b"}]}`:                                                            "messages[1].content[0].input",
		`{"max_tokens":8,"messages":[{"role":"user","content":"a"},{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"document"}]}]}]}`: "messages[2].content[0].content[0]",
		`{"max_tokens":8,` + hi + `,"system":[{"type":"image"}]}`:                                                                                                                                                                                   "system[0]",
		`{"max_tokens":8,` + hi + `,"system":7}`:                                                      "system",
		`{"max_tokens":8,` + hi + `,"tools":[{"type":"web_search_20250305","name":"web_search"}]}`:    "tools[0]",
		`{"max_tokens":8,` + hi + `,"tools":[{"name":"f","type":7}]}`:                                 "tools[0].type",
		`{"max_tokens":8,` + hi + `,"tools":[{"name":"f","strict":true}]}`:                            "tools[0].strict",
		`{"max_tokens":8,` + hi + `,"tools":[{"name":"f","defer_loading":true}]}`:                     "tools[0].defer_loading",
		`{"max_tokens":8,` + hi + `,"tools":[{"name":"f","allowed_callers":["code_execution"]}]}`:     "tools[0].allowed_callers",
		`{"max_tokens":8,` + hi + `,"tools":[{"name":"f","colour":"red"}]}`:                           "tools[0].colour",
		`{"max_tokens":8,` + hi + `,"tools":[{"description":"no name"}]}`:                             "tools[0].name",
		`{"max_tokens":8,` + hi + `,"tools":{}}`:                                                      "tools",
		`{"max_tokens":8,` + hi + `,"tool_choice":{"type":"some"}}`:                                   "tool_choice.type",
		`{"max_tokens":8,` + hi + `,"tool_choice":{"type":"tool"}}`:                                   "tool_choice.name",
		`{"max_tokens":8,` + hi + `,"tool_choice":{"type":"auto","disable_parallel_tool_use":"yes"}}`: "tool_choice.disable_parallel_tool_use",
		`{"max_tokens":8,` + hi + `,"thinking":{"type":"on"}}`:                                        "thinking.type",
		`{"max_tokens":8,` + hi + `,"thinking":{"type":"adaptive","budget_tokens":1024}}`:             "thinking.budget_tokens",
		`{"max_tokens":8,` + hi + `,"thinking":{"type":"disabled","display":"omitted"}}`:              "thinking.display",
		`{"max_tokens":8,` + hi + `,"thinking":{"type":"adaptive","display":"full"}}`:                 "thinking.display",
		`{"max_tokens":8,` + hi + `,"output_config":{"format":{"type":"json_schema"}}}`:               "output_config.format",
		`{"max_tokens":8,` + hi + `,"output_config":{"effort":"extreme"}}`:                            "output_config.effort",
		`{"max_tokens":8,` + hi + `,"output_config":{"effort":3}}`:                                    "output_config.effort",
	} {
		var top map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &top); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		_, err := convert.GeminiRequest(top)
		if err == nil || !strings.HasPrefix(err.Error(), field+":") && !strings.HasPrefix(err.Error(), field+".") {
			t.Errorf("%s: err = %v, want one naming %s", body, err, field)
		}
	}
}
