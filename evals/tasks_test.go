package evals

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
	return []Task{fibQuickstart(), echoNoTool(), shellState()}
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
