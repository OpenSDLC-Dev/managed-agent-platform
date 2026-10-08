package convert_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/convert"
	"github.com/anthropics/anthropic-sdk-go"
)

// streamed converts chunks, each a chunk's data, then ends the stream, and
// returns the events.
func streamed(t *testing.T, usage *convert.Usage, chunks ...string) []byte {
	t.Helper()
	s := convert.NewStream("alias", "req_1", sig)
	var out bytes.Buffer
	for _, c := range chunks {
		b, err := s.Chunk([]byte(c))
		if err != nil {
			t.Fatalf("Chunk(%s): %v", c, err)
		}
		out.Write(b)
	}
	if !s.Finished() {
		t.Errorf("the stream has not finished after %v", chunks)
	}
	if usage != nil {
		s.SetUsage(*usage)
	}
	out.Write(s.End())
	return out.Bytes()
}

// accumulated reads events as anthropic-sdk-go does: each event's data
// decoded as the event its name says, accumulated into the message. It
// fails on an event out of the order Accumulate takes them in.
func accumulated(t *testing.T, events []byte) (anthropic.Message, []string) {
	t.Helper()
	var m anthropic.Message
	var names []string
	for _, ev := range strings.Split(strings.TrimSuffix(string(events), "\n\n"), "\n\n") {
		name, data, ok := strings.Cut(ev, "\ndata: ")
		if !ok || !strings.HasPrefix(name, "event: ") {
			t.Fatalf("not an event: %q", ev)
		}
		name = strings.TrimPrefix(name, "event: ")
		var e anthropic.MessageStreamEventUnion
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			t.Fatalf("%s: %v", data, err)
		}
		if e.Type != name {
			t.Errorf("event %s carries %s", name, e.Type)
		}
		if err := m.Accumulate(e); err != nil {
			t.Fatalf("Accumulate(%s): %v\n%s", data, err, events)
		}
		names = append(names, name)
	}
	return m, names
}

func chunk(delta string, finish string) string {
	f := "null"
	if finish != "" {
		f = fmt.Sprintf("%q", finish)
	}
	return fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"up","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`, delta, f)
}

func TestStream(t *testing.T) {
	for _, c := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{"text", []string{chunk(`{"role":"assistant","content":""}`, ""), chunk(`{"content":"Hel"}`, ""), chunk(`{"content":"lo"}`, ""), chunk(`{}`, "stop")},
			"text(Hello) / end_turn"},
		{"reasoning, then text, then calls", []string{
			chunk(`{"role":"assistant","reasoning_content":"thi"}`, ""), chunk(`{"reasoning_content":"nk","content":null}`, ""),
			chunk(`{"content":"calling"}`, ""),
			chunk(`{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"get_time","arguments":""}}]}`, ""),
			chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"tz\":"}}]}`, ""),
			chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"\"UTC\"}"}}]}`, ""),
			chunk(`{"tool_calls":[{"index":1,"id":"c2","type":"function","function":{"name":"get_date","arguments":"{}"}}]}`, ""),
			chunk(`{}`, "tool_calls")},
			`thinking(think|mapgw1.gwdep_a.) text(calling) tool_use(c1 get_time {"tz":"UTC"}) tool_use(c2 get_date {}) / tool_use`},
		{"every call in one chunk, none with arguments", []string{
			chunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":""}},{"index":1,"id":"c2","function":{"name":"b"}}]}`, "tool_calls")},
			"tool_use(c1 a {}) tool_use(c2 b {}) / tool_use"},
		{"text after a call, reasoning after text", []string{
			chunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":"{}"}}]}`, ""),
			chunk(`{"content":"and"}`, ""), chunk(`{"reasoning_content":"more"}`, ""), chunk(`{}`, "stop")},
			"tool_use(c1 a {}) text(and) thinking(more|mapgw1.gwdep_a.) / tool_use"},
		{"refusal", []string{chunk(`{"refusal":"no"}`, "content_filter")}, "text(no) / refusal"},
		{"length", []string{chunk(`{"content":"cu"}`, "length")}, "text(cu) / max_tokens"},
		{"a usage chunk after the finish", []string{chunk(`{"content":"hi"}`, ""), chunk(`{}`, "stop"),
			`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3}}`}, "text(hi) / end_turn"},
	} {
		m, names := accumulated(t, streamed(t, &convert.Usage{Input: 6, Output: 3, CacheRead: 4}, c.chunks...))
		if got := summary(m); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
		if names[0] != "message_start" || names[len(names)-2] != "message_delta" || names[len(names)-1] != "message_stop" {
			t.Errorf("%s: events %v", c.name, names)
		}
		if m.ID != "chatcmpl-1" || m.Model != "alias" || m.Usage.InputTokens != 6 || m.Usage.OutputTokens != 3 || m.Usage.CacheReadInputTokens != 4 {
			t.Errorf("%s: id %s, model %s, usage %+v", c.name, m.ID, m.Model, m.Usage)
		}
	}
}

// A stream with no chunk but its end is an empty message, and one naming
// no id takes the id given.
func TestStreamEmpty(t *testing.T) {
	s := convert.NewStream("alias", "req_1", sig)
	m, names := accumulated(t, s.End())
	if m.ID != "req_1" || len(m.Content) != 0 || m.StopReason != "end_turn" || strings.Join(names, ",") != "message_start,message_delta,message_stop" {
		t.Errorf("%+v %v", m, names)
	}
}

