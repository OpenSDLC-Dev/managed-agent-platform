package evals

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
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
// Both tokens are set: an unset Recall would leave {{RECALL}} unsubstituted and
// a grader looking for the literal placeholder reds, which is the loud failure
// fill is written to produce (TestFillLeavesAnUnsetRecallStanding pins it) but
// not what any other test here means to exercise.
func trialWith(events []map[string]any) *Trial {
	return &Trial{Nonce: "n0", Recall: "r0", Events: events}
}

func textBlocks(text string) []any {
	return []any{map[string]any{"type": "text", "text": text}}
}

// WritesFile's bash arm needs the command to write, not merely name the
// path: a `cat` of the file is the recall trial's read, and reading it as a
// write would hand MemorySynced a miss to blame on the platform.
func TestWritesFileBashNeedsAWriteConstruct(t *testing.T) {
	const path = "/mnt/memory/notes/codename.md"
	bash := func(cmd string) map[string]any {
		return map[string]any{"type": "agent.tool_use", "name": "bash", "input": map[string]any{"command": cmd}}
	}
	for cmd, want := range map[string]bool{
		"cat " + path:                         false,
		"ls -l " + path + " && wc -c " + path: false,
		"echo n0 > " + path:                   true,
		"printf 'x\\n' >> " + path:            true,
		"echo n0 | tee " + path:               true,
		"cp /tmp/draft.md " + path:            true,
		"sed -i 's/a/b/' " + path:             true,
		"touch " + path:                       true,
	} {
		if got := wroteFile(trialWith([]map[string]any{bash(cmd)}), path); got != want {
			t.Errorf("%q counted as a write = %v, want %v", cmd, got, want)
		}
	}
	write := map[string]any{"type": "agent.tool_use", "name": "write", "input": map[string]any{"file_path": path, "content": "x"}}
	if !wroteFile(trialWith([]map[string]any{write}), path) {
		t.Error("a write tool call was not counted")
	}
	if wroteFile(trialWith([]map[string]any{write}), "/mnt/memory/notes/other.md") {
		t.Error("a write of another file was counted")
	}
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

func TestMCPToolUse(t *testing.T) {
	g := MCPToolUse("vault", "read_passphrase", Either)

	called := trialWith([]map[string]any{
		{"type": "agent.mcp_tool_use", "mcp_server_name": "vault", "name": "read_passphrase"},
	})
	if err := g.Check(t, called); err != nil {
		t.Errorf("the call the grader names should pass: %v", err)
	}

	// The prefixed mcp__{server}__{tool} lives only inside a provider request.
	// A grader reading it off the log would be asserting a naming scheme the log
	// does not carry, so the bare name is what has to match.
	prefixed := trialWith([]map[string]any{
		{"type": "agent.mcp_tool_use", "mcp_server_name": "vault",
			"name": "mcp__vault__read_passphrase"},
	})
	if err := g.Check(t, prefixed); err == nil {
		t.Error("the prefixed name is not what the event carries; matching it should fail")
	}

	// Same tool name, different server — two servers may each offer a tool by
	// the same name, so the pair is what identifies a call.
	otherServer := trialWith([]map[string]any{
		{"type": "agent.mcp_tool_use", "mcp_server_name": "other", "name": "read_passphrase"},
	})
	if err := g.Check(t, otherServer); err == nil {
		t.Error("another server's tool of the same name should not satisfy this grader")
	}

	// An MCP call recorded as a plain agent.tool_use is the regression this
	// grader exists to see: the platform routing an MCP call onto the built-in
	// tool's event would make the wire wrong while the trial stayed green.
	asPlainToolUse := trialWith([]map[string]any{
		{"type": "agent.tool_use", "mcp_server_name": "vault", "name": "read_passphrase"},
	})
	if err := g.Check(t, asPlainToolUse); err == nil {
		t.Error("agent.tool_use is the wrong event family; it should not satisfy this grader")
	}
}

func TestMCPEvaluatedPermissionAsk(t *testing.T) {
	g := MCPEvaluatedPermissionAsk("vault", "read_passphrase", Platform)

	ask := trialWith([]map[string]any{
		{"type": "agent.mcp_tool_use", "mcp_server_name": "vault",
			"name": "read_passphrase", "evaluated_permission": "ask"},
	})
	if err := g.Check(t, ask); err != nil {
		t.Errorf("a gated mcp call should pass: %v", err)
	}

	allow := trialWith([]map[string]any{
		{"type": "agent.mcp_tool_use", "mcp_server_name": "vault",
			"name": "read_passphrase", "evaluated_permission": "allow"},
	})
	if err := g.Check(t, allow); err == nil {
		t.Error("an ungated mcp call should fail evaluated-permission-ask")
	}

	// No matching call: the model never reached for the tool, which MCPToolUse
	// owns as Either. Reding here would file a model refusal as a platform
	// defect, so it passes — the same division its built-in twin makes.
	none := trialWith([]map[string]any{
		{"type": "agent.mcp_tool_use", "mcp_server_name": "vault", "name": "other_tool",
			"evaluated_permission": "allow"},
	})
	if err := g.Check(t, none); err != nil {
		t.Errorf("another tool's call should not be graded here: %v", err)
	}

	// Two calls, the second unstamped — a gate that held only for the opening
	// call. A first-match-wins check passes this, which is why every call is
	// checked.
	secondUngated := trialWith([]map[string]any{
		{"type": "agent.mcp_tool_use", "id": "sevt_1", "mcp_server_name": "vault",
			"name": "read_passphrase", "evaluated_permission": "ask"},
		{"type": "agent.mcp_tool_use", "id": "sevt_2", "mcp_server_name": "vault",
			"name": "read_passphrase", "evaluated_permission": "allow"},
	})
	if err := g.Check(t, secondUngated); err == nil {
		t.Error("a second ungated call should fail even when the first was gated")
	}
}

// TestRequiresActionRaisedCountsMCPCalls pins the half of the trigger an MCP-only
// trial depends on. Counting agent.tool_use alone made this grader pass such a
// trial by having nothing to look at — a gate regressed to allow-unattended left
// it green.
func TestRequiresActionRaisedCountsMCPCalls(t *testing.T) {
	g := RequiresActionRaised(Platform)

	ranWithoutPause := trialWith([]map[string]any{
		{"type": "agent.mcp_tool_use", "mcp_server_name": "vault", "name": "read_passphrase"},
		{"type": "session.status_idle", "stop_reason": map[string]any{"type": "end_turn"}},
	})
	if err := g.Check(t, ranWithoutPause); err == nil {
		t.Error("a gated mcp call that ran without a requires_action pause should fail")
	}

	paused := trialWith([]map[string]any{
		{"type": "agent.mcp_tool_use", "mcp_server_name": "vault", "name": "read_passphrase"},
		{"type": "session.status_idle", "stop_reason": map[string]any{
			"type": "requires_action", "event_ids": []any{"sevt_1"}}},
	})
	if err := g.Check(t, paused); err != nil {
		t.Errorf("a requires_action idle with event_ids should pass: %v", err)
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

	errOnly := trialWith([]map[string]any{
		{"type": "agent.tool_result", "is_error": true,
			"content": textBlocks("boom n0")},
	})
	if err := ToolResultOK(Platform).Check(t, errOnly); err == nil {
		t.Error("ToolResultOK should fail when every result is an error")
	}

	// A result with no is_error flag is malformed, not an implicit success: the
	// grader must skip it rather than count it, or a dropped-flag wire
	// regression would green a run.
	noFlag := trialWith([]map[string]any{
		{"type": "agent.tool_result", "content": textBlocks("value is n0")},
	})
	if err := ToolResultOK(Platform).Check(t, noFlag); err == nil {
		t.Error("ToolResultOK should not count a result missing is_error as a success")
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

	// The MCP family, which travels in its own pair and correlates on its own
	// field. Left out, the executor's second answering path would carry this
	// invariant nowhere — and the mcp-answer trial is where it has no other
	// coverage.
	mcpOK := trialWith([]map[string]any{
		{"type": "agent.mcp_tool_use", "id": "sevt_1", "name": "read_passphrase"},
		{"type": "agent.mcp_tool_result", "mcp_tool_use_id": "sevt_1"},
	})
	if err := joined.Check(t, mcpOK); err != nil {
		t.Errorf("one mcp call, one mcp result should pass: %v", err)
	}

	mcpDoubled := trialWith([]map[string]any{
		{"type": "agent.mcp_tool_use", "id": "sevt_1", "name": "read_passphrase"},
		{"type": "agent.mcp_tool_result", "mcp_tool_use_id": "sevt_1"},
		{"type": "agent.mcp_tool_result", "mcp_tool_use_id": "sevt_1"},
	})
	if err := joined.Check(t, mcpDoubled); err == nil {
		t.Error("two mcp results for one mcp call should fail tool-results-joined")
	}

	mcpUnanswered := trialWith([]map[string]any{
		{"type": "agent.mcp_tool_use", "id": "sevt_1", "name": "read_passphrase"},
	})
	if err := joined.Check(t, mcpUnanswered); err == nil {
		t.Error("an unanswered mcp call should fail tool-results-joined")
	}

	// The families must not cross-join: an MCP result carrying a built-in call's
	// id answers nothing, and correlating on the wrong field would hide it. The
	// built-in call gets its own proper result, so the built-in half passes and
	// the failure can only come from the orphan MCP result — without it the
	// built-in half reds first and this proves nothing about the MCP arm.
	crossed := trialWith([]map[string]any{
		{"type": "agent.tool_use", "id": "toolu_1", "name": "bash"},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1"},
		{"type": "agent.mcp_tool_result", "mcp_tool_use_id": "toolu_1"},
	})
	if err := joined.Check(t, crossed); err == nil {
		t.Error("an mcp result must not answer a built-in tool_use")
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

func TestFillReplacesEveryOccurrence(t *testing.T) {
	tr := &Trial{Nonce: "xyz", Recall: "r0"}
	got := tr.fill("a {{NONCE}} b {{NONCE}} c {{RECALL}}")
	if strings.Contains(got, "{{") || got != "a xyz b xyz c r0" {
		t.Errorf("fill = %q, want all placeholders replaced", got)
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

	// Two calls, the second unstamped — a gate that held only for the opening
	// call. A first-call-only check passes this, which is why every call is
	// checked.
	secondUngated := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "id": "sevt_1", "evaluated_permission": "ask"},
		{"type": "agent.tool_use", "name": "bash", "id": "sevt_2", "evaluated_permission": "allow"},
	})
	if err := g.Check(t, secondUngated); err == nil {
		t.Error("a second bash call that was not gated should fail even when the first was")
	}
}

func TestConfirmedResult(t *testing.T) {
	deny := ConfirmedResult("bash", []string{"APPEND_{{NONCE}}"}, true, "DENY_{{NONCE}}", Platform)
	gatedCall := func(id, command string) map[string]any {
		return map[string]any{"type": "agent.tool_use", "name": "bash", "id": id,
			"input": map[string]any{"command": command}}
	}
	const appended = "echo APPEND_n0 >> /workspace/notes.txt"

	// A denial: the synthesized result is an error carrying the deny message,
	// sequenced after the confirmation and correlated by tool_use_id.
	denied := trialWith([]map[string]any{
		gatedCall("sevt_1", appended),
		{"type": "user.tool_confirmation", "result": "deny", "tool_use_id": "sevt_1"},
		{"type": "agent.tool_result", "tool_use_id": "sevt_1", "is_error": true,
			"content": textBlocks("not approved: DENY_n0")},
	})
	if err := deny.Check(t, denied); err != nil {
		t.Errorf("a denied call's synthesized error result should pass: %v", err)
	}

	// The confirmation names an id that is on no agent.tool_use — the shape a gate
	// that listed the wrong event in requires_action produces. Correlating only
	// forward from the confirmation cannot see it, since the platform would answer
	// the id it was handed; this is the join that catches it.
	danglingID := trialWith([]map[string]any{
		gatedCall("sevt_1", appended),
		{"type": "user.tool_confirmation", "result": "deny", "tool_use_id": "sevt_span"},
		{"type": "agent.tool_result", "tool_use_id": "sevt_span", "is_error": true,
			"content": textBlocks("not approved: DENY_n0")},
	})
	if err := deny.Check(t, danglingID); err == nil {
		t.Error("a confirmation naming no tool_use on the log should fail")
	}

	// The transcript that isolates the dangling-name join, and the only one that
	// can: the dangling confirmation is itself well-formed — it has a result — and
	// it sits beside a graded call that was confirmed and answered correctly. Every
	// other branch is therefore satisfied, so deleting the join is the only way
	// this can pass. (An earlier version of this case gave the dangling
	// confirmation no result, and a mutant with the join removed still red it
	// through a different branch — the test asserted the right outcome for the
	// wrong reason, which two reviewers caught by mutation and this fixes.)
	danglingBeside := trialWith([]map[string]any{
		gatedCall("sevt_1", appended),
		{"type": "user.tool_confirmation", "result": "deny", "tool_use_id": "sevt_1"},
		{"type": "agent.tool_result", "tool_use_id": "sevt_1", "is_error": true,
			"content": textBlocks("not approved: DENY_n0")},
		{"type": "user.tool_confirmation", "result": "deny", "tool_use_id": "sevt_span"},
		{"type": "agent.tool_result", "tool_use_id": "sevt_span", "is_error": true,
			"content": textBlocks("not approved: DENY_n0")},
	})
	if err := deny.Check(t, danglingBeside); err == nil {
		t.Error("a confirmation naming no tool_use should fail even when the graded call resolved cleanly")
	}

	// The bridge stopped some other call and the graded one went through
	// unconfirmed. This grader stays quiet — blaming the platform here would
	// misread the harness giving up on a model that re-pauses past
	// maxConfirmRounds — and the sibling that owns the case reds instead: a gated
	// call that ran without being stopped carries no "ask" stamp. The pair is
	// asserted together, because the vacuity above is only safe if the sibling
	// really does fire.
	ungatedRun := []map[string]any{
		{"type": "agent.tool_use", "name": "bash", "id": "sevt_1", "evaluated_permission": "allow",
			"input": map[string]any{"command": appended}},
		gatedCall("sevt_2", "ls /workspace"),
		{"type": "user.tool_confirmation", "result": "deny", "tool_use_id": "sevt_2"},
		{"type": "agent.tool_result", "tool_use_id": "sevt_2", "is_error": true,
			"content": textBlocks("not approved: DENY_n0")},
	}
	if err := deny.Check(t, trialWith(ungatedRun)); err != nil {
		t.Errorf("a graded call nobody confirmed should pass vacuously: %v", err)
	}
	if err := EvaluatedPermissionAsk("bash", Platform).Check(t, trialWith(ungatedRun)); err == nil {
		t.Error("the sibling grader must red on the ungated call ConfirmedResult declines to blame")
	}

	// The result is for a DIFFERENT tool_use_id than the confirmation named — the
	// correlation must reject it rather than green on a stray result.
	crossed := trialWith([]map[string]any{
		gatedCall("sevt_1", appended),
		{"type": "user.tool_confirmation", "result": "deny", "tool_use_id": "sevt_1"},
		{"type": "agent.tool_result", "tool_use_id": "sevt_other", "is_error": true,
			"content": textBlocks("not approved: DENY_n0")},
	})
	if err := deny.Check(t, crossed); err == nil {
		t.Error("a result for another tool_use_id should not satisfy the confirmed call")
	}

	// The result precedes the confirmation, with nothing after it.
	beforeOnly := trialWith([]map[string]any{
		gatedCall("sevt_1", appended),
		{"type": "agent.tool_result", "tool_use_id": "sevt_1", "is_error": true,
			"content": textBlocks("not approved: DENY_n0")},
		{"type": "user.tool_confirmation", "result": "deny", "tool_use_id": "sevt_1"},
	})
	if err := deny.Check(t, beforeOnly); err == nil {
		t.Error("a result only before the confirmation should fail")
	}

	// A confirmation with no tool_use_id is malformed, not an absent gate.
	noID := trialWith([]map[string]any{
		gatedCall("sevt_1", appended),
		{"type": "user.tool_confirmation", "result": "deny"},
	})
	if err := deny.Check(t, noID); err == nil {
		t.Error("a confirmation with no tool_use_id should fail")
	}

	// A verifying retry: the model repeats a command carrying the same marker and
	// that one errors. One confirmed call satisfying the claim is enough, so this
	// passes — requiring every matching call to satisfy it made a correct platform
	// red whenever the model checked its own work.
	verifiedAfter := trialWith([]map[string]any{
		gatedCall("sevt_1", appended),
		{"type": "user.tool_confirmation", "result": "deny", "tool_use_id": "sevt_1"},
		{"type": "agent.tool_result", "tool_use_id": "sevt_1", "is_error": true,
			"content": textBlocks("not approved: DENY_n0")},
		gatedCall("sevt_2", "grep APPEND_n0 /workspace/notes.txt"),
		{"type": "user.tool_confirmation", "result": "deny", "tool_use_id": "sevt_2"},
		{"type": "agent.tool_result", "tool_use_id": "sevt_2", "is_error": true,
			"content": textBlocks("no such thing")},
	})
	if err := deny.Check(t, verifiedAfter); err != nil {
		t.Errorf("one confirmed call satisfying the claim should be enough: %v", err)
	}

	// The denial produced a success result: the deny did not block.
	notBlocked := trialWith([]map[string]any{
		gatedCall("sevt_1", appended),
		{"type": "user.tool_confirmation", "result": "deny", "tool_use_id": "sevt_1"},
		{"type": "agent.tool_result", "tool_use_id": "sevt_1", "is_error": false,
			"content": textBlocks("not approved: DENY_n0")},
	})
	if err := deny.Check(t, notBlocked); err == nil {
		t.Error("a denied call whose result succeeded should fail")
	}

	// The right flag, but the deny message never made it back.
	wrongMessage := trialWith([]map[string]any{
		gatedCall("sevt_1", appended),
		{"type": "user.tool_confirmation", "result": "deny", "tool_use_id": "sevt_1"},
		{"type": "agent.tool_result", "tool_use_id": "sevt_1", "is_error": true,
			"content": textBlocks("permission denied")},
	})
	if err := deny.Check(t, wrongMessage); err == nil {
		t.Error("a denial that lost its deny message should fail")
	}

	// No confirmation on the log: nothing gated, nothing to grade. The model half
	// owns "the model never reached the gate", so this passes vacuously.
	noConfirm := trialWith([]map[string]any{
		{"type": "agent.message", "content": textBlocks("hi")},
	})
	if err := deny.Check(t, noConfirm); err != nil {
		t.Errorf("no confirmation should pass vacuously: %v", err)
	}

	// The model gated something else entirely and never made the call the task
	// described. Every confirmation still resolves, so the bridge is fine; the
	// Model-class ToolCalledWith owns the miss and this must not blame the
	// platform for it.
	otherCallOnly := trialWith([]map[string]any{
		gatedCall("sevt_2", "ls /workspace"),
		{"type": "user.tool_confirmation", "result": "deny", "tool_use_id": "sevt_2"},
		{"type": "agent.tool_result", "tool_use_id": "sevt_2", "is_error": true,
			"content": textBlocks("not approved: DENY_n0")},
	})
	if err := deny.Check(t, otherCallOnly); err != nil {
		t.Errorf("a trial whose model never made the graded call should pass vacuously: %v", err)
	}

	// An allow whose result succeeded — the wantErr=false, empty-content path.
	allow := ConfirmedResult("bash", []string{"GATED_{{NONCE}}"}, false, "", Platform)
	const gatedWrite = "echo GATED_n0 > /workspace/gated.txt"
	allowed := trialWith([]map[string]any{
		gatedCall("sevt_1", gatedWrite),
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
		gatedCall("sevt_1", gatedWrite),
		{"type": "user.tool_confirmation", "result": "allow", "tool_use_id": "sevt_1"},
		{"type": "agent.tool_result", "tool_use_id": "sevt_1", "content": textBlocks("done")},
	})
	if err := allow.Check(t, allowNoFlag); err == nil {
		t.Error("a confirmed result with no is_error must fail wantErr=false, not pass vacuously")
	}
}

func TestToolCalledWith(t *testing.T) {
	g := ToolCalledWith("bash", []string{"cat /workspace/mark.txt"}, Model)

	ok := trialWith([]map[string]any{
		bashUse("export MARK=n0"),
		bashUse("cat /workspace/mark.txt"),
	})
	if err := g.Check(t, ok); err != nil {
		t.Errorf("the instructed command should satisfy tool-called-with: %v", err)
	}

	// A heredoc write mentioning both `cat` and the path is not the read the task
	// asked for — the reason the marker is the whole command and not two words.
	heredoc := trialWith([]map[string]any{
		bashUse("cat > /workspace/mark.txt <<'EOF'\nn0\nEOF"),
	})
	if err := g.Check(t, heredoc); err == nil {
		t.Error("a heredoc write should not count as the instructed cat")
	}

	// The marker carries characters json.Marshal escapes (`>`), so a matcher
	// working on the encoded input would never find them. This pins that the
	// match is against what the model wrote.
	redirect := ToolCalledWith("bash", []string{"echo GATED_{{NONCE}} > /workspace/gated.txt"}, Model)
	wrote := trialWith([]map[string]any{bashUse("echo GATED_n0 > /workspace/gated.txt")})
	if err := redirect.Check(t, wrote); err != nil {
		t.Errorf("a marker containing > should match the decoded command: %v", err)
	}

	// A marker must be carried by one call, not assembled from two.
	split := trialWith([]map[string]any{
		bashUse("cat /workspace/other.txt"),
		bashUse("ls /workspace/mark.txt"),
	})
	if err := g.Check(t, split); err == nil {
		t.Error("markers spread across two calls should not satisfy tool-called-with")
	}
}

func TestOnlyIf(t *testing.T) {
	// shell-state's shape: the Platform file check speaks only once the model has
	// run the export the file's content depends on.
	exported := calledWith("bash", "export MARK", "{{NONCE}}")
	inner := Grader{Name: "always-fails", Class: Platform,
		Check: func(_ *testing.T, _ *Trial) error { return errAlways }}
	g := OnlyIf(inner, exported)

	if g.Name != inner.Name || g.Class != inner.Class {
		t.Errorf("OnlyIf = %s/%s, want the wrapped grader's own name and class", g.Name, g.Class)
	}

	// The premise never held: the platform is not asked to answer for it.
	skipped := trialWith([]map[string]any{bashUse("echo hi > /workspace/mark.txt")})
	if err := g.Check(t, skipped); err != nil {
		t.Errorf("a grader whose premise never held should pass: %v", err)
	}

	// The model did as asked, so the claim is live.
	did := trialWith([]map[string]any{bashUse("export MARK=n0")})
	if err := g.Check(t, did); err == nil {
		t.Error("a grader whose premise held should run and report the inner failure")
	}

	// Every premise must hold, not just one.
	both := OnlyIf(inner, exported, calledWith("bash", "$MARK", "mark.txt"))
	if err := both.Check(t, did); err != nil {
		t.Errorf("one premise of two should not be enough to make the claim live: %v", err)
	}

	// The premise matches the same markers ToolCalledWith does, so a trial that
	// silences the Platform grader always reds the Model one beside it — the
	// property that keeps a gate from opening a window where neither fires.
	if err := ToolCalledWith("bash", []string{"export MARK", "{{NONCE}}"}, Model).Check(t, skipped); err == nil {
		t.Error("the Model grader must red on exactly the trial that made the Platform grader vacuous")
	}
}

var errAlways = errors.New("the wrapped grader ran")

func TestCallResult(t *testing.T) {
	g := CallResult("bash", []string{"cat /workspace/mark.txt"}, false, "{{NONCE}}", Platform)

	ok := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "id": "toolu_1",
			"input": map[string]any{"command": "cat /workspace/mark.txt"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1", "is_error": false,
			"content": textBlocks("n0")},
	})
	if err := g.Check(t, ok); err != nil {
		t.Errorf("the matching call's own result should pass: %v", err)
	}

	// The round trip came back empty — the persistent-shell regression this pins.
	empty := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "id": "toolu_1",
			"input": map[string]any{"command": "cat /workspace/mark.txt"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1", "is_error": false,
			"content": textBlocks("")},
	})
	if err := g.Check(t, empty); err == nil {
		t.Error("an empty result for the matching call should fail")
	}

	// The nonce is on the log, but on some other call's result. Correlation is
	// what keeps that from greening the claim.
	elsewhere := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "id": "toolu_1",
			"input": map[string]any{"command": "cat /workspace/mark.txt"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1", "is_error": false,
			"content": textBlocks("")},
		{"type": "agent.tool_result", "tool_use_id": "toolu_2", "is_error": false,
			"content": textBlocks("n0")},
	})
	if err := g.Check(t, elsewhere); err == nil {
		t.Error("another call's result carrying the nonce should not satisfy the graded call")
	}

	// No such call: the Model half owns the miss, so a Platform-class grader must
	// pass here rather than blame the platform for a command never run.
	noCall := trialWith([]map[string]any{
		bashUse("export MARK=n0"),
	})
	if err := g.Check(t, noCall); err != nil {
		t.Errorf("no matching call should pass vacuously: %v", err)
	}

	// A second attempt that worked satisfies the claim: how many times a model
	// reaches for a tool is its own business.
	retried := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "id": "toolu_1",
			"input": map[string]any{"command": "cat /workspace/mark.txt"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1", "is_error": true,
			"content": textBlocks("boom")},
		{"type": "agent.tool_use", "name": "bash", "id": "toolu_2",
			"input": map[string]any{"command": "cat /workspace/mark.txt"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_2", "is_error": false,
			"content": textBlocks("n0")},
	})
	if err := g.Check(t, retried); err != nil {
		t.Errorf("one satisfying call among several should pass: %v", err)
	}

	// The content is right but the flag says the call errored: the wantErr check
	// must not be skipped just because the text matched.
	wrongFlag := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "id": "toolu_1",
			"input": map[string]any{"command": "cat /workspace/mark.txt"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1", "is_error": true,
			"content": textBlocks("n0")},
	})
	if err := g.Check(t, wrongFlag); err == nil {
		t.Error("a matching call whose result errored should fail even with the right content")
	}

	// A dropped is_error is terminal, not something a later good call forgives:
	// the second result says nothing about the first, and letting a retry erase a
	// missing wire field is the vacuous pass the flag check exists to close.
	flaglessThenGood := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "id": "toolu_1",
			"input": map[string]any{"command": "cat /workspace/mark.txt"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1", "content": textBlocks("n0")},
		{"type": "agent.tool_use", "name": "bash", "id": "toolu_2",
			"input": map[string]any{"command": "cat /workspace/mark.txt"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_2", "is_error": false,
			"content": textBlocks("n0")},
	})
	if err := g.Check(t, flaglessThenGood); err == nil {
		t.Error("a result missing is_error must fail even when a later matching call is well-formed")
	}

	// A call that never came back is not gradeable, but it must not excuse its
	// siblings: the resultless call is skipped, and the one that did come back
	// with the wrong content still reds.
	danglingThenBad := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "id": "toolu_1",
			"input": map[string]any{"command": "cat /workspace/mark.txt"}},
		{"type": "agent.tool_use", "name": "bash", "id": "toolu_2",
			"input": map[string]any{"command": "cat /workspace/mark.txt"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_2", "is_error": false,
			"content": textBlocks("")},
	})
	if err := g.Check(t, danglingThenBad); err == nil {
		t.Error("a call with no result must not excuse a sibling whose result is wrong")
	}

	// When no matching call came back at all there is nothing to judge, and
	// blaming the platform for a verdict it was never given is exactly the
	// vacuous-red this branch avoids.
	allDangling := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "bash", "id": "toolu_1",
			"input": map[string]any{"command": "cat /workspace/mark.txt"}},
	})
	if err := g.Check(t, allDangling); err != nil {
		t.Errorf("a matching call with no result at all should pass vacuously: %v", err)
	}

	// An empty marker list grades any call to the tool — needle-search's glob.
	anyGlob := CallResult("glob", nil, false, "/workspace/src/util/helpers.go", Either)
	globbed := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "glob", "id": "toolu_1",
			"input": map[string]any{"pattern": "**/*.go"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1", "is_error": false,
			"content": textBlocks("/workspace/src/util/helpers.go\n/workspace/src/main.go")},
	})
	if err := anyGlob.Check(t, globbed); err != nil {
		t.Errorf("a glob result naming the seeded file should pass: %v", err)
	}
	missed := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "glob", "id": "toolu_1",
			"input": map[string]any{"pattern": "*.go"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1", "is_error": false,
			"content": textBlocks("no matches")},
	})
	if err := anyGlob.Check(t, missed); err == nil {
		t.Error("a glob result that never names the seeded file should fail")
	}
}

