package convert_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/convert"
)

// sseData is each data line of a recorded stream, its CRLF framing as
// Gemini sends it.
func sseData(raw []byte) []string {
	var data []string
	for _, ev := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n\n") {
		if d, ok := strings.CutPrefix(ev, "data: "); ok {
			data = append(data, d)
		}
	}
	return data
}

// geminiStreamed converts chunks, then ends the stream, and returns the
// events.
func geminiStreamed(t *testing.T, chunks ...string) []byte {
	t.Helper()
	s := convert.NewGeminiStream("alias", "req_1", wrapSig)
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
	s.SetUsage(convert.Usage{Input: 12, Output: 351})
	out.Write(s.End())
	return out.Bytes()
}

// The recorded streams accumulate, as anthropic-sdk-go reads them, to the
// message the whole answer would make: thought summaries leading under the
// bare Gemini prefix, the text whose signature rides on an empty last part
// left out; and a parallel call's signature, on its first call alone, leading
// in an empty block.
func TestGeminiStreamRecorded(t *testing.T) {
	m, names := accumulated(t, geminiStreamed(t, sseData(recorded(t, "stream_text.sse"))...))
	got := summary(m)
	if !strings.HasPrefix(got, "thinking(**Generating the sequence**") ||
		!strings.HasSuffix(got, "|mapgw1.gwdep_g.gemini:) text(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30) / end_turn") {
		t.Errorf("text stream: %s", got)
	}
	if m.ID != "5ELKapvUAs2XktwPj_KPqAE" || m.Model != "alias" || m.Usage.InputTokens != 12 || m.Usage.OutputTokens != 351 {
		t.Errorf("message: %+v", m)
	}
	if names[0] != "message_start" || names[len(names)-2] != "message_delta" || names[len(names)-1] != "message_stop" {
		t.Errorf("events: %v", names)
	}

	raw := recorded(t, "stream_tools.sse")
	m, _ = accumulated(t, geminiStreamed(t, sseData(raw)...))
	want := "thinking(|mapgw1.gwdep_g.gemini:" + signatureOf(t, raw) + ") " +
		`tool_use(call_446465 get_weather {"city":"Paris"}) tool_use(call_446466 get_weather {"city":"Tokyo"}) ` +
		`tool_use(call_446469 get_weather {"city":"Lima"}) / tool_use`
	if got := summary(m); got != want {
		t.Errorf("tool stream:\n got  %s\n want %s", got, want)
	}
}

