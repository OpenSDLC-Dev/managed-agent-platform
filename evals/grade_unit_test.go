package evals

import (
	"strings"
	"testing"
)

// These run on an ordinary `go test ./...` — no model, no Postgres, no Docker.
// They are what keeps the grading logic honest on every PR, since the live
// suite that would exercise it end to end only runs opted-in. A grader that
// passed everything would sail through the live run too; these prove each one
// can fail, and fails on the thing it names.

// trialWith builds a Trial from a hand-written transcript. The nonce is fixed so
// a grader's {{NONCE}} substitution is checkable.
func trialWith(events []map[string]any) *Trial {
	return &Trial{Nonce: "n0", Events: events}
}

func textBlocks(text string) []any {
	return []any{map[string]any{"type": "text", "text": text}}
}

func TestSplitLines(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"\n", nil},
		{"a", []string{"a"}},
		{"a\n", []string{"a"}},
		{"a\nb", []string{"a", "b"}},
		{"a\nb\n", []string{"a", "b"}},
		{"a\n\nb", []string{"a", "", "b"}}, // a blank interior line is content
	}
	for _, c := range cases {
		got := splitLines(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitLines(%q) = %q, want %q", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitLines(%q) = %q, want %q", c.in, got, c.want)
				break
			}
		}
	}
}

func TestFinalMessageHas(t *testing.T) {
	g := FinalMessageHas("DONE:{{NONCE}}", Either)
	// Two agent messages: the grader must read the last, and substitute the nonce.
	tr := trialWith([]map[string]any{
		{"type": "agent.message", "content": textBlocks("working on it")},
		{"type": "agent.message", "content": textBlocks("all set, DONE:n0")},
	})
	if err := g.Check(t, tr); err != nil {
		t.Errorf("want pass, got %v", err)
	}

	// The token is present but in an earlier message, not the final one.
	trStale := trialWith([]map[string]any{
		{"type": "agent.message", "content": textBlocks("DONE:n0")},
		{"type": "agent.message", "content": textBlocks("actually, wait")},
	})
	if err := g.Check(t, trStale); err == nil {
		t.Error("want failure when the token is only in a non-final message")
	}
}

func TestToolUseAtLeast(t *testing.T) {
	tr := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash"},
		{"type": "agent.tool_use", "name": "bash"},
		{"type": "agent.tool_use", "name": "read"},
	})
	if err := ToolUseAtLeast("bash", 2, Platform).Check(t, tr); err != nil {
		t.Errorf("bash>=2 should pass with two bash calls: %v", err)
	}
	if err := ToolUseAtLeast("bash", 3, Platform).Check(t, tr); err == nil {
		t.Error("bash>=3 should fail with two bash calls")
	}
	if err := ToolUseAtLeast("", 3, Platform).Check(t, tr); err != nil {
		t.Errorf("any>=3 should pass with three total calls: %v", err)
	}
}

func TestNoToolUseAndContainerGraders(t *testing.T) {
	clean := trialWith([]map[string]any{
		{"type": "agent.message", "content": textBlocks("ECHO:n0")},
	})
	if err := NoToolUse(Model).Check(t, clean); err != nil {
		t.Errorf("NoToolUse should pass on a text-only transcript: %v", err)
	}
	dirty := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash"},
	})
	if err := NoToolUse(Model).Check(t, dirty); err == nil {
		t.Error("NoToolUse should fail when a tool ran")
	}
}

func TestToolResultGraders(t *testing.T) {
	tr := trialWith([]map[string]any{
		{"type": "agent.tool_result", "is_error": true,
			"content": textBlocks("cat: missing: No such file")},
		{"type": "agent.tool_result", "is_error": false,
			"content": textBlocks("value is n0")},
	})
	if err := ToolResultOK(Platform).Check(t, tr); err != nil {
		t.Errorf("ToolResultOK should pass with one successful result: %v", err)
	}
	if err := ToolResultContains("n0", Platform).Check(t, tr); err != nil {
		t.Errorf("ToolResultContains should find the nonce in the ok result: %v", err)
	}

	// The nonce appears only in an error result — a contains-check that ignored
	// is_error would wrongly pass, so this pins that it does not.
	errOnly := trialWith([]map[string]any{
		{"type": "agent.tool_result", "is_error": true,
			"content": textBlocks("boom n0")},
	})
	if err := ToolResultContains("n0", Platform).Check(t, errOnly); err == nil {
		t.Error("ToolResultContains should ignore error results")
	}
	if err := ToolResultOK(Platform).Check(t, errOnly); err == nil {
		t.Error("ToolResultOK should fail when every result is an error")
	}
}

func TestCorePackToolResultsJoined(t *testing.T) {
	joined := corePackByName(t, "tool-results-joined")

	ok := trialWith([]map[string]any{
		{"type": "agent.tool_use", "id": "toolu_1", "name": "bash"},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1"},
	})
	if err := joined.Check(t, ok); err != nil {
		t.Errorf("one use, one result should pass: %v", err)
	}

	// A tool_use with no result: the wedged-session shape.
	unanswered := trialWith([]map[string]any{
		{"type": "agent.tool_use", "id": "toolu_1", "name": "bash"},
	})
	if err := joined.Check(t, unanswered); err == nil {
		t.Error("an unanswered tool_use should fail tool-results-joined")
	}

	// Two results for one use: the double-feed shape.
	doubled := trialWith([]map[string]any{
		{"type": "agent.tool_use", "id": "toolu_1", "name": "bash"},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1"},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1"},
	})
	if err := joined.Check(t, doubled); err == nil {
		t.Error("two results for one use should fail tool-results-joined")
	}

	// Both id and tool_use_id dropped by a wire regression: they must not join
	// on the empty string and pass vacuously.
	missingID := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash"},
		{"type": "agent.tool_result"},
	})
	if err := joined.Check(t, missingID); err == nil {
		t.Error("a tool_use with no id should fail rather than join on the empty string")
	}
}