func TestGlobPathList(t *testing.T) {
	g := GlobPathList(Platform)
	globResult := func(text string, isErr bool) *Trial {
		return trialWith([]map[string]any{
			{"type": "agent.tool_use", "name": "glob", "id": "toolu_1",
				"input": map[string]any{"pattern": "**/*.go"}},
			{"type": "agent.tool_result", "tool_use_id": "toolu_1", "is_error": isErr,
				"content": textBlocks(text)},
		})
	}
	if err := g.Check(t, globResult("/workspace/src/main.go\n/workspace/src/decoy.go", false)); err != nil {
		t.Errorf("absolute paths should pass: %v", err)
	}
	// The tool's own "nothing matched" answer is not a malformed path list.
	if err := g.Check(t, globResult("no matches", false)); err != nil {
		t.Errorf("no matches should pass: %v", err)
	}
	// The mtime stat prefix leaking into the records — a real shape of a broken
	// glob that still returns something.
	if err := g.Check(t, globResult("1712.9 /workspace/src/main.go", false)); err == nil {
		t.Error("an mtime prefix on the record should fail glob-path-list")
	}
	// Relative paths: the caller cannot join them to anything.
	if err := g.Check(t, globResult("src/main.go", false)); err == nil {
		t.Error("a relative path should fail glob-path-list")
	}
	// A failed glob has no path list to shape-check; the pattern is the model's.
	if err := g.Check(t, globResult("glob: bad pattern", true)); err != nil {
		t.Errorf("an errored glob should pass: %v", err)
	}
	// A success with no content at all: glob says "no matches" for an empty list,
	// so this is the shape of a dropped content block, and a grader that walked
	// zero lines would accept it silently.
	if err := g.Check(t, globResult("", false)); err == nil {
		t.Error("a glob success with no content should fail glob-path-list")
	}
	// Only the first record is checked, and that is the tool's contract talking:
	// search.go is NUL-delimited end to end because a filename may legally carry a
	// newline, so a later "line" can be the tail of a perfectly good path. A
	// per-line check would red the platform for correct output.
	if err := g.Check(t, globResult("/workspace/od\nd name.go\n/workspace/b.go", false)); err != nil {
		t.Errorf("a path containing a newline is legal glob output, not a shape regression: %v", err)
	}
	// A result with no is_error flag is malformed, not an implicit failure to skip.
	noFlag := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "glob", "id": "toolu_1",
			"input": map[string]any{"pattern": "**/*.go"}},
		{"type": "agent.tool_result", "tool_use_id": "toolu_1",
			"content": textBlocks("/workspace/src/main.go")},
	})
	if err := g.Check(t, noFlag); err == nil {
		t.Error("a glob result missing is_error should fail rather than be skipped")
	}
	// No glob at all: the task's tool-use floor owns that miss.
	if err := g.Check(t, trialWith([]map[string]any{bashUse("ls")})); err != nil {
		t.Errorf("no glob call should pass vacuously: %v", err)
	}
}

