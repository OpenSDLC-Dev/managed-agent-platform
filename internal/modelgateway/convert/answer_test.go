package convert_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/convert"
	"github.com/anthropics/anthropic-sdk-go"
)

func sig() string { return "mapgw1.gwdep_a." }

// message decodes a converted answer as anthropic-sdk-go reads one.
func message(t *testing.T, b []byte) anthropic.Message {
	t.Helper()
	var m anthropic.Message
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return m
}

// summary is a message's content as one comparable line.
func summary(m anthropic.Message) string {
	var parts []string
	for _, b := range m.Content {
		switch b.Type {
		case "thinking":
			parts = append(parts, "thinking("+b.Thinking+"|"+b.Signature+")")
		case "text":
			parts = append(parts, "text("+b.Text+")")
		case "tool_use":
			parts = append(parts, "tool_use("+b.ID+" "+b.Name+" "+string(b.Input)+")")
		default:
			parts = append(parts, b.Type)
		}
	}
	return strings.Join(parts, " ") + " / " + string(m.StopReason)
}

func TestAnswer(t *testing.T) {
	for _, c := range []struct {
		name, choice, want string
	}{
		{"text", `{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}`, "text(hi) / end_turn"},
		{"reasoning first, and every call", `{"message":{"role":"assistant","reasoning_content":"think","content":"calling","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"get_time","arguments":"{\"tz\": \"UTC\"}"}},
			{"id":"c2","type":"function","function":{"name":"get_date","arguments":""}}]},"finish_reason":"tool_calls"}`,
			`thinking(think|mapgw1.gwdep_a.) text(calling) tool_use(c1 get_time {"tz":"UTC"}) tool_use(c2 get_date {}) / tool_use`},
		{"a tool turn that says stop", `{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"stop"}`,
			"tool_use(c1 f {}) / tool_use"},
		{"length", `{"message":{"role":"assistant","content":"cut"},"finish_reason":"length"}`, "text(cut) / max_tokens"},
		{"refusal", `{"message":{"role":"assistant","content":null,"refusal":"no"},"finish_reason":"content_filter"}`, "text(no) / refusal"},
		{"cut short", `{"message":{"role":"assistant","content":"par"},"finish_reason":"insufficient_system_resource"}`, "text(par) / end_turn"},
		{"empty", `{"message":{"role":"assistant","content":""},"finish_reason":"stop"}`, " / end_turn"},
	} {
		b, err := convert.Answer([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"up","choices":[`+c.choice+`]}`),
			"alias", "req_1", &convert.Usage{Input: 6, Output: 3, CacheRead: 4}, sig)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		m := message(t, b)
		if got := summary(m); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
		if m.ID != "chatcmpl-1" || m.Model != "alias" || m.Role != "assistant" || m.Type != "message" ||
			m.Usage.InputTokens != 6 || m.Usage.OutputTokens != 3 || m.Usage.CacheReadInputTokens != 4 || m.StopSequence != "" {
			t.Errorf("%s: %s", c.name, b)
		}
	}
}

// An answer naming no id takes the one given; one reporting no usage
// reports zeros.
func TestAnswerFallbacks(t *testing.T) {
	b, err := convert.Answer([]byte(`{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`), "alias", "req_1", nil, sig)
	if err != nil {
		t.Fatal(err)
	}
	if m := message(t, b); m.ID != "req_1" || !bytes.Contains(b, []byte(`"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`)) {
		t.Errorf("%s", b)
	}
}

func TestAnswerRefusals(t *testing.T) {
	for answer, want := range map[string]string{
		`[]`:                                     "not a JSON object",
		`{"choices":[]}`:                         "no choice",
		`{"choices":[{"finish_reason":"stop"}]}`: "no message",
		`{"choices":[{"message":{"function_call":{"name":"f","arguments":"{}"}}}]}`:                           "function_call",
		`{"choices":[{"message":{"tool_calls":[{"id":"c","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`: "not a JSON object",
		`{"choices":[{"message":{"tool_calls":[{"id":"c","function":{"name":"f","arguments":"[1]"}}]}}]}`:     "not a JSON object",
		`{"choices":[{"message":{"tool_calls":{}}}]}`:                                                         "tool_calls",
	} {
		if _, err := convert.Answer([]byte(answer), "alias", "req_1", nil, sig); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Answer(%s) = %v, want an error saying %q", answer, err, want)
		}
	}
}

// Messages has a reason for each finish Chat Completions names: the token
// limit wins, then a tool call whatever the finish says, then a refusal.
func TestStopReason(t *testing.T) {
	for _, c := range []struct {
		finish          string
		called, refused bool
		want            string
	}{
		{"stop", false, false, "end_turn"}, {"length", false, false, "max_tokens"}, {"content_filter", false, false, "refusal"},
		{"tool_calls", false, false, "end_turn"}, {"aborted", false, false, "end_turn"}, {"", false, false, "end_turn"},
		{"length", true, false, "max_tokens"}, {"stop", true, false, "tool_use"}, {"content_filter", true, false, "tool_use"},
		{"stop", false, true, "refusal"}, {"length", false, true, "max_tokens"}, {"tool_calls", true, true, "tool_use"},
	} {
		if got := convert.StopReason(c.finish, c.called, c.refused); got != c.want {
			t.Errorf("StopReason(%q, %v, %v) = %s, want %s", c.finish, c.called, c.refused, got, c.want)
		}
	}
}

// A tool call the token limit cut short is left out of a whole answer, which
// stops for max_tokens; one whose arguments are whole stays.
func TestAnswerCutShort(t *testing.T) {
	b, err := convert.Answer([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"calling","tool_calls":[
		{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},
		{"id":"c2","type":"function","function":{"name":"g","arguments":"{\"a\":"}}]},"finish_reason":"length"}]}`), "alias", "msg_1", nil, sig)
	if err != nil {
		t.Fatal(err)
	}
	var m anthropic.Message
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Content) != 2 || m.Content[0].Text != "calling" || m.Content[1].ID != "c1" || m.StopReason != "max_tokens" {
		t.Errorf("answer %s", b)
	}
}

// A content, refusal or reasoning_content that is not a string is refused,
// rather than read as no text.
func TestAnswerNonStringText(t *testing.T) {
	for _, key := range []string{"content", "refusal", "reasoning_content"} {
		_, err := convert.Answer([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","`+key+`":[{"type":"text","text":"hello"}]},"finish_reason":"stop"}]}`), "alias", "msg_1", nil, sig)
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: %v, want an error naming it", key, err)
		}
	}
}

