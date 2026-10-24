package convert_test

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

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
	if len(got) != 64 || !strings.HasPrefix(got, long[:23]+"_") || !strings.HasSuffix(got, "_"+long[len(long)-23:]) {
		t.Errorf("FunctionName(128 characters) = %q (%d), want 64: the first 23, _, 16 hex digits, _, the last 23", got, len(got))
	}
	if again := convert.FunctionName(long); again != got {
		t.Errorf("FunctionName is not stable: %q then %q", got, again)
	}
	if other := convert.FunctionName(long[:60] + "u" + long[61:]); other == got {
		t.Errorf("two names that differ only in a byte neither end keeps shorten alike: %q", got)
	}
	names := convert.NamesOf([]json.RawMessage{
		json.RawMessage(`{"name":"` + short + `"}`), json.RawMessage(`{"name":"` + long + `"}`),
	}, nil)
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

// A long MCP server's name is what every tool on it opens with, so the
// shortened name keeps the tool's own name at its end.
func TestFunctionNameKeepsAnMCPToolsOwnName(t *testing.T) {
	got := convert.FunctionName("mcp__acme-corp-internal-knowledge-base-connector__delete_repository")
	if len(got) != 64 || !strings.HasPrefix(got, "mcp__acme-corp-internal") || !strings.HasSuffix(got, "__delete_repository") {
		t.Errorf("FunctionName = %q, want the server's start and the tool's whole name kept", got)
	}
}

// An end that would cut a rune is cut at a rune boundary instead, so the
// function name stays valid UTF-8 and reads back.
func TestFunctionNameCutsAtARune(t *testing.T) {
	long := strings.Repeat("a", 22) + "é" + strings.Repeat("c", 30) + "é" + strings.Repeat("b", 22)
	got := convert.FunctionName(long)
	if !strings.HasPrefix(got, strings.Repeat("a", 22)+"_") || !strings.HasSuffix(got, "_"+strings.Repeat("b", 22)) ||
		len(got) != 62 || !utf8.ValidString(got) {
		t.Errorf("FunctionName = %q (%d), want both cuts outside é", got, len(got))
	}
}

// A call the history made on a tool the request no longer offers reads back
// whole too. Where two names go out alike the later wins: a tool over the
// history, and a later call over an earlier one.
func TestNamesOfReadsAHistoryCallBack(t *testing.T) {
	long := strings.Repeat("x", 100)
	fn := convert.FunctionName(long)
	history := []convert.Message{
		{Role: "user", Content: json.RawMessage(`"hi"`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"calling"},{"type":"tool_use","id":"t1","name":"` + long + `","input":{}}]`)},
	}
	if got := convert.NamesOf(nil, history).Tool(fn); got != long {
		t.Errorf("Tool(%q) = %q, want the history's tool", fn, got)
	}
	tools := []json.RawMessage{json.RawMessage(`{"name":"` + fn + `"}`)}
	if got := convert.NamesOf(tools, history).Tool(fn); got != fn {
		t.Errorf("Tool(%q) = %q, want the tool the request offers under that name", fn, got)
	}
	later := append(history, convert.Message{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"t2","name":"` + fn + `","input":{}}]`)})
	if got := convert.NamesOf(nil, later).Tool(fn); got != fn {
		t.Errorf("Tool(%q) = %q, want the later call's tool", fn, got)
	}
}

// Every place a request names a tool sends the same function name: its tools,
// tool_choice, and the calls its history made. Two tools that would go out
// under one name are refused, in either order; a name listed twice is not.
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

	tool := func(name string) string { return `{"name":"` + name + `","input_schema":{"type":"object"}}` }
	for _, c := range []struct {
		tools   string
		refused bool
	}{
		{tool(long) + "," + tool(fn), true},
		{tool(fn) + "," + tool(long), true},
		{tool(long) + "," + tool(long), false},
	} {
		var top map[string]json.RawMessage
		_ = json.Unmarshal([]byte(`{"model":"m","max_tokens":5,`+hi+`,"tools":[`+c.tools+`]}`), &top)
		_, err := convert.Request(top, "up", nil)
		if refused := err != nil && strings.Contains(err.Error(), "tools[1]") && strings.Contains(err.Error(), "goes out as"); refused != c.refused {
			t.Errorf("tools %s: err = %v, want refused %v", c.tools[:20], err, c.refused)
		}
	}
}

