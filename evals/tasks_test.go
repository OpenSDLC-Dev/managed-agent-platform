package evals

import (
	"fmt"
	"strings"
)

// The task set. Each entry is a small, self-contained claim about the platform,
// written so that the only way to satisfy it is for the whole chain to work:
// REST accepted the session, the brain called the model, the queue carried the
// tool call, the executor ran it in a container, the result came back through
// the log, and the brain woke and finished.
//
// Prompts are written the way the docs tell a user to write them — plain
// English, no incantations — because a prompt tuned until only our platform's
// quirks satisfy it would stop being a regression test. Where a prompt is
// specific, it is specific about the artifact (a path, an exact token), never
// about how to produce it.

// fib20 is the first 20 Fibonacci numbers from 0. Spelled out rather than
// computed: a grader that derives its expectation from the same definition the
// prompt gives could agree with a wrong prompt. These are the numbers.
var fib20 = []string{
	"0", "1", "1", "2", "3", "5", "8", "13", "21", "34",
	"55", "89", "144", "233", "377", "610", "987", "1597", "2584", "4181",
}

func tasks() []Task {
	return []Task{
		fibQuickstart(), echoNoTool(), shellState(),
		editConfig(), needleSearch(), permAllow(), permDeny(),
		exitCode(), journalMultiturn(), viewRange(),
	}
}

// fibQuickstart is the reference quickstart, kept deliberately close to the
// published flow: it is the shape a first-time user copies, so if it breaks for
// them it must break here first.
//
// It is the suite's broadest single test: producing the file at all requires the
// model to call a tool, suspend, and be woken with the result, so a pass means
// the whole async loop closed at least once. It need not close more than once —
// a single compound command can write the script, run it, and capture the
// output — which is exactly why this task grades the file, not a tool count.
func fibQuickstart() Task {
	return Task{
		ID: "fib-quickstart",
		Turns: []Turn{{Message: "Write a Python script that computes the first 20 " +
			"Fibonacci numbers starting from 0, run it, and save its output to " +
			"/workspace/fibonacci.txt with one number per line and nothing else. " +
			"When the file is correct, reply with DONE:{{NONCE}}"}},
		Graders: []Grader{
			// The artifact is the assertion: these exact numbers, in this
			// order, in that file. Nothing but a tool that ran can put them
			// there, and the core pack's tool-results-joined grader proves the
			// loop that ran it closed — so no separate tool-count grader is
			// needed here, and a count would misfire on a model that did the
			// whole thing in one compound command.
			FileLines("fibonacci.txt", fib20, Platform),
			ToolResultOK(Platform),
			// Either: the file being right proves the platform worked, so a
			// missing token here is most likely the model forgetting to say it.
			FinalMessageHas("DONE:{{NONCE}}", Either),
		},
	}
}

// echoNoTool is the negative baseline: the platform must be able to do nothing.
//
// It is cheap and it catches a real class of bug that every other task is blind
// to — work invented on a session that asked for none. If the executor
// provisioned a container for a text-only turn, every task above would still
// pass and this one would fail.
func echoNoTool() Task {
	return Task{
		ID:     "echo-notool",
		System: "Answer directly from your own knowledge. Do not use any tools.",
		Turns:  []Turn{{Message: "Reply with exactly ECHO:{{NONCE}} and nothing else."}},
		Graders: []Grader{
			// Asserted before anything else here would provision one: this
			// grader is why the harness asks Docker directly instead of going
			// through the sandbox provider.
			ContainerAbsent(Platform),
			// Model: a model that reaches for bash to echo a string is being
			// silly, not evidence of a broken product.
			NoToolUse(Model),
			// Either: the core pack already proves the platform delivered the
			// turn and streamed a reply, so a missing marker here is most likely
			// the model not echoing as asked — but a mangled delivery would look
			// the same from the final message alone, so the class does not commit.
			FinalMessageHas("ECHO:{{NONCE}}", Either),
		},
	}
}

