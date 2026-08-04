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

// inlineableMime is the gate between the platform's pinned mime table
// (internal/mimetab, the harvest's source) and the grader's eyes:
// only registry mimes this predicate accepts are inlined into the grading
// prompt, and nothing ties the two packages at compile time (#264). This pins
// the contract from the consumer side — the exact values the harvest table
// publishes for textual deliverables must stay accepted, and its binary
// values must stay rejected — so a predicate change that would silently
// un-inline harvested deliverables fails here.
func TestInlineableMimePinsHarvestTableContract(t *testing.T) {
	accept := []string{
		"application/json",
		"text/calendar; charset=utf-8",
		"text/csv; charset=utf-8",
		"text/markdown; charset=utf-8",
		"text/plain; charset=utf-8",
		"text/tab-separated-values; charset=utf-8",
		"text/vtt; charset=utf-8",
		"text/x-python; charset=utf-8",
		"text/x-rst; charset=utf-8",
		"text/x-sql; charset=utf-8",
		"text/x-tex; charset=utf-8",
		"text/x-toml; charset=utf-8",
		"text/yaml; charset=utf-8",
	}
	for _, m := range accept {
		if !inlineableMime(m) {
			t.Errorf("inlineableMime(%q) = false, want true", m)
		}
	}
	reject := []string{
		"application/octet-stream", // the unknown-extension fallback
		"application/x-tar",
		"application/yaml", // the pre-#264 .yaml value — stays dark
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	}
	for _, m := range reject {
		if inlineableMime(m) {
			t.Errorf("inlineableMime(%q) = true, want false", m)
		}
	}
}
