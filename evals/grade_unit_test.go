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

	// A result with no is_error flag is malformed, not an implicit success: both
	// success graders must skip it rather than count it, or a dropped-flag wire
	// regression would green a run.
	noFlag := trialWith([]map[string]any{
		{"type": "agent.tool_result", "content": textBlocks("value is n0")},
	})
	if err := ToolResultOK(Platform).Check(t, noFlag); err == nil {
		t.Error("ToolResultOK should not count a result missing is_error as a success")
	}
	if err := ToolResultContains("n0", Platform).Check(t, noFlag); err == nil {
		t.Error("ToolResultContains should skip a result missing is_error")
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

func TestToolCallResult(t *testing.T) {
	g := ToolCallResult("bash", "missing_{{NONCE}}", true, "exit code: 1", Either)

	// The nonce'd call's own result is an error carrying the trailer.
	ok := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "id": "toolu_1",
			"input": map[string]any{"command": "cat /workspace/missing_n0.txt"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1", "is_error": true,
			"content": textBlocks("cat: missing: No such file\nexit code: 1")},
	})
	if err := g.Check(t, ok); err != nil {
		t.Errorf("the nonce'd call's own error result should pass: %v", err)
	}

	// No bash call carries the nonce'd path — even though a stray result on the log
	// holds the trailer, the grader must not borrow it.
	noCall := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "id": "toolu_1",
			"input": map[string]any{"command": "ls /workspace"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1", "is_error": true,
			"content": textBlocks("exit code: 1")},
	})
	if err := g.Check(t, noCall); err == nil {
		t.Error("no matching call should fail rather than borrow another call's result")
	}

	// The matching call's OWN result succeeded, while an unrelated result errored.
	// Correlation is the point: is_error is read off the call's own result, so the
	// stray error must not green it.
	wrongResult := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "id": "toolu_1",
			"input": map[string]any{"command": "cat /workspace/missing_n0.txt"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1", "is_error": false,
			"content": textBlocks("(it somehow existed)")},
		{"type": "agent.tool_result", "tool_use_id": "toolu_other", "is_error": true,
			"content": textBlocks("exit code: 1")},
	})
	if err := g.Check(t, wrongResult); err == nil {
		t.Error("a success result on the matching call should fail even if another result errored")
	}

	// The matching call errored but with the wrong content.
	wrongContent := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "id": "toolu_1",
			"input": map[string]any{"command": "cat /workspace/missing_n0.txt"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1", "is_error": true,
			"content": textBlocks("permission denied")},
	})
	if err := g.Check(t, wrongContent); err == nil {
		t.Error("the matching error result lacking the trailer should fail")
	}

	// The matching call's result dropped is_error entirely. A wantErr=false
	// grader must reject the malformed result rather than read the absence as a
	// zero-value false — the vacuous-pass the strict flag check closes.
	ok2 := ToolCallResult("edit", "config.ini", false, "", Either)
	noFlag := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "edit", "id": "toolu_1",
			"input": map[string]any{"file_path": "/workspace/config.ini"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1",
			"content": textBlocks("edited /workspace/config.ini (1 replacement(s))")},
	})
	if err := ok2.Check(t, noFlag); err == nil {
		t.Error("a result with no is_error must fail a wantErr=false check, not pass vacuously")
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

	// No tool ran, so there was nothing to gate. The model half (the task's
	// ToolUseAtLeast) owns "the model never called the tool"; this passes rather
	// than blaming the platform for a pause that was never due.
	noTool := trialWith([]map[string]any{
		{"type": "session.status_idle", "stop_reason": map[string]any{"type": "end_turn"}},
	})
	if err := g.Check(t, noTool); err != nil {
		t.Errorf("no tool_use should pass vacuously: %v", err)
	}

	// A tool ran and the session paused with event_ids — the real bridge path.
	paused := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash"},
		{"type": "session.status_idle", "stop_reason": map[string]any{
			"type": "requires_action", "event_ids": []any{"sevt_1"}}},
	})
	if err := g.Check(t, paused); err != nil {
		t.Errorf("a requires_action idle with event_ids should pass: %v", err)
	}

	// A tool ran but the session never suspended — the gate failed to fire, which
	// is a genuine platform fault.
	ranWithoutPause := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash"},
		{"type": "session.status_idle", "stop_reason": map[string]any{"type": "end_turn"}},
	})
	if err := g.Check(t, ranWithoutPause); err == nil {
		t.Error("a gated tool that ran without a requires_action pause should fail")
	}

	// requires_action with no event_ids is the malformed shape, not a pause.
	empty := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash"},
		{"type": "session.status_idle", "stop_reason": map[string]any{
			"type": "requires_action", "event_ids": []any{}}},
	})
	if err := g.Check(t, empty); err == nil {
		t.Error("requires_action with no event_ids should fail")
	}

	// A non-empty event_ids array carrying a non-string (or empty string) id is
	// also malformed: the harness cannot confirm it, so the grader must red rather
	// than treat the pause as well-formed.
	badID := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash"},
		{"type": "session.status_idle", "stop_reason": map[string]any{
			"type": "requires_action", "event_ids": []any{float64(42)}}},
	})
	if err := g.Check(t, badID); err == nil {
		t.Error("requires_action with a non-string event id should fail")
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
	// No bash at all: the model never called it, which the task's ToolUseAtLeast
	// owns. This passes rather than double-blaming the platform for a "gated" tool
	// the model simply never invoked.
	none := trialWith([]map[string]any{{"type": "agent.tool_use", "name": "read"}})
	if err := g.Check(t, none); err != nil {
		t.Errorf("no bash tool_use should pass vacuously: %v", err)
	}
}

