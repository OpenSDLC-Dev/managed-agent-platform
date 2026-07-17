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

	// ContainerAbsent short-circuits to a pass the moment a tool ran, before it
	// asks Docker: a container for a tool the model actually called is the
	// executor doing its job, and NoToolUse above already flags the model
	// reaching for a tool it was told not to, so ContainerAbsent must not also
	// blame the platform for it. This exercises that branch; drop the
	// tool_use short-circuit and, on any host without Docker, it would fatal
	// instead of returning nil — which is the bite this pins.
	if err := ContainerAbsent(Platform).Check(t, dirty); err != nil {
		t.Errorf("ContainerAbsent should pass without touching Docker when a tool ran: %v", err)
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

func TestToolNotUsed(t *testing.T) {
	clean := trialWith([]map[string]any{{"type": "agent.tool_use", "name": "edit"}})
	if err := ToolNotUsed("write", Model).Check(t, clean); err != nil {
		t.Errorf("ToolNotUsed(write) should pass when only edit ran: %v", err)
	}
	dirty := trialWith([]map[string]any{{"type": "agent.tool_use", "name": "write"}})
	if err := ToolNotUsed("write", Model).Check(t, dirty); err == nil {
		t.Error("ToolNotUsed(write) should fail when write ran")
	}
}

func TestToolUseInputContains(t *testing.T) {
	g := ToolUseInputContains("bash", "missing_{{NONCE}}", Either)
	tr := trialWith([]map[string]any{bashUse("cat /workspace/missing_n0.txt")})
	if err := g.Check(t, tr); err != nil {
		t.Errorf("should find the nonce'd path in the bash input: %v", err)
	}
	// The token is absent from every bash input.
	other := trialWith([]map[string]any{bashUse("ls /workspace")})
	if err := g.Check(t, other); err == nil {
		t.Error("should fail when no bash input carries the token")
	}
	// The token is present but on a different tool.
	wrongTool := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "read", "input": map[string]any{"file_path": "/workspace/missing_n0.txt"}},
	})
	if err := g.Check(t, wrongTool); err == nil {
		t.Error("should fail when the token is only on a non-bash tool")
	}
}

func TestToolResultEquals(t *testing.T) {
	g := ToolResultEquals("SECRET_{{NONCE}}", Either)
	exact := trialWith([]map[string]any{
		{"type": "agent.tool_result", "is_error": false, "content": textBlocks("SECRET_n0")},
	})
	if err := g.Check(t, exact); err != nil {
		t.Errorf("a byte-exact result should pass: %v", err)
	}
	// A superset only contains the value — the byte-equal guard must reject it.
	superset := trialWith([]map[string]any{
		{"type": "agent.tool_result", "is_error": false, "content": textBlocks("SECRET_n0 and a trailing bit")},
	})
	if err := g.Check(t, superset); err == nil {
		t.Error("ToolResultEquals should reject a result that only contains the value")
	}
}

func TestToolResultMatches(t *testing.T) {
	g := ToolResultMatches(`(?m)^(/workspace/)?src/util/helpers\.go:3:`, Platform)
	ok := trialWith([]map[string]any{
		{"type": "agent.tool_result", "is_error": false,
			"content": textBlocks("/workspace/src/util/helpers.go:3:// NEEDLE_n0 marks the spot")},
	})
	if err := g.Check(t, ok); err != nil {
		t.Errorf("a grep path:line:text line should match: %v", err)
	}
	// The same text on an error result must not satisfy it.
	errRes := trialWith([]map[string]any{
		{"type": "agent.tool_result", "is_error": true,
			"content": textBlocks("/workspace/src/util/helpers.go:3:x")},
	})
	if err := g.Check(t, errRes); err == nil {
		t.Error("ToolResultMatches should ignore error results")
	}
}

func TestToolErrorResultContains(t *testing.T) {
	g := ToolErrorResultContains("exit code: 1", Platform)
	ok := trialWith([]map[string]any{
		{"type": "agent.tool_result", "is_error": true,
			"content": textBlocks("cat: missing: No such file\nexit code: 1")},
	})
	if err := g.Check(t, ok); err != nil {
		t.Errorf("the error trailer should match: %v", err)
	}
	// The same text on a NON-error result must not count — the trailer lives on
	// an is_error result, and a grader that ignored is_error would false-pass.
	nonErr := trialWith([]map[string]any{
		{"type": "agent.tool_result", "is_error": false, "content": textBlocks("exit code: 1")},
	})
	if err := g.Check(t, nonErr); err == nil {
		t.Error("ToolErrorResultContains should ignore non-error results")
	}
}