// shellState pins the persistent shell, which is the most intricate machinery in
// the toolset: bash state does not survive a call by itself — each call is its
// own exec process — so cwd and exported variables are carried by a snapshot and
// restore around every command (internal/sandbox/shell). Three commands that
// must see each other's effects is the smallest thing that can prove it.
//
// The variable is exported deliberately. The snapshot draws its line at `export`
// (a plain variable is documented not to carry, because nothing in `declare`
// separates a user's plain variables from bash's own internals), so a task built
// on a plain one would be asserting a divergence the package states it has.
func shellState() Task {
	return Task{
		ID: "shell-state",
		System: "Use the bash tool. Run each command as its own separate bash call, " +
			"in order. Do not combine them into one command.",
		Turns: []Turn{{Message: "Run these three bash commands, one per call:\n" +
			"1. export MARK={{NONCE}}\n" +
			"2. echo \"$MARK\" > /workspace/mark.txt\n" +
			"3. cat /workspace/mark.txt\n" +
			"Then tell me what the third command printed."}},
		Graders: []Grader{
			// For a model that does as asked — export in one call, `echo "$MARK"`
			// into the file in another — the file holds the nonce only if the
			// export survived to the second call. An empty file is the shape of a
			// snapshot regression, and a platform bug. That "as asked" is the load
			// bearing assumption: the model here is the system under test, not an
			// adversary, so the graders below steer it onto the instructed path
			// and catch a regression there; they do not try to defend against a
			// model that deliberately writes the literal nonce, which no
			// transcript-only grader can distinguish from a working shell.
			FileLines("mark.txt", []string{"{{NONCE}}"}, Platform),
			// Steer the model onto the path the file check can trust: at least two
			// bash calls, the export not packed into the write (so it cannot
			// trivially hold within one shell), and the write done by a bash call
			// that read "$MARK" (not the `write` tool, which would bypass the
			// shell entirely). Class Model — these describe following the
			// instruction, and a miss means re-prompt, not a platform bug.
			ToolUseAtLeast("bash", 2, Model),
			SeparateBashCalls("MARK=", "mark.txt"),
			BashCommandWith("$MARK", "mark.txt"),
			// The nonce came back out of the container through a tool result:
			// the round trip, not just the write.
			ToolResultContains("{{NONCE}}", Platform),
		},
	}
}

// editConfig pins the edit tool's surgical replace: change one placeholder and
// nothing else. Byte-equality is the assertion — a model that rewrote the file,
// even to plausible content, would drift a byte (a trailing newline, the key
// order) and fail.
//
// The load-bearing caveat is shell-state's: the file check trusts a cooperative
// model to edit rather than rewrite. ToolNotUsed(write) steers it onto the edit
// tool and names the miss as the model's when it sidesteps, but no
// transcript-only grader can stop a model that rewrites the file byte-perfectly
// by hand — which is why the byte check is the platform signal and the tool
// checks are the model's.
func editConfig() Task {
	const seed = "[service]\nname = eval\ntoken = REPLACE_ME\nretries = 3\n"
	const want = "[service]\nname = eval\ntoken = {{NONCE}}\nretries = 3\n"
	return Task{
		ID:    "edit-config",
		Seeds: []Seed{{Path: "config.ini", Content: seed}},
		Turns: []Turn{{Message: "The file /workspace/config.ini contains the placeholder " +
			"REPLACE_ME. Read the file, then replace REPLACE_ME with {{NONCE}}, changing " +
			"nothing else. When the file is updated, reply DONE:{{NONCE}}."}},
		Graders: []Grader{
			FileEquals("config.ini", want, Platform),
			ToolUseAtLeast("read", 1, Model),
			ToolUseAtLeast("edit", 1, Model),
			ToolNotUsed("write", Model),
			FinalMessageHas("DONE:{{NONCE}}", Either),
		},
	}
}