func TestNotInToolTraffic(t *testing.T) {
	g := NotInToolTraffic("{{RECALL}}", Either)

	clean := trialWith([]map[string]any{
		bashUse("echo entry-one-n0 > /workspace/journal.txt"),
		{"type": "agent.tool_result", "tool_use_id": "toolu_1", "is_error": false,
			"content": textBlocks("")},
		{"type": "agent.message", "content": textBlocks("DONE2:n0 r0")},
	})
	if err := g.Check(t, clean); err != nil {
		t.Errorf("a token that only ever appears in a message should pass: %v", err)
	}

	// The model stashed the word in a file: the replay witness has quietly become
	// a second persistence check.
	stashed := trialWith([]map[string]any{
		bashUse("echo r0 > /workspace/note.txt"),
	})
	if err := g.Check(t, stashed); err == nil {
		t.Error("a token written through a tool input should fail")
	}

	// And read back out.
	readBack := trialWith([]map[string]any{
		{"type": "agent.tool_result", "tool_use_id": "toolu_1", "is_error": false,
			"content": textBlocks("r0")},
	})
	if err := g.Check(t, readBack); err == nil {
		t.Error("a token coming back in a tool result should fail")
	}

	// Somewhere inputText does not look: an object key, and a non-string value.
	// This grader's job is to find the token wherever it is, so it reads the
	// encoded input too.
	inKey := trialWith([]map[string]any{
		{"type": "agent.tool_use", "name": "write", "id": "toolu_1",
			"input": map[string]any{"command": "echo safe", "r0": true}},
	})
	if err := g.Check(t, inKey); err == nil {
		t.Error("a token carried as an input key should fail")
	}

	// And the converse, which is why both spellings are read rather than only the
	// encoded one: json.Marshal HTML-escapes & < >, so a token carrying any of
	// them is on the log in plain sight and absent from the encoding. The nonces
	// this grader is used with today are hex, but a grader that could be defeated
	// by the choice of token is not a witness.
	escaped := NotInToolTraffic("tom&jerry", Either)
	inValue := trialWith([]map[string]any{
		bashUse("echo tom&jerry > /workspace/note.txt"),
	})
	if err := escaped.Check(t, inValue); err == nil {
		t.Error("a token whose JSON encoding is escaped should still be found in the decoded input")
	}
}