func TestEventCountAtLeast(t *testing.T) {
	tr := trialWith([]map[string]any{
		{"type": "user.message"}, {"type": "user.message"}, {"type": "agent.message"},
	})
	if err := EventCountAtLeast("user.message", 2, Platform).Check(t, tr); err != nil {
		t.Errorf("two user.message should meet a floor of 2: %v", err)
	}
	if err := EventCountAtLeast("user.message", 3, Platform).Check(t, tr); err == nil {
		t.Error("a floor of 3 should fail with two events")
	}
}

func TestRequiresActionRaised(t *testing.T) {
	g := RequiresActionRaised(Platform)
	paused := trialWith([]map[string]any{
		{"type": "session.status_idle", "stop_reason": map[string]any{
			"type": "requires_action", "event_ids": []any{"sevt_1"}}},
	})
	if err := g.Check(t, paused); err != nil {
		t.Errorf("a requires_action idle with event_ids should pass: %v", err)
	}
	ended := trialWith([]map[string]any{
		{"type": "session.status_idle", "stop_reason": map[string]any{"type": "end_turn"}},
	})
	if err := g.Check(t, ended); err == nil {
		t.Error("an end_turn-only transcript should fail requires-action-raised")
	}
	// requires_action with no event_ids is the malformed shape, not a pause.
	empty := trialWith([]map[string]any{
		{"type": "session.status_idle", "stop_reason": map[string]any{
			"type": "requires_action", "event_ids": []any{}}},
	})
	if err := g.Check(t, empty); err == nil {
		t.Error("requires_action with no event_ids should fail")
	}
}

func TestEvaluatedPermissionAsk(t *testing.T) {
	g := EvaluatedPermissionAsk("bash", Platform)
	ask := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "evaluated_permission": "ask"},
	})
	if err := g.Check(t, ask); err != nil {
		t.Errorf("a gated bash tool_use should pass: %v", err)
	}
	allow := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "evaluated_permission": "allow"},
	})
	if err := g.Check(t, allow); err == nil {
		t.Error("an ungated bash tool_use should fail evaluated-permission-ask")
	}
	// No bash at all must fail rather than pass vacuously.
	none := trialWith([]map[string]any{{"type": "agent.tool_use", "name": "read"}})
	if err := g.Check(t, none); err == nil {
		t.Error("no bash tool_use should fail rather than pass vacuously")
	}
}

func TestResultAfterConfirmation(t *testing.T) {
	g := ResultAfterConfirmation(Platform)
	ordered := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "id": "toolu_1"},
		{"type": "user.tool_confirmation", "result": "allow", "tool_use_id": "sevt_1"},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1"},
	})
	if err := g.Check(t, ordered); err != nil {
		t.Errorf("a result after the confirmation should pass: %v", err)
	}
	// A result only before the confirmation, none after — the gate did nothing.
	before := trialWith([]map[string]any{
		{"type": "agent.tool_result", "tool_use_id": "toolu_1"},
		{"type": "user.tool_confirmation", "result": "allow", "tool_use_id": "sevt_1"},
	})
	if err := g.Check(t, before); err == nil {
		t.Error("a result only before the confirmation should fail")
	}
	noConfirm := trialWith([]map[string]any{
		{"type": "agent.tool_result", "tool_use_id": "toolu_1"},
	})
	if err := g.Check(t, noConfirm); err == nil {
		t.Error("no confirmation on the log should fail")
	}
}

func TestReadRangeLine(t *testing.T) {
	g := ReadRangeLine("poem.txt", 57, "SECRET_{{NONCE}}", Either)
	read5757 := func(resultText string) *Trial {
		return trialWith([]map[string]any{
			{"type": "agent.tool_use", "name": "read", "id": "toolu_1", "input": map[string]any{
				"file_path": "/workspace/poem.txt", "view_range": []any{float64(57), float64(57)}}},
			{"type": "agent.tool_result", "tool_use_id": "toolu_1", "content": textBlocks(resultText)},
		})
	}
	if err := g.Check(t, read5757("SECRET_n0")); err != nil {
		t.Errorf("an exact [57,57] read returning the line should pass: %v", err)
	}
	// The slicer returned the neighbouring line — the off-by-one this guards.
	if err := g.Check(t, read5757("line-58")); err == nil {
		t.Error("a [57,57] read returning the wrong bytes should fail")
	}
	// No single-line [57,57] read requested at all.
	wholeFile := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "read", "id": "toolu_1", "input": map[string]any{
			"file_path": "/workspace/poem.txt", "view_range": []any{float64(1), float64(100)}}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1", "content": textBlocks("SECRET_n0")},
	})
	if err := g.Check(t, wholeFile); err == nil {
		t.Error("no [57,57] read should fail rather than pass on a whole-file read")
	}
}