// The leading thinking block opens on a thought or on a signed call that
// comes first, and closes at the first text or call, which it can no longer
// reach after that: a thought, or a call's signature, that comes later goes
// nowhere. A call Gemini names no id for gets the one the whole answer
// would give it.
func TestGeminiStreamLeadingBlock(t *testing.T) {
	const (
		text     = `{"candidates":[{"content":{"parts":[{"text":"calling"}]}}],"responseId":"r1"}`
		thought  = `{"candidates":[{"content":{"parts":[{"text":"hmm","thought":true}]}}],"responseId":"r1"}`
		signed   = `{"candidates":[{"content":{"parts":[{"functionCall":{"id":"c1","name":"f","args":{"a":1}},"thoughtSignature":"c2ln"}]}}],"responseId":"r1"}`
		unsigned = `{"candidates":[{"content":{"parts":[{"functionCall":{"id":"c1","name":"f"}}]}}],"responseId":"r1"}`
		stop     = `{"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}],"responseId":"r1"}`
	)
	for name, c := range map[string]struct {
		chunks []string
		want   string
	}{
		"an unsigned call first":    {[]string{unsigned, stop}, "tool_use(c1 f {}) / tool_use"},
		"a signed call first":       {[]string{signed, stop}, `thinking(|mapgw1.gwdep_g.gemini:c2ln) tool_use(c1 f {"a":1}) / tool_use`},
		"thoughts, then a call":     {[]string{thought, thought, signed, stop}, `thinking(hmmhmm|mapgw1.gwdep_g.gemini:c2ln) tool_use(c1 f {"a":1}) / tool_use`},
		"text, then a signed call":  {[]string{text, signed, stop}, `text(calling) tool_use(c1 f {"a":1}) / tool_use`},
		"a thought after a call":    {[]string{unsigned, thought, stop}, "tool_use(c1 f {}) / tool_use"},
		"a thought after text":      {[]string{text, thought, text, stop}, "text(callingcalling) / end_turn"},
		"thoughts alone":            {[]string{thought, stop}, "thinking(hmm|mapgw1.gwdep_g.gemini:) / end_turn"},
		"nothing but the finish":    {[]string{stop}, " / end_turn"},
		"a cap":                     {[]string{text, `{"candidates":[{"finishReason":"MAX_TOKENS"}]}`}, "text(calling) / max_tokens"},
		"a safety stop":             {[]string{text, `{"candidates":[{"finishReason":"SAFETY"}]}`}, "text(calling) / refusal"},
		"a prompt blocked outright": {[]string{`{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"},"usageMetadata":{"promptTokenCount":9}}`}, " / refusal"},
	} {
		m, _ := accumulated(t, geminiStreamed(t, c.chunks...))
		if got := summary(m); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", name, got, c.want)
		}
	}

	noID := `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f"}},{"functionCall":{"name":"g"}}]},"finishReason":"STOP"}],"responseId":"r9"}`
	streamedIDs, _ := accumulated(t, geminiStreamed(t, noID))
	whole, err := convert.GeminiAnswer([]byte(noID), "alias", "req_1", nil, wrapSig)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := summary(streamedIDs), summary(message(t, whole)); got != want || !strings.HasPrefix(streamedIDs.Content[0].ID, "toolu_") {
		t.Errorf("ids: streamed %s, whole %s", got, want)
	}
}

// A stream has not finished until a candidate names its finish reason, or
// the prompt is blocked, and not once it goes on after one.
func TestGeminiStreamFinished(t *testing.T) {
	s := convert.NewGeminiStream("alias", "req_1", wrapSig)
	if _, err := s.Chunk([]byte(`{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"trafficType":"ON_DEMAND"}}`)); err != nil || s.Finished() {
		t.Fatalf("finished %v, %v", s.Finished(), err)
	}
	if _, err := s.Chunk([]byte(`{"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}]}`)); err != nil || !s.Finished() {
		t.Fatalf("finished %v, %v", s.Finished(), err)
	}
	// One that goes on after its finish has not finished.
	if _, err := s.Chunk([]byte(`{"candidates":[{"content":{"parts":[{"text":"more"}]}}]}`)); err != nil || s.Finished() {
		t.Fatalf("after more: finished %v, %v", s.Finished(), err)
	}
}

// A chunk it cannot carry fails, naming what it could not.
func TestGeminiStreamRefusals(t *testing.T) {
	for chunk, want := range map[string]string{
		`[1]`:               "not a JSON object",
		`{"candidates":{}}`: "candidates",
		`{"candidates":[{"content":{"parts":{}}}]}`:                                                          "parts",
		`{"candidates":[{"content":{"parts":[{"inlineData":{"data":"eA=="}}]}}]}`:                            "inlineData",
		`{"candidates":[{"content":{"parts":[{"functionCall":{"args":{}}}]}}]}`:                              "no name",
		`{"candidates":[{"content":{"parts":[{"text":7}]}}]}`:                                                "text is not a string",
		`{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL","finishMessage":"call f was malformed"}]}`: "MALFORMED_FUNCTION_CALL: call f was malformed",
	} {
		s := convert.NewGeminiStream("alias", "req_1", wrapSig)
		if _, err := s.Chunk([]byte(chunk)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want one naming %q", chunk, err, want)
		}
	}
}