func TestSeparateBashCalls(t *testing.T) {
	g := SeparateBashCalls("MARK=", "mark.txt")

	// Separate calls: no single command carries both markers.
	ok := trialWith([]map[string]any{
		bashUse("export MARK=n0"),
		bashUse(`echo "$MARK" > /workspace/mark.txt`),
		bashUse("cat /workspace/mark.txt"),
	})
	if err := g.Check(t, ok); err != nil {
		t.Errorf("separate calls should pass: %v", err)
	}

	// One call packing both: the false-green shape the grader closes.
	combined := trialWith([]map[string]any{
		bashUse(`export MARK=n0; echo "$MARK" > /workspace/mark.txt`),
	})
	if err := g.Check(t, combined); err == nil {
		t.Error("a single command doing both should fail separate-bash-calls")
	}
}

func TestBashCommandWith(t *testing.T) {
	g := BashCommandWith("$MARK", "mark.txt")

	// A bash call that reads the variable and writes the file: passes.
	ok := trialWith([]map[string]any{
		bashUse("export MARK=n0"),
		bashUse(`echo "$MARK" > /workspace/mark.txt`),
	})
	if err := g.Check(t, ok); err != nil {
		t.Errorf("a bash write reading $MARK should pass: %v", err)
	}

	// The file was written by the write tool, not bash — the persistence
	// sidestep the grader closes: no bash command contains both markers.
	viaWriteTool := trialWith([]map[string]any{
		bashUse("export MARK=n0"),
		{"type": "agent.tool_use", "name": "write",
			"input": map[string]any{"path": "/workspace/mark.txt", "content": "n0"}},
		bashUse("cat /workspace/mark.txt"),
	})
	if err := g.Check(t, viaWriteTool); err == nil {
		t.Error("writing the file with the write tool should fail bash-command-with")
	}
}

func bashUse(command string) map[string]any {
	return map[string]any{"type": "agent.tool_use", "name": "bash",
		"input": map[string]any{"command": command}}
}

func TestCorePackUsageAccounted(t *testing.T) {
	usage := corePackByName(t, "usage-accounted")

	ok := trialWith([]map[string]any{
		{"type": "span.model_request_end", "id": "sevt_1",
			"model_usage": map[string]any{"input_tokens": float64(5), "output_tokens": float64(2)}},
	})
	if err := usage.Check(t, ok); err != nil {
		t.Errorf("populated usage should pass: %v", err)
	}

	none := trialWith([]map[string]any{
		{"type": "agent.message", "content": textBlocks("hi")},
	})
	if err := usage.Check(t, none); err == nil {
		t.Error("no model_request_end should fail usage-accounted")
	}

	zero := trialWith([]map[string]any{
		{"type": "span.model_request_end", "id": "sevt_1",
			"model_usage": map[string]any{"input_tokens": float64(0), "output_tokens": float64(0)}},
	})
	if err := usage.Check(t, zero); err == nil {
		t.Error("zero token counts should fail usage-accounted")
	}

	// A fully cached turn: fresh input_tokens is 0 but cache_read carries the
	// real input. Summing the cached counters is what keeps this from being a
	// false platform failure.
	cached := trialWith([]map[string]any{
		{"type": "span.model_request_end", "id": "sevt_1",
			"model_usage": map[string]any{"input_tokens": float64(0),
				"cache_read_input_tokens": float64(100), "output_tokens": float64(8)}},
	})
	if err := usage.Check(t, cached); err != nil {
		t.Errorf("a fully cached turn should pass usage-accounted: %v", err)
	}
}

func TestCorePackEndsWithEndTurn(t *testing.T) {
	ends := corePackByName(t, "ends-with-end-turn")
	task := Task{Turns: []Turn{{Message: "x"}}}

	tr := &Trial{Task: task, Idles: []map[string]any{
		{"stop_reason": map[string]any{"type": "end_turn"}},
	}}
	if err := ends.Check(t, tr); err != nil {
		t.Errorf("end_turn should pass: %v", err)
	}

	tr.Idles = []map[string]any{{"stop_reason": map[string]any{"type": "max_tokens"}}}
	if err := ends.Check(t, tr); err == nil {
		t.Error("max_tokens should fail ends-with-end-turn")
	}

	// No idles at all must fail cleanly rather than panic on Idles[-1] — the
	// shape a future empty-Turns task, or any drive that recorded no idle, would
	// otherwise crash the whole run on.
	empty := &Trial{Task: task, Idles: nil}
	if err := ends.Check(t, empty); err == nil {
		t.Error("no idles should fail ends-with-end-turn rather than panic")
	}
}

// corePackByName pulls one named grader out of the core pack, failing the test
// if the name ever drifts — so a rename cannot silently orphan a unit test.
func corePackByName(t *testing.T, name string) Grader {
	t.Helper()
	for _, g := range corePack(Task{Turns: []Turn{{Message: "x"}}}) {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("core pack has no grader named %q", name)
	return Grader{}
}

func TestSubstReplacesEveryOccurrence(t *testing.T) {
	got := subst("a {{NONCE}} b {{NONCE}}", "xyz")
	if strings.Contains(got, "{{NONCE}}") || got != "a xyz b xyz" {
		t.Errorf("subst = %q, want all placeholders replaced", got)
	}
}