// A refusal, which OpenAI sends in place of content and ends with stop, is
// the answer's text and its stop reason; tool arguments sent as a JSON
// object are read as its JSON; and cache writes are counted apart.
func TestAnswerVendorShapes(t *testing.T) {
	b, err := convert.Answer([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":"No"},"finish_reason":"stop"}]}`), "alias", "msg_1", nil, sig)
	var m anthropic.Message
	if err != nil || json.Unmarshal(b, &m) != nil || len(m.Content) != 1 || m.Content[0].Text != "No" || m.StopReason != "refusal" {
		t.Errorf("refusal: %s, %v", b, err)
	}
	b, err = convert.Answer([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":{"x":1}}}]},"finish_reason":"tool_calls"}]}`),
		"alias", "msg_1", &convert.Usage{Input: 10, Output: 10, CacheWrite: 50, CacheRead: 40}, sig)
	m = anthropic.Message{}
	if err != nil || json.Unmarshal(b, &m) != nil || len(m.Content) != 1 || string(m.Content[0].Input) != `{"x":1}` ||
		m.Usage.InputTokens != 10 || m.Usage.CacheCreationInputTokens != 50 || m.Usage.CacheReadInputTokens != 40 {
		t.Errorf("object arguments and usage: %s, %v", b, err)
	}
	if _, err := convert.Answer([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":7}}]},"finish_reason":"tool_calls"}]}`), "alias", "msg_1", nil, sig); err == nil || !strings.Contains(err.Error(), "neither a string nor an object") {
		t.Errorf("number arguments: %v", err)
	}
}
