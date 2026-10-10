package convert_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/convert"
)

// wrapSig stands in for the gateway's provenance wrapper.
func wrapSig(v string) string { return "mapgw1.gwdep_g." + v }

// recorded is an answer gemini-3.8-flash gave on 2026-10-10
// (docs/plan/62_gemini-upstream-protocol.md, "Ground truth").
func recorded(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "gemini", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// signatureOf is the first thoughtSignature a recorded answer carries.
func signatureOf(t *testing.T, b []byte) string {
	t.Helper()
	m := regexp.MustCompile(`"thoughtSignature": "([^"]+)"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("the recording carries no signature")
	}
	return string(m[1])
}

func geminiAnswer(t *testing.T, b []byte) string {
	t.Helper()
	out, err := convert.GeminiAnswer(b, "alias", "req_1", &convert.Usage{Input: 6, Output: 335, CacheRead: 2}, wrapSig)
	if err != nil {
		t.Fatalf("GeminiAnswer(%s): %v", b, err)
	}
	m := message(t, out)
	if m.Model != "alias" || m.Role != "assistant" || m.Type != "message" || m.Usage.InputTokens != 6 || m.Usage.OutputTokens != 335 ||
		m.Usage.CacheReadInputTokens != 2 || m.StopSequence != "" {
		t.Errorf("message = %+v", m)
	}
	return summary(m)
}

// The recorded answers: a text answer's signature is not carried, and so
// leaves no thinking block; thought summaries lead as one, under the bare
// Gemini prefix; and a parallel call's signature, on its first call alone,
// leads in an empty one.
func TestGeminiAnswerRecorded(t *testing.T) {
	if got, want := geminiAnswer(t, recorded(t, "plain.json")), "text(Hello there, how are you?) / end_turn"; got != want {
		t.Errorf("plain:\n got  %s\n want %s", got, want)
	}
	got := geminiAnswer(t, recorded(t, "thoughts.json"))
	if !strings.HasPrefix(got, "thinking(**My Mental Calculation Strategy**") || !strings.HasSuffix(got, "|mapgw1.gwdep_g.gemini:) text(391) / end_turn") {
		t.Errorf("thoughts: %s", got)
	}
	calls := recorded(t, "parallel_calls.json")
	want := "thinking(|mapgw1.gwdep_g.gemini:" + signatureOf(t, calls) + `) tool_use(call_38500 get_weather {"city":"Paris"}) ` +
		`tool_use(call_38501 get_weather {"city":"Tokyo"}) / tool_use`
	if got := geminiAnswer(t, calls); got != want {
		t.Errorf("parallel calls:\n got  %s\n want %s", got, want)
	}
}

// The id is the upstream's responseId, or the gateway's without one.
func TestGeminiAnswerID(t *testing.T) {
	for body, want := range map[string]string{
		`{"responseId":"r-1","candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}]}`: "r-1",
		`{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}]}`:                    "req_1",
	} {
		out, err := convert.GeminiAnswer([]byte(body), "alias", "req_1", nil, wrapSig)
		if err != nil {
			t.Fatal(err)
		}
		if m := message(t, out); m.ID != want || m.Usage.InputTokens != 0 {
			t.Errorf("%s: id %q, usage %+v; want %q and zeros", body, m.ID, m.Usage, want)
		}
	}
}

// Adjacent text parts are one block, an empty part none; a call without an
// id gets one of its own, the same for the same answer and another for each
// call.
func TestGeminiAnswerParts(t *testing.T) {
	body := `{"responseId":"r-2","candidates":[{"content":{"role":"model","parts":[{"text":"a"},{"text":""},{"text":"b"},
		{"functionCall":{"name":"f","args":{"x":1}}},{"functionCall":{"name":"f"}},{"text":"c"},{"text":"","thoughtSignature":"c2ln"}]},"finishReason":"STOP"}]}`
	got := geminiAnswer(t, []byte(body))
	m := regexp.MustCompile(`^text\(ab\) tool_use\((toolu_\w+) f \{"x":1\}\) tool_use\((toolu_\w+) f \{\}\) text\(c\) / tool_use$`).FindStringSubmatch(got)
	if m == nil || m[1] == m[2] {
		t.Fatalf("got %s", got)
	}
	if again := geminiAnswer(t, []byte(body)); again != got {
		t.Errorf("the same answer converted twice: %s, then %s", got, again)
	}
	if other := geminiAnswer(t, []byte(strings.Replace(body, "r-2", "r-3", 1))); strings.Contains(other, m[1]) {
		t.Errorf("another answer reused the id %s: %s", m[1], other)
	}
}

// Each finish reason Gemini documents has a stop reason or an error naming
// it; a prompt blocked before any candidate is an empty refusal.
func TestGeminiAnswerFinishReasons(t *testing.T) {
	answer := func(reason string) string {
		return `{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"` + reason + `","finishMessage":"call f was malformed"}]}`
	}
	for reason, want := range map[string]string{
		"STOP": "text(x) / end_turn", "": "text(x) / end_turn",
		"MAX_TOKENS": "text(x) / max_tokens", "CONTINUATION": "text(x) / max_tokens",
		"SAFETY": "text(x) / refusal", "RECITATION": "text(x) / refusal", "LANGUAGE": "text(x) / refusal",
		"BLOCKLIST": "text(x) / refusal", "PROHIBITED_CONTENT": "text(x) / refusal", "SPII": "text(x) / refusal",
		"IMAGE_SAFETY": "text(x) / refusal", "IMAGE_PROHIBITED_CONTENT": "text(x) / refusal", "IMAGE_RECITATION": "text(x) / refusal",
		"IMAGE_OTHER": "text(x) / refusal",
	} {
		if got := geminiAnswer(t, []byte(answer(reason))); got != want {
			t.Errorf("%q: got %s, want %s", reason, got, want)
		}
	}
	if got := geminiAnswer(t, []byte(`{"candidates":[{"finishReason":"SAFETY"}]}`)); got != " / refusal" {
		t.Errorf("a candidate with no content: %s", got)
	}
	if got := geminiAnswer(t, []byte(`{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"},"usageMetadata":{"promptTokenCount":9}}`)); got != " / refusal" {
		t.Errorf("a blocked prompt: %s", got)
	}
	for _, reason := range []string{"MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL", "TOO_MANY_TOOL_CALLS", "MISSING_THOUGHT_SIGNATURE",
		"MALFORMED_RESPONSE", "OTHER", "NO_IMAGE", "FINISH_REASON_UNSPECIFIED", "SOMETHING_NEW"} {
		_, err := convert.GeminiAnswer([]byte(answer(reason)), "alias", "req_1", nil, wrapSig)
		if err == nil || !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), "call f was malformed") {
			t.Errorf("%s: err = %v, want one naming it and its message", reason, err)
		}
	}
}