// An unset Recall must leave {{RECALL}} standing rather than substitute the
// empty string: strings.Contains(anything, "") is true, so the empty
// substitution would green every recall assertion while proving nothing.
func TestFillLeavesAnUnsetRecallStanding(t *testing.T) {
	tr := &Trial{Nonce: "n0", Events: []map[string]any{
		{"type": "agent.message", "content": textBlocks("all done n0")},
	}}
	if got := tr.fill("say {{RECALL}}"); got != "say {{RECALL}}" {
		t.Errorf("fill on a trial with no Recall = %q, want the placeholder left in place", got)
	}
	if err := FinalMessageHas("{{RECALL}}", Either).Check(t, tr); err == nil {
		t.Error("a recall assertion on a trial with no Recall must fail, not pass vacuously")
	}

	// And with the token set it must pass on a message that carries it. Without
	// this direction the test above passes for the wrong reason on a grader that
	// never substitutes {{RECALL}} at all — which is exactly what the live suite
	// caught when only some graders knew the token.
	tr.Recall = "r0"
	tr.Events = []map[string]any{
		{"type": "agent.message", "content": textBlocks("DONE2:n0 r0")},
	}
	if err := FinalMessageHas("{{RECALL}}", Either).Check(t, tr); err != nil {
		t.Errorf("a message carrying the recall token should pass: %v", err)
	}
	if got := tr.fill("say {{RECALL}} about n0"); got != "say r0 about n0" {
		t.Errorf("fill = %q, want both tokens substituted", got)
	}
}