func TestConfirmedResult(t *testing.T) {
	// A denial: the synthesized result is an error carrying the deny message,
	// sequenced after the confirmation and correlated by tool_use_id.
	deny := ConfirmedResult(true, "DENY_{{NONCE}}", Platform)
	denied := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "id": "sevt_1"},
		{"type": "user.tool_confirmation", "result": "deny", "tool_use_id": "sevt_1"},
		{"type": "agent.tool_result", "tool_use_id": "sevt_1", "is_error": true,
			"content": textBlocks("not approved: DENY_n0")},
	})
	if err := deny.Check(t, denied); err != nil {
		t.Errorf("a denied call's synthesized error result should pass: %v", err)
	}

	// The result is for a DIFFERENT tool_use_id than the confirmation named — the
	// correlation must reject it rather than green on a stray result.
	crossed := trialWith([]map[string]any{
		{"type": "user.tool_confirmation", "result": "deny", "tool_use_id": "sevt_1"},
		{"type": "agent.tool_result", "tool_use_id": "sevt_other", "is_error": true,
			"content": textBlocks("not approved: DENY_n0")},
	})
	if err := deny.Check(t, crossed); err == nil {
		t.Error("a result for another tool_use_id should not satisfy the confirmed call")
	}

	// The result precedes the confirmation, with nothing after it.
	beforeOnly := trialWith([]map[string]any{
		{"type": "agent.tool_result", "tool_use_id": "sevt_1", "is_error": true,
			"content": textBlocks("not approved: DENY_n0")},
		{"type": "user.tool_confirmation", "result": "deny", "tool_use_id": "sevt_1"},
	})
	if err := deny.Check(t, beforeOnly); err == nil {
		t.Error("a result only before the confirmation should fail")
	}

	// No confirmation on the log: nothing gated, nothing to grade. The model half
	// owns "the model never reached the gate", so this passes vacuously.
	noConfirm := trialWith([]map[string]any{
		{"type": "agent.message", "content": textBlocks("hi")},
	})
	if err := deny.Check(t, noConfirm); err != nil {
		t.Errorf("no confirmation should pass vacuously: %v", err)
	}

	// An allow whose result succeeded — the wantErr=false, empty-content path.
	allow := ConfirmedResult(false, "", Platform)
	allowed := trialWith([]map[string]any{
		{"type": "user.tool_confirmation", "result": "allow", "tool_use_id": "sevt_1"},
		{"type": "agent.tool_result", "tool_use_id": "sevt_1", "is_error": false,
			"content": textBlocks("done")},
	})
	if err := allow.Check(t, allowed); err != nil {
		t.Errorf("an allowed call's successful result should pass: %v", err)
	}

	// The allowed result dropped is_error. The wantErr=false direction must reject
	// the malformed result, not read the absence as a zero-value false — the
	// vacuous Platform pass the strict flag check closes.
	allowNoFlag := trialWith([]map[string]any{
		{"type": "user.tool_confirmation", "result": "allow", "tool_use_id": "sevt_1"},
		{"type": "agent.tool_result", "tool_use_id": "sevt_1", "content": textBlocks("done")},
	})
	if err := allow.Check(t, allowNoFlag); err == nil {
		t.Error("a confirmed result with no is_error must fail wantErr=false, not pass vacuously")
	}
}

