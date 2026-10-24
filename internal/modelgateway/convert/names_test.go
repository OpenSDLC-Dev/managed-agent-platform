package convert_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/convert"
)

// A Messages tool name runs to 128 characters and a Chat Completions function
// name to 64, so a longer one goes out shortened, the same way every time, and
// an answer's name comes back as the tool's own (#981).
func TestFunctionNameFitsChatCompletions(t *testing.T) {
	short := strings.Repeat("a", 64)
	if got := convert.FunctionName(short); got != short {
		t.Errorf("FunctionName of 64 characters = %q, want it unchanged", got)
	}
	long := "mcp__" + strings.Repeat("s", 50) + "__" + strings.Repeat("t", 71) // 128
	got := convert.FunctionName(long)
	if len(got) != 64 || !strings.HasPrefix(got, long[:47]+"_") {
		t.Errorf("FunctionName(128 characters) = %q (%d), want 64 starting with the name's first 47 and _", got, len(got))
	}
	if again := convert.FunctionName(long); again != got {
		t.Errorf("FunctionName is not stable: %q then %q", got, again)
	}
	if other := convert.FunctionName(long[:127] + "u"); other == got {
		t.Errorf("two names that differ only in their last byte shorten alike: %q", got)
	}
	names := convert.NamesOf([]json.RawMessage{
		json.RawMessage(`{"name":"` + short + `"}`), json.RawMessage(`{"name":"` + long + `"}`),
	})
	if back := names.Tool(got); back != long {
		t.Errorf("Tool(%q) = %q, want the tool's own name", got, back)
	}
	if back := names.Tool(short); back != short {
		t.Errorf("Tool(%q) = %q, want a name that was not shortened kept", short, back)
	}
	if back := convert.Names(nil).Tool("f"); back != "f" {
		t.Errorf("the zero Names read %q as %q", "f", back)
	}
}

// A name whose 47th byte falls inside a rune is cut at the rune before it, so
// the function name stays valid UTF-8 and reads back.
func TestFunctionNameCutsAtARune(t *testing.T) {
	long := strings.Repeat("a", 46) + "é" + strings.Repeat("b", 30)
	got := convert.FunctionName(long)
	if !strings.HasPrefix(got, strings.Repeat("a", 46)+"_") || len(got) != 63 {
		t.Errorf("FunctionName = %q (%d), want the cut before é", got, len(got))
	}
}

// Every place a request names a tool sends the same function name: its tools,
// tool_choice, and the calls its history made. A shortened name that another
// tool already takes is refused rather than sent twice.
func TestRequestShortensLongToolNames(t *testing.T) {
	long := strings.Repeat("x", 100)
	fn := convert.FunctionName(long)
	out := request(t, `{"model":"m","max_tokens":5,
		"tools":[{"name":"`+long+`","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"`+long+`"},
		"messages":[{"role":"user","content":"hi"},
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"`+long+`","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`)
	tools := out["tools"].([]any)
	if got := tools[0].(map[string]any)["function"].(map[string]any)["name"]; got != fn {
		t.Errorf("tools[0] name = %v, want %q", got, fn)
	}
	if got := out["tool_choice"].(map[string]any)["function"].(map[string]any)["name"]; got != fn {
		t.Errorf("tool_choice name = %v, want %q", got, fn)
	}
	calls := out["messages"].([]any)[1].(map[string]any)["tool_calls"].([]any)
	if got := calls[0].(map[string]any)["function"].(map[string]any)["name"]; got != fn {
		t.Errorf("history call name = %v, want %q", got, fn)
	}

	var top map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"model":"m","max_tokens":5,`+hi+`,"tools":[
		{"name":"`+long+`","input_schema":{"type":"object"}},{"name":"`+fn+`","input_schema":{"type":"object"}}]}`), &top)
	if _, err := convert.Request(top, "up", nil); err == nil || !strings.Contains(err.Error(), "tools[1]") {
		t.Errorf("Request with a tool already named another's shortened name: err = %v, want tools[1] refused", err)
	}
}

// An answer names the function it was sent, and both converters read it back
// as the tool's own name.
func TestAnswerReadsShortenedNamesBack(t *testing.T) {
	long := strings.Repeat("x", 100)
	fn := convert.FunctionName(long)
	names := convert.NamesOf([]json.RawMessage{json.RawMessage(`{"name":"` + long + `"}`)})
	call := `"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"` + fn + `","arguments":"{}"}}]`

	b, err := convert.Answer([]byte(`{"choices":[{"index":0,"message":{"role":"assistant",`+call+`},"finish_reason":"tool_calls"}]}`),
		"alias", "msg_1", nil, sig, names)
	if err != nil {
		t.Fatal(err)
	}
	var answer struct {
		Content []struct{ Name string } `json:"content"`
	}
	if err := json.Unmarshal(b, &answer); err != nil || len(answer.Content) != 1 || answer.Content[0].Name != long {
		t.Errorf("Answer = %s (%v), want one tool_use named %q", b, err, long)
	}

	s := convert.NewStream("alias", "req_1", sig, names)
	var out []byte
	for _, ch := range []string{
		chunk(`{`+call+`}`, ""),
		chunk(`{"tool_calls":[{"index":0,"function":{"name":"`+fn+`","arguments":""}}]}`, ""), // the name again, unchanged
		chunk(`{}`, "tool_calls"),
	} {
		ev, err := s.Chunk([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev...)
	}
	m, _ := accumulated(t, append(out, s.End()...))
	if len(m.Content) != 1 || m.Content[0].Name != long {
		t.Errorf("streamed content %+v, want one tool_use named %q", m.Content, long)
	}
}

// NamesOf reads a definition's name by its exact key, as Tools does, so a
// definition that also carries "Name" cannot point a call on one tool at
// another.
func TestNamesOfReadsTheNameKeyAlone(t *testing.T) {
	long := strings.Repeat("x", 100)
	fn := convert.FunctionName(long)
	names := convert.NamesOf([]json.RawMessage{json.RawMessage(`{"name":"` + fn + `","Name":"` + long + `"}`)})
	if got := names.Tool(fn); got != fn {
		t.Errorf("Tool(%q) = %q, want the name the tool was sent under", fn, got)
	}
}