// An answer it cannot carry fails, naming what it could not.
func TestGeminiAnswerRefusals(t *testing.T) {
	for body, contains := range map[string]string{
		`[]`:                "not a JSON object",
		`{"candidates":[]}`: "no candidate",
		`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AAAA"}}]},"finishReason":"STOP"}]}`: "inlineData",
		`{"candidates":[{"content":{"parts":[{"executableCode":{"code":"1"}}]},"finishReason":"STOP"}]}`:                       "executableCode",
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":[1]}}]},"finishReason":"STOP"}]}`:              "args",
		`{"candidates":[{"content":{"parts":[{"functionCall":{"args":{}}}]},"finishReason":"STOP"}]}`:                          "name",
		`{"candidates":[{"content":{"parts":[{"text":7}]},"finishReason":"STOP"}]}`:                                            "text",
		`{"candidates":[{"content":{"parts":{}},"finishReason":"STOP"}]}`:                                                      "parts",
	} {
		_, err := convert.GeminiAnswer([]byte(body), "alias", "req_1", nil, wrapSig)
		if err == nil || !strings.Contains(err.Error(), contains) {
			t.Errorf("%s: err = %v, want one naming %s", body, err, contains)
		}
	}
}

// Every field is read by its exact key, as Gemini writes it.
func TestGeminiAnswerExactKeys(t *testing.T) {
	got := geminiAnswer(t, []byte(`{"candidates":[{"content":{"parts":[{"Text":"x","text":"y"},{"text":"t","Thought":true}]},"finishReason":"STOP"}]}`))
	if got != "text(yt) / end_turn" {
		t.Errorf("got %s", got)
	}
}