// An answer names the function it was sent, and both converters read it back
// as the tool's own name.
func TestAnswerReadsShortenedNamesBack(t *testing.T) {
	long := strings.Repeat("x", 100)
	fn := convert.FunctionName(long)
	names := convert.NamesOf([]json.RawMessage{json.RawMessage(`{"name":"` + long + `"}`)}, nil)
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
	names := convert.NamesOf([]json.RawMessage{json.RawMessage(`{"name":"` + fn + `","Name":"` + long + `"}`)}, nil)
	if got := names.Tool(fn); got != fn {
		t.Errorf("Tool(%q) = %q, want the name the tool was sent under", fn, got)
	}
}

// A Messages tool name may start with a digit or a dash, and a Gemini
// function name must start with a letter or "_", so such a name goes out with
// a "_" in front, within Gemini's 128, and an answer's name comes back as the
// tool's own (#985).
func TestGeminiNameStartsWithALetterOrUnderscore(t *testing.T) {
	for name, want := range map[string]string{
		"get_time": "get_time", "Get": "Get", "_private": "_private", "": "",
		"1password": "_1password", "-dash": "_-dash", "9": "_9",
	} {
		if got := convert.GeminiName(name); got != want {
			t.Errorf("GeminiName(%q) = %q, want %q", name, got, want)
		}
	}
	long := "1" + strings.Repeat("x", 127) // 128, and 129 with its "_"
	got := convert.GeminiName(long)
	if len(got) != 128 || !strings.HasPrefix(got, "_1xxx") || !strings.HasSuffix(got, "xxx") {
		t.Errorf("GeminiName(128 characters from a digit) = %q (%d), want 128 from a \"_\"", got, len(got))
	}
	if again := convert.GeminiName(long); again != got {
		t.Errorf("GeminiName is not stable: %q then %q", got, again)
	}
	letter := "a" + strings.Repeat("x", 127)
	if got := convert.GeminiName(letter); got != letter {
		t.Errorf("GeminiName of 128 characters from a letter = %q, want it unchanged", got)
	}
	names := convert.GeminiNamesOf([]json.RawMessage{
		json.RawMessage(`{"name":"1password"}`), json.RawMessage(`{"name":"` + long + `"}`),
		json.RawMessage(`{"name":"get_time"}`),
	}, nil)
	for fn, want := range map[string]string{"_1password": "1password", got: long, "get_time": "get_time"} {
		if back := names.Tool(fn); back != want {
			t.Errorf("Tool(%q) = %q, want %q", fn, back, want)
		}
	}
	// Chat Completions takes a name that starts with a digit, so NamesOf maps
	// none of these.
	if back := convert.NamesOf([]json.RawMessage{json.RawMessage(`{"name":"1password"}`)}, nil).Tool("_1password"); back != "_1password" {
		t.Errorf("NamesOf read _1password as %q", back)
	}
}

// A whole and a streamed Gemini answer name each call as the tool's own.
func TestGeminiAnswerReadsMappedNamesBack(t *testing.T) {
	names := convert.GeminiNamesOf([]json.RawMessage{json.RawMessage(`{"name":"1password"}`)}, nil)
	call := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"c1","name":"_1password","args":{}}}]},"finishReason":"STOP"}],"responseId":"r1"}`

	b, err := convert.GeminiAnswer([]byte(call), "alias", "req_1", nil, wrapSig, names)
	if err != nil {
		t.Fatal(err)
	}
	var answer struct {
		Content []struct{ Name string } `json:"content"`
	}
	if err := json.Unmarshal(b, &answer); err != nil || len(answer.Content) != 1 || answer.Content[0].Name != "1password" {
		t.Errorf("GeminiAnswer = %s (%v), want one tool_use named 1password", b, err)
	}

	s := convert.NewGeminiStream("alias", "req_1", wrapSig, names)
	out, err := s.Chunk([]byte(call))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := accumulated(t, append(out, s.End()...))
	if len(m.Content) != 1 || m.Content[0].Name != "1password" {
		t.Errorf("streamed content %+v, want one tool_use named 1password", m.Content)
	}
}