// needleSearch pins the search tools' output contracts: glob to enumerate and
// grep to locate, with grep's path:line:text line shape asserted against the one
// seeded needle among decoys. The nonce is what makes the needle findable and the
// decoys not: a case-sensitive grep for NEEDLE_{{NONCE}} passes over the
// lowercase "needle" decoy.
//
// grep carries the graded contract; glob is exercised by the prompt but not
// graded, because its output (a bare list of paths) is only meaningful once the
// model has acted on it. The cooperative-model caveat applies as in shell-state:
// the grep-result grader means something only when the model actually greps for
// the needle, which the prompt asks and the grep-used grader steers.
func needleSearch() Task {
	return Task{
		ID: "needle-search",
		Seeds: []Seed{
			{Path: "src/util/helpers.go", Content: "package util\n\n// NEEDLE_{{NONCE}} marks the spot\nfunc Help() int { return 0 }\n"},
			{Path: "src/main.go", Content: "package main\n\nfunc main() {}\n"},
			{Path: "src/util/other.go", Content: "package util\n\nfunc Other() {}\n"},
			{Path: "src/decoy.go", Content: "package src\n\n// a needle in a haystack (decoy, lowercase)\nvar X = 1\n"},
		},
		Turns: []Turn{{Message: "Search /workspace for the Go source file that contains the exact " +
			"text NEEDLE_{{NONCE}}. Use the glob tool to list the .go files and the grep tool to " +
			"find the match. Write the location to /workspace/answer.txt as a single line " +
			"`path:line` — the path relative to /workspace, e.g. src/foo.go:12 — then reply DONE:{{NONCE}}."}},
		Graders: []Grader{
			// grep runs with an absolute root, so it emits absolute paths; the
			// optional /workspace/ prefix accepts that and a relative rewrite both.
			ToolResultMatches(`(?m)^(/workspace/)?src/util/helpers\.go:3:`, Platform),
			ToolUseAtLeast("grep", 1, Model),
			FileMatches("answer.txt", `^(/workspace/)?src/util/helpers\.go:3$`, Either),
			FinalMessageHas("DONE:{{NONCE}}", Either),
		},
	}
}

// permAllow pins the happy path of the permission bridge: a gated tool suspends
// the session on requires_action, a confirmation releases it, and the tool runs
// — with the result provably sequenced after the approval, so the gate is not
// cosmetic. The toolset gates every tool via default_config, and the prompt uses
// only bash, so the one pause is the bash call.
//
// The bridge graders are Platform because the pause, the ask stamp, and the
// post-approval result are the product behaviour under test; they carry the same
// cooperative-model caveat as the file tasks, since they mean something only once
// the model calls the gated tool the prompt asks for.
func permAllow() Task {
	return Task{
		ID:    "perm-allow",
		Tools: gatedToolset(),
		Turns: []Turn{{
			Message: "Use the bash tool to write the text GATED_{{NONCE}} to /workspace/gated.txt " +
				"(for example, `echo GATED_{{NONCE}} > /workspace/gated.txt`). When the file is " +
				"written, reply DONE:{{NONCE}}.",
			OnAsk: &Ask{Allow: true},
		}},
		Graders: []Grader{
			RequiresActionRaised(Platform),
			EvaluatedPermissionAsk("bash", Platform),
			ResultAfterConfirmation(Platform),
			FileLines("gated.txt", []string{"GATED_{{NONCE}}"}, Platform),
			FinalMessageHas("DONE:{{NONCE}}", Either),
		},
	}
}

// permDeny is the negative twin: the same gate, but the confirmation denies, and
// the platform synthesizes an is_error tool_result carrying the deny message
// instead of running the tool. The action is a benign append the reviewer
// happens to decline — deliberately benign, because a task that asks the model to
// delete a "protected" file tests the model's refusal reflex, not our denial
// path; the seeded file being unchanged afterwards is the proof the command never
// ran.
func permDeny() Task {
	return Task{
		ID:    "perm-deny",
		Tools: gatedToolset(),
		Seeds: []Seed{{Path: "notes.txt", Content: "ORIGINAL_{{NONCE}}\n"}},
		Turns: []Turn{{
			Message: "Use the bash tool to append a line to /workspace/notes.txt by running " +
				"`echo APPEND_{{NONCE}} >> /workspace/notes.txt`. If the command is blocked before " +
				"it runs, reply DENIED:{{NONCE}}; if it runs, reply DONE:{{NONCE}}.",
			OnAsk: &Ask{Allow: false, DenyMessage: "not approved: DENY_{{NONCE}}"},
		}},
		Graders: []Grader{
			RequiresActionRaised(Platform),
			ToolErrorResultContains("DENY_{{NONCE}}", Platform),
			ResultAfterConfirmation(Platform),
			// The denied append never ran, so the seed is byte-for-byte intact.
			FileLines("notes.txt", []string{"ORIGINAL_{{NONCE}}"}, Platform),
			FinalMessageHas("DENIED:{{NONCE}}", Either),
		},
	}
}