// A choice that goes on after its finish has not finished, and a chunk
// that only repeats an empty value is no going on.
func TestStreamFinish(t *testing.T) {
	s := convert.NewStream("alias", "req_1", sig)
	for _, c := range []struct {
		chunk    string
		finished bool
	}{
		{chunk(`{"content":"a"}`, ""), false},
		{chunk(`{}`, "stop"), true},
		{chunk(`{"content":""}`, ""), true},
		{`{"choices":[],"usage":{"prompt_tokens":1}}`, true},
		{chunk(`{"content":"b"}`, ""), false},
	} {
		if _, err := s.Chunk([]byte(c.chunk)); err != nil {
			t.Fatal(err)
		}
		if s.Finished() != c.finished {
			t.Errorf("after %s finished = %v, want %v", c.chunk, s.Finished(), c.finished)
		}
	}
}

func TestStreamRefusals(t *testing.T) {
	for _, c := range []struct {
		chunks []string
		want   string
	}{
		{[]string{`[]`}, "not a JSON object"},
		{[]string{`{"choices":[{"index":1,"delta":{"content":"x"}}]}`}, "other than the first"},
		{[]string{chunk(`{"function_call":{"name":"f"}}`, "")}, "function_call"},
		{[]string{chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"a"}}]}`, ""),
			chunk(`{"tool_calls":[{"index":1,"id":"b","function":{"name":"b"}}]}`, ""),
			chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}`, "")}, "went back to tool call 0"},
		{[]string{chunk(`{"tool_calls":[{"index":"0"}]}`, "")}, "index"},
		{[]string{chunk(`{"content":[{"type":"text","text":"x"}]}`, "")}, "content"},
		{[]string{chunk(`{"reasoning_content":7}`, "")}, "reasoning_content"},
		{[]string{chunk(`{"tool_calls":[{"index":0,"id":"c","function":{"name":"f","arguments":7}}]}`, "")}, "neither a string nor an object"},
		{[]string{chunk(`{"tool_calls":[{"id":"call_a","function":{"name":"f","arguments":"{}"}}]}`, ""),
			chunk(`{"tool_calls":[{"id":"call_b","function":{"name":"g","arguments":"{}"}}]}`, "")}, "name or id"},
		{[]string{chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"get_"}}]}`, ""),
			chunk(`{"tool_calls":[{"index":0,"function":{"name":"time"}}]}`, "")}, "name or id"},
		{[]string{chunk(`{"tool_calls":[{"index":0,"function":{"name":"f"}}]}`, ""),
			chunk(`{"tool_calls":[{"index":0,"id":"call_1","function":{"arguments":"{}"}}]}`, "")}, "name or id"},
		{[]string{chunk(`{"tool_calls":{}}`, "")}, "tool_calls"},
	} {
		s := convert.NewStream("alias", "req_1", sig)
		var err error
		for _, ch := range c.chunks {
			if _, err = s.Chunk([]byte(ch)); err != nil {
				break
			}
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want an error saying %q", c.chunks, err, c.want)
		}
	}
}

func TestPing(t *testing.T) {
	var e anthropic.MessageStreamEventUnion
	name, data, _ := strings.Cut(strings.TrimSuffix(string(convert.Ping()), "\n\n"), "\ndata: ")
	if err := json.Unmarshal([]byte(data), &e); err != nil || name != "event: ping" || e.Type != "ping" {
		t.Errorf("%q: %v", convert.Ping(), err)
	}
}

// A tool call's id and name sent again with each fragment, unchanged, are a
// repeat: the call is named once and its arguments joined.
func TestStreamRepeatedToolCallFields(t *testing.T) {
	s := convert.NewStream("alias", "req_1", sig)
	var out []byte
	for _, ch := range []string{
		chunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":"}}]}`, ""),
		chunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_time","arguments":"\"UTC\"}"}}]}`, ""),
		chunk(`{}`, "tool_calls"),
	} {
		b, err := s.Chunk([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	m, _ := accumulated(t, append(out, s.End()...))
	if len(m.Content) != 1 || m.Content[0].Name != "get_time" || m.Content[0].ID != "call_1" || string(m.Content[0].Input) != `{"tz":"UTC"}` {
		t.Errorf("content %+v", m.Content)
	}
}

// A stream the token limit cut short in a tool call stops for max_tokens.
func TestStreamCutShortInACall(t *testing.T) {
	s := convert.NewStream("alias", "req_1", sig)
	var out []byte
	for _, ch := range []string{
		chunk(`{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{\"a\":"}}]}`, ""),
		chunk(`{}`, "length"),
	} {
		b, err := s.Chunk([]byte(ch))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	if m, _ := accumulated(t, append(out, s.End()...)); m.StopReason != "max_tokens" {
		t.Errorf("stop_reason %s", m.StopReason)
	}
}

// A streamed refusal stops for refusal, and tool arguments sent as a JSON
// object are the input.
func TestStreamVendorShapes(t *testing.T) {
	for _, c := range []struct {
		chunks []string
		check  func(anthropic.Message) bool
	}{
		{[]string{chunk(`{"refusal":"No"}`, ""), chunk(`{}`, "stop")},
			func(m anthropic.Message) bool {
				return m.StopReason == "refusal" && len(m.Content) == 1 && m.Content[0].Text == "No"
			}},
		{[]string{chunk(`{"tool_calls":[{"index":0,"id":"c","function":{"name":"f","arguments":{"x":1}}}]}`, ""), chunk(`{}`, "tool_calls")},
			func(m anthropic.Message) bool { return len(m.Content) == 1 && string(m.Content[0].Input) == `{"x":1}` }},
	} {
		s := convert.NewStream("alias", "req_1", sig)
		var out []byte
		for _, ch := range c.chunks {
			b, err := s.Chunk([]byte(ch))
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, b...)
		}
		if m, _ := accumulated(t, append(out, s.End()...)); !c.check(m) {
			t.Errorf("%v: %+v", c.chunks, m)
		}
	}
}