func TestReadRangeRequested(t *testing.T) {
	g := ReadRangeRequested("poem.txt", 57, Model)
	asked := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "read", "id": "toolu_1", "input": map[string]any{
			"file_path": "/workspace/poem.txt", "view_range": []any{float64(57), float64(57)}}},
	})
	if err := g.Check(t, asked); err != nil {
		t.Errorf("an exact [57,57] read of poem.txt should pass: %v", err)
	}
	// The right file but the wrong range.
	wrongRange := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "read", "id": "toolu_1", "input": map[string]any{
			"file_path": "/workspace/poem.txt", "view_range": []any{float64(1), float64(100)}}},
	})
	if err := g.Check(t, wrongRange); err == nil {
		t.Error("a whole-file read should fail read-range-requested")
	}
	// The right range on a sibling whose name only ends similarly — the
	// component-boundary guard must reject it.
	sibling := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "read", "id": "toolu_1", "input": map[string]any{
			"file_path": "/workspace/my-poem.txt", "view_range": []any{float64(57), float64(57)}}},
	})
	if err := g.Check(t, sibling); err == nil {
		t.Error("a sibling file ending in poem.txt should not satisfy the grader")
	}
	// The right basename and range but the wrong root: /tmp/poem.txt is a different
	// file the model read by mistake, not the seeded /workspace one.
	wrongRoot := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "read", "id": "toolu_1", "input": map[string]any{
			"file_path": "/tmp/poem.txt", "view_range": []any{float64(57), float64(57)}}},
	})
	if err := g.Check(t, wrongRoot); err == nil {
		t.Error("a read of /tmp/poem.txt should not satisfy a grader for the workspace poem.txt")
	}
	// The workspace-relative form is accepted (the model may pass a bare path).
	relative := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "read", "id": "toolu_1", "input": map[string]any{
			"file_path": "poem.txt", "view_range": []any{float64(57), float64(57)}}},
	})
	if err := g.Check(t, relative); err != nil {
		t.Errorf("a workspace-relative poem.txt read should pass: %v", err)
	}
}

func TestReadRangeBytes(t *testing.T) {
	g := ReadRangeBytes("poem.txt", 57, "MARKER_{{NONCE}}", Platform)
	read := func(resultText string, isErr bool) *Trial {
		return trialWith([]map[string]any{
			{"type": "agent.tool_use", "name": "read", "id": "toolu_1", "input": map[string]any{
				"file_path": "/workspace/poem.txt", "view_range": []any{float64(57), float64(57)}}},
			{"type": "agent.tool_result", "tool_use_id": "toolu_1", "is_error": isErr,
				"content": textBlocks(resultText)},
		})
	}
	if err := g.Check(t, read("MARKER_n0", false)); err != nil {
		t.Errorf("the exact line bytes should pass: %v", err)
	}
	// The slicer returned the neighbouring line — the off-by-one this guards.
	if err := g.Check(t, read("line-58", false)); err == nil {
		t.Error("wrong bytes should fail read-range-bytes")
	}
	// An is_error result for the matching read is not a valid slice.
	if err := g.Check(t, read("MARKER_n0", true)); err == nil {
		t.Error("an is_error read result should fail read-range-bytes")
	}
	// No matching [57,57] read at all: this half is vacuous, since
	// ReadRangeRequested owns the miss. It passes rather than blaming the slicer
	// for a line the model never read.
	noRead := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "read", "id": "toolu_1", "input": map[string]any{
			"file_path": "/workspace/poem.txt", "view_range": []any{float64(1), float64(100)}}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1", "is_error": false,
			"content": textBlocks("MARKER_n0")},
	})
	if err := g.Check(t, noRead); err != nil {
		t.Errorf("no [57,57] read should pass vacuously: %v", err)
	}
}

func TestEventAfterUserMessage(t *testing.T) {
	g := EventAfterUserMessage("agent.tool_use", 2, Either)

	// A tool_use follows the second user.message — the second turn did work.
	after := trialWith([]map[string]any{
		{"type": "user.message"},
		{"type": "agent.tool_use", "name": "write"},
		{"type": "user.message"},
		{"type": "agent.tool_use", "name": "bash"},
	})
	if err := g.Check(t, after); err != nil {
		t.Errorf("a tool_use after the 2nd user.message should pass: %v", err)
	}

	// Both tool_uses precede the second user.message — turn two did nothing. A
	// whole-transcript count would be fooled by turn one's work; this is not.
	onlyTurnOne := trialWith([]map[string]any{
		{"type": "user.message"},
		{"type": "agent.tool_use", "name": "write"},
		{"type": "agent.tool_use", "name": "bash"},
		{"type": "user.message"},
	})
	if err := g.Check(t, onlyTurnOne); err == nil {
		t.Error("no tool_use after the 2nd user.message should fail")
	}

	// Fewer than two user.message events on the log at all.
	oneTurn := trialWith([]map[string]any{
		{"type": "user.message"},
		{"type": "agent.tool_use", "name": "bash"},
	})
	if err := g.Check(t, oneTurn); err == nil {
		t.Error("fewer than 2 user.message events should fail")
	}
}

func TestOkResult(t *testing.T) {
	// A result with no is_error field is malformed, not implicitly ok: a wire
	// regression that dropped is_error must not read as success.
	if okResult(map[string]any{"content": textBlocks("hi")}) {
		t.Error("a result missing is_error should not be ok")
	}
	if okResult(map[string]any{"is_error": true, "content": textBlocks("boom")}) {
		t.Error("an is_error result should not be ok")
	}
	if !okResult(map[string]any{"is_error": false, "content": textBlocks("hi")}) {
		t.Error("an explicit is_error:false result should be ok")
	}
}
