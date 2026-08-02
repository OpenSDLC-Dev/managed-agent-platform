package brain

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// parseVerdict's protocol tolerance: the three verdicts parse with their
// explanation, a VERDICT line anywhere near the end wins, and an unknown or
// missing verdict reads as needs_revision with the full reply kept.
func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name, in, result, explanation string
	}{
		{"satisfied", "criteria met\nVERDICT: satisfied", domain.OutcomeResultSatisfied, "criteria met"},
		{"needs revision", "missing tests\nVERDICT: needs_revision", verdictNeedsRevision, "missing tests"},
		{"failed", "rubric contradicts the description\nVERDICT: failed", domain.OutcomeResultFailed, "rubric contradicts the description"},
		{"verdict not last", "before\nVERDICT: satisfied\nafter", domain.OutcomeResultSatisfied, "before\nafter"},
		{"unknown verdict", "hmm\nVERDICT: maybe", verdictNeedsRevision, "hmm\nVERDICT: maybe"},
		{"no verdict line", "just prose, no protocol", verdictNeedsRevision, "just prose, no protocol"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, explanation := parseVerdict(c.in)
			if result != c.result || explanation != c.explanation {
				t.Errorf("parseVerdict(%q) = (%q, %q), want (%q, %q)",
					c.in, result, explanation, c.result, c.explanation)
			}
		})
	}
}

// renderTranscript labels each conversation-bearing event with its role,
// flattens tool calls to name+input, skips non-conversation events, and cuts
// any single item at graderItemBudget.
func TestRenderTranscript(t *testing.T) {
	body := func(v any) []byte {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	long := strings.Repeat("a", graderItemBudget+100)

	out := renderTranscript([]domain.Event{
		{Type: domain.EventUserMessage, Body: body(map[string]any{"content": "hello"})},
		{Type: domain.EventSystemMessage, Body: body(map[string]any{"content": []map[string]any{{"type": "text", "text": "system note"}}})},
		{Type: domain.EventAgentMessage, Body: body(map[string]any{"content": []map[string]any{
			{"type": "text", "text": "first"}, {"type": "tool_use"}, {"type": "text", "text": "second"}}})},
		{Type: domain.EventAgentToolUse, Body: body(map[string]any{"name": "bash", "input": map[string]any{"command": "ls"}})},
		{Type: domain.EventAgentToolResult, Body: body(map[string]any{"content": "ok"})},
		{Type: domain.EventSpanOutcomeEvalEnd, Body: body(map[string]any{"result": "satisfied"})},
		{Type: domain.EventUserMessage, Body: body(map[string]any{"content": long})},
	})

	for _, want := range []string{
		"## user\nhello", "## system\nsystem note", "first\nsecond",
		"## agent tool call\nbash", `"command":"ls"`, "## tool result\nok", "[truncated]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, long) {
		t.Error("item budget not applied to the long message")
	}
	if strings.Contains(out, "satisfied") {
		t.Error("non-conversation event leaked into the transcript")
	}
}

// Once the head fills graderTranscriptBudget, later items stop appending and
// the tail is cut with a truncation note.
func TestRenderTranscriptTotalBudget(t *testing.T) {
	body, err := json.Marshal(map[string]any{"content": strings.Repeat("b", graderItemBudget+10)})
	if err != nil {
		t.Fatal(err)
	}
	var history []domain.Event
	for range graderTranscriptBudget/graderItemBudget + 10 {
		history = append(history, domain.Event{Type: domain.EventUserMessage, Body: body})
	}
	out := renderTranscript(history)
	if !strings.HasSuffix(out, "[transcript truncated]") {
		t.Fatalf("missing transcript truncation note; len=%d", len(out))
	}
	if len(out) > graderTranscriptBudget+len("\n[transcript truncated]") {
		t.Errorf("transcript length %d exceeds the budget", len(out))
	}
}

// contentText's fallbacks: a bare string, a block array, an unknown content
// shape kept raw, and bodies with no readable content at all. A search_result
// block has no top-level text — its title, source, and nested text blocks
// flatten instead (web_search evidence must reach the grader).
func TestContentText(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"string", `{"content":"plain"}`, "plain"},
		{"blocks", `{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`, "a\nb"},
		{"search results", `{"content":[{"type":"search_result","title":"Go 1.26 notes","source":"https://go.dev/doc","content":[{"type":"text","text":"release highlights"}]},{"type":"text","text":"plain"}]}`,
			"Go 1.26 notes\nhttps://go.dev/doc\nrelease highlights\nplain"},
		{"unknown shape", `{"content":{"k":1}}`, `{"k":1}`},
		{"no content", `{}`, ""},
		{"invalid json", `{`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := contentText([]byte(c.body)); got != c.want {
				t.Errorf("contentText(%s) = %q, want %q", c.body, got, c.want)
			}
		})
	}
}