// exitCode pins tool-failure propagation and guards against a hallucinated
// answer: a command that exits non-zero must come back as an is_error result
// whose content carries the exit-code trailer, and the model can only report the
// code by having consumed that result. The exit code exists nowhere but the real
// tool output — cat of a missing file exits 1 — so a correct EXIT:…:1 proves the
// model read the true result rather than guessing.
func exitCode() Task {
	return Task{
		ID: "exit-code",
		Turns: []Turn{{Message: "Use the bash tool to run exactly `cat /workspace/missing_{{NONCE}}.txt`. " +
			"That file does not exist, so the command fails. Report the exact exit code it returned, " +
			"in the form EXIT:{{NONCE}}:<code>."}},
		Graders: []Grader{
			// Either: the nonce'd path missing from every bash input is either the
			// model running a different command or a mis-joined streamed tool JSON,
			// and the transcript alone cannot separate them.
			ToolUseInputContains("bash", "missing_{{NONCE}}", Either),
			ToolErrorResultContains("exit code: 1", Platform),
			FinalMessageHas("EXIT:{{NONCE}}:1", Either),
		},
	}
}

// journalMultiturn pins two turns on one session: the second must resume the
// first's context (event replay) and see the first's file (the same container,
// adopted again). The final file holding both lines is the workspace-persisted
// signal, and a second model-request span is the resume actually re-invoking the
// model.
//
// The caveat: a model could reconstruct the first line from its replayed context
// rather than from the file, so the file check does not by itself prove the
// container was reused — but same-session containers are the same container by
// construction (the executor adopts by session), and the span count proves the
// resume ran. Stated honestly, this is a persistence-and-replay test, not a
// defence against a model rewriting the file from memory.
func journalMultiturn() Task {
	return Task{
		ID: "journal-multiturn",
		Turns: []Turn{
			{Message: "Create /workspace/journal.txt with a single first line reading exactly: " +
				"entry-one-{{NONCE}}. Reply DONE1:{{NONCE}}."},
			{Message: "Append a second line to /workspace/journal.txt, below the first, reading " +
				"exactly: entry-two-{{NONCE}}. Keep the first line unchanged. Reply DONE2:{{NONCE}}."},
		},
		Graders: []Grader{
			FileLines("journal.txt", []string{"entry-one-{{NONCE}}", "entry-two-{{NONCE}}"}, Platform),
			EventCountAtLeast("user.message", 2, Platform),
			EventCountAtLeast("span.model_request_end", 2, Platform),
			FinalMessageHas("DONE2:{{NONCE}}", Either),
		},
	}
}

// viewRange pins read's view_range slicing byte-for-byte: read line 57 of a
// 100-line file and it must be exactly line 57, not its neighbour and not line 57
// plus a stray newline. The seeded marker lives only on that line, so an
// off-by-one in the slicer returns the wrong bytes and the read-range grader
// names it. The marker is a plain token and the task is a plain copy: a "SECRET"
// on the line read as something to exfiltrate and made the model decline.
func viewRange() Task {
	return Task{
		ID:    "view-range",
		Seeds: []Seed{{Path: "poem.txt", Content: poem()}},
		Turns: []Turn{{Message: "The file /workspace/poem.txt has 100 numbered lines. Using the read " +
			"tool's line-range feature, read only line 57. Then copy that exact line into a new file " +
			"/workspace/line57.txt, and reply DONE:{{NONCE}}."}},
		Graders: []Grader{
			ReadRangeLine("poem.txt", 57, "MARKER_{{NONCE}}", Either),
			FileLines("line57.txt", []string{"MARKER_{{NONCE}}"}, Platform),
			FinalMessageHas("DONE:{{NONCE}}", Either),
		},
	}
}

// gatedToolset is the built-in toolset with every tool set to always_ask, so the
// first tool call suspends on requires_action. Gating via default_config (rather
// than per-tool configs) keeps it simple: the permission tasks use only bash, so
// one policy covers the one tool they call.
func gatedToolset() []any {
	return []any{map[string]any{
		"type":           "agent_toolset_20260401",
		"default_config": map[string]any{"permission_policy": map[string]any{"type": "always_ask"}},
	}}
}

// poem builds the 100-line seed for view-range, with the nonce'd secret on line
// 57. The other lines are numbered so a wrong slice is obvious in a failure
// message (line-56 or line-58 instead of the secret).
func poem() string {
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%d", i+1)
	}
	lines[56] = "MARKER_{{NONCE}}" // line 57, 1-indexed
	return strings.Join(lines, "\n") + "\n"
}