func TestInputTextMatchesWhatTheModelWrote(t *testing.T) {
	// json.Marshal would render this as > and \n; the decoded form is what
	// a marker is matched against.
	got := inputText(map[string]any{"input": map[string]any{
		"command": "echo A > /tmp/x && cat <<'EOF'\nB\nEOF",
	}})
	if !strings.Contains(got, "echo A > /tmp/x") {
		t.Errorf("inputText = %q, want the raw redirect", got)
	}

	// Every string value is included, in key order, one per line — so a marker
	// cannot straddle two fields.
	multi := inputText(map[string]any{"input": map[string]any{
		"path": "/workspace", "pattern": "**/*.go",
	}})
	if multi != "/workspace\n**/*.go\n" {
		t.Errorf("inputText = %q, want both values newline-separated in key order", multi)
	}
	if strings.Contains(multi, "/workspace**") {
		t.Error("values must not be concatenated into a marker nobody wrote")
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

// no-session-error tolerates exactly one thing, and this table says what: a
// clone failure the executor means to retry, on a trial that mounts a
// repository, for a reason that means the network was unwell.
//
// The rows that must NOT be tolerated are the point. retry_status is "retrying"
// for every reason the executor emits, so the reason is the only thing standing
// between "a GitHub blip the platform recovered from" and "our own bug arriving
// under a rule written for the network's" — and a grader that quietly stopped
// noticing the second would be worse than the unactionable red the tolerance
// was added to remove.
func TestNoSessionErrorToleratesOnlyARetriedTransientClone(t *testing.T) {
	cloneErr := func(reason, retry string) map[string]any {
		return map[string]any{
			"type": "session.error",
			"error": map[string]any{
				"type":         "github_repository_clone_error",
				"reason":       reason,
				"retry_status": map[string]any{"type": retry},
			},
		}
	}
	byName := func(task Task) Grader {
		t.Helper()
		for _, g := range corePack(task) {
			if g.Name == "no-session-error" {
				return g
			}
		}
		t.Fatal("core pack has no no-session-error grader")
		return Grader{}
	}
	repoTask := Task{Turns: []Turn{{Message: "x"}}, Repo: &RepoFixture{MountPath: "/workspace/fixture"}}
	plainTask := Task{Turns: []Turn{{Message: "x"}}}
	repoGrader, plainGrader := byName(repoTask), byName(plainTask)

	for _, tc := range []struct {
		name      string
		grader    Grader
		ev        map[string]any
		tolerated bool
	}{
		{"a retried network failure on a repository trial", repoGrader, cloneErr("network", "retrying"), true},
		{"a retried timeout on a repository trial", repoGrader, cloneErr("timeout", "retrying"), true},
		{"a retried auth failure — a bad token is actionable", repoGrader, cloneErr("auth", "retrying"), false},
		{"a retried not_found — the wrong repository is actionable", repoGrader, cloneErr("not_found", "retrying"), false},
		{"a retried internal failure — that one is ours", repoGrader, cloneErr("internal", "retrying"), false},
		{"a network failure the platform gave up on", repoGrader, cloneErr("network", "exhausted"), false},
		{"the very same event, on a trial that mounts nothing", plainGrader, cloneErr("network", "retrying"), false},
		{"a session.error that is not a clone at all", repoGrader,
			map[string]any{"type": "session.error", "error": map[string]any{"type": "model_error"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.grader.Check(t, &Trial{Events: []map[string]any{tc.ev}})
			if tolerated := err == nil; tolerated != tc.tolerated {
				t.Errorf("tolerated = %v, want %v (grader said: %v)", tolerated, tc.tolerated, err)
			}
		})
	}
}

func TestSpawnedAgent(t *testing.T) {
	spawn := func(id, name string) map[string]any {
		return map[string]any{
			"id": id, "type": "agent.tool_use", "name": "create_agent",
			"input": map[string]any{"agent_name": name, "message": "ask the archivist for the code"},
		}
	}
	answer := func(useID string, isErr bool) map[string]any {
		return map[string]any{"type": "agent.tool_result", "tool_use_id": useID, "is_error": isErr}
	}
	tr := trialWith([]map[string]any{spawn("sevt_1", "herald"), answer("sevt_1", false)})

	if err := SpawnedAgent("herald", Model).Check(t, tr); err != nil {
		t.Errorf("want pass for the agent that was spawned: %v", err)
	}
	// The archivist is named in the herald's task text and nowhere else, which
	// is the exact confusion this grader reads the field to avoid.
	err := SpawnedAgent("archivist", Model).Check(t, tr)
	if err == nil {
		t.Fatal("want failure for an agent merely mentioned in another spawn's message")
	}
	if !strings.Contains(err.Error(), "herald") {
		t.Errorf("failure %q does not say who was spawned instead", err)
	}
	if spawnedAgent("archivist")(tr) {
		t.Error("the premise holds for an agent that was never spawned")
	}
	if !spawnedAgent("herald")(tr) {
		t.Error("the premise does not hold for the agent that was spawned")
	}

	// A call the settlement refused is the model asking wrongly, not a spawn —
	// and counting it would hold open the premise of the Platform grader beside
	// this one, which would then red for a malformed call.
	refused := trialWith([]map[string]any{spawn("sevt_2", "herald"), answer("sevt_2", true)})
	if err := SpawnedAgent("herald", Model).Check(t, refused); err == nil {
		t.Error("want failure for a create_agent the settlement answered is_error")
	}
	if spawnedAgent("herald")(refused) {
		t.Error("the premise holds for a spawn the settlement refused")
	}

	// A settlement answers every delegation call in the commit that emits it, so
	// an unanswered create_agent is a turn that never settled.
	unsettled := trialWith([]map[string]any{spawn("sevt_3", "herald")})
	if err := SpawnedAgent("herald", Model).Check(t, unsettled); err == nil {
		t.Error("want failure for a create_agent no result answers")
	}
}

// The two create-agent bodies the harness builds, offline. The roster arm would
// otherwise first be exercised by a paid live run; the single-agent arm is here
// for the stronger reason — it must still send exactly the four keys it sent
// before rosters existed, since a stray key there would change every other trial
// in the suite at once.
func TestAgentBodies(t *testing.T) {
	tr := &Trial{Nonce: "n0", Recall: "r0"}

	plain := agentBody(Task{ID: "t", System: "be brief"}, "m", tr, nil, "", nil)
	if got, want := slices.Sorted(maps.Keys(plain)), []string{"model", "name", "system", "tools"}; !slices.Equal(got, want) {
		t.Errorf("a task with no roster sent keys %v, want %v", got, want)
	}

	coord := agentBody(Task{ID: "t"}, "m", tr, nil, "", []any{"agent_1", "agent_2"})
	ma, _ := coord["multiagent"].(map[string]any)
	if ma["type"] != "coordinator" {
		t.Errorf("multiagent.type = %v, want coordinator", ma["type"])
	}
	if agents, _ := ma["agents"].([]any); len(agents) != 2 || agents[0] != "agent_1" || agents[1] != "agent_2" {
		t.Errorf("multiagent.agents = %v, want the two member ids as bare strings, in roster order", ma["agents"])
	}

	worker := memberBody(RosterMember{Name: "archivist", System: "the code is {{RECALL}}", Toolset: true}, "m", tr)
	if worker["system"] != "the code is r0" {
		t.Errorf("member system = %v, want the recall token filled", worker["system"])
	}
	tools, _ := worker["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("a member with the toolset was given tools %v, want the one bare toolset", worker["tools"])
	}
	if entry, _ := tools[0].(map[string]any); entry["type"] != "agent_toolset_20260401" {
		t.Errorf("member toolset entry = %v, want the bare agent toolset", tools[0])
	}
	// Sent empty rather than omitted: this member is offered its two delegation
	// tools and nothing else.
	if got, _ := memberBody(RosterMember{Name: "herald"}, "m", tr)["tools"].([]any); len(got) != 0 {
		t.Errorf("a member with no toolset was given tools %v", got)
	}
}

// threadTrial stands a trial up over a canned threads route. The thread graders
// read the session's topology off the wire, which is the one thing a
// hand-written transcript cannot carry — so this serves the page instead, and
// the graders reach it through the same client every live trial uses.
func threadTrial(t *testing.T, threads ...map[string]any) *Trial {
	t.Helper()
	data := make([]any, len(threads))
	for i, th := range threads {
		data[i] = th
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Errorf("serve the threads page: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return &Trial{Nonce: "n0", Recall: "r0", SessionID: "sesn_1", stack: &stack{url: srv.URL}}
}

// threadRow is one row of that page. An empty parent renders the JSON null the
// primary carries rather than a missing key, because that field is what the
// graders discriminate on.
func threadRow(id, parent, agent, status string) map[string]any {
	row := map[string]any{
		"id":               id,
		"type":             "session_thread",
		"session_id":       "sesn_1",
		"status":           status,
		"agent":            map[string]any{"name": agent},
		"parent_thread_id": nil,
	}
	if parent != "" {
		row["parent_thread_id"] = parent
	}
	return row
}

func TestThreadPerAgent(t *testing.T) {
	g := ThreadPerAgent([]string{"archivist", "herald"}, Platform)
	primary := threadRow("sthr_p", "", "lead", "idle")

	both := threadTrial(t, primary,
		threadRow("sthr_a", "sthr_p", "archivist", "idle"),
		threadRow("sthr_h", "sthr_p", "herald", "idle"))
	if err := g.Check(t, both); err != nil {
		t.Errorf("want pass with a child thread per roster agent: %v", err)
	}

	one := threadTrial(t, primary, threadRow("sthr_a", "sthr_p", "archivist", "idle"))
	if err := g.Check(t, one); err == nil {
		t.Error("want failure when only one of the two workers ran a thread")
	}

	// The same two agents, but the herald hangs off the archivist rather than
	// the primary — a depth this platform never builds.
	deep := threadTrial(t, primary,
		threadRow("sthr_a", "sthr_p", "archivist", "idle"),
		threadRow("sthr_h", "sthr_a", "herald", "idle"))
	if err := g.Check(t, deep); err == nil {
		t.Error("want failure when a child is parented to a sibling rather than the primary")
	}

	// Every row a child: the primary is missing from the list entirely.
	noPrimary := threadTrial(t,
		threadRow("sthr_a", "sthr_p", "archivist", "idle"),
		threadRow("sthr_h", "sthr_p", "herald", "idle"))
	if err := g.Check(t, noPrimary); err == nil {
		t.Error("want failure when no listed thread is the primary")
	}
}

func TestEveryThreadIdle(t *testing.T) {
	g := EveryThreadIdle(Platform)
	primary := threadRow("sthr_p", "", "lead", "idle")

	settled := threadTrial(t, primary, threadRow("sthr_a", "sthr_p", "archivist", "idle"))
	if err := g.Check(t, settled); err != nil {
		t.Errorf("want pass when every thread is idle: %v", err)
	}

	running := threadTrial(t, primary, threadRow("sthr_a", "sthr_p", "archivist", "running"))
	err := g.Check(t, running)
	if err == nil {
		t.Fatal("want failure when a child thread is still running")
	}
	// The message names the thread that did not settle, because the reader was
	// not watching the run.
	if !strings.Contains(err.Error(), "archivist") || !strings.Contains(err.Error(), "sthr_a") {
		t.Errorf("failure %q names neither the thread nor its agent", err)
	}
}
