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

// tasks is the registered set. repo-answer rejoined it in #358, once the private
// fixture repository and the fine-grained read-only token it needs existed;
// while they did not it was mandatory rather than opt-in — the scheduled job
// runs `make eval`, which sets RUN_EVALS=1, and repoConfig fails rather than
// skips — so it aborted before creating its session and reddened every nightly.
//
// The set is pinned by TestTaskSetIsPinned, which runs offline on every
// `make verify`: adding or dropping a trial here is meant to fail it, because
// the count is spelled out in two documents that nothing else can hold to it.
func tasks() []Task {
	return []Task{
		fibQuickstart(), echoNoTool(), shellState(),
		editConfig(), needleSearch(), permAllow(), permDeny(),
		exitCode(), journalMultiturn(), viewRange(), skillAnswer(),
		fileAnswer(), repoAnswer(), mcpAnswer(), outcomeSatisfy(), outcomeRevise(),
		coordinatorTeam(), memoryRecall(), memoryWrite(),
	}
}

// memoryRecall is the memory chain end to end, store to model (plan 36 slice
// 4): a passphrase lives only in a memory seeded into a store attached
// read_only, so a correct answer proves store → session attachment →
// executor materialization → the brain's Memory-stores block → the agent
// reading the mount. The Recall token appears in no prompt — only in the
// memory's bytes.
func memoryRecall() Task {
	const mount = "/mnt/memory/notes"
	return Task{
		ID: "memory-recall",
		Memory: &MemoryFixture{Name: "notes", Access: "read_only",
			Memories: map[string]string{"/passphrase.md": "The passphrase is {{RECALL}}."}},
		Turns: []Turn{{Message: "A memory store is mounted in your sandbox. " +
			"What is this task's passphrase, as saved in your memory? Reply with exactly the passphrase and nothing else."}},
		Graders: []Grader{
			// Either for both, as fileAnswer: the passphrase is reachable only
			// through the mount, so a right answer is unambiguous, while a
			// missing one may be the model declining to read.
			ReadsFile(mount+"/passphrase.md", Either),
			FinalMessageHas("{{RECALL}}", Either),
		},
	}
}

// memoryWrite is the other direction, model to store: the agent is told a
// fact and asked to save it in its memory store, and the store holding it
// after the turn proves the run-end sync pushed what the agent wrote
// (decision 11) with the session as the version's actor.
func memoryWrite() Task {
	const mount = "/mnt/memory/notes"
	return Task{
		ID:     "memory-write",
		Memory: &MemoryFixture{Name: "notes", Access: "read_write"},
		Turns: []Turn{{Message: "Remember this for future sessions: the project's code name is {{RECALL}}. " +
			"Save it in your memory store as a file named codename.md, then reply with exactly the word saved."}},
		Graders: []Grader{
			// Either: a model may decline to write. Platform for the sync: once
			// the transcript shows the write, only the platform can lose it.
			WritesFile(mount+"/codename.md", Either),
			MemorySynced(mount+"/codename.md", "/codename.md", "{{RECALL}}", Platform),
		},
	}
}

// coordinatorTeam is the coordinator topology end to end (plan 35 slice 4): one
// session, two worker threads, and an answer no single agent could give. Two
// codes exist in this trial, each reachable by exactly one of the two workers
// and by nothing else — the archive code is a line in a seeded file the
// coordinator has no tool to open, the herald code is in a system prompt only
// the herald's own thread is given — so a final message carrying both means the
// coordinator spawned both agents as real threads, each ran its own turn under
// its own agent, both reported, and both reports reached the summary.
//
// The turn names no delegation tool, for the reason mcpAnswer's names no MCP
// tool: what tells the model it may delegate at all is the four tools the brain
// injects for the primary thread of a session whose snapshot carries a roster,
// and that injection is the link under test. It does name the two agents, in the
// coordinator's own system prompt, because nothing else can: the platform
// surfaces the roster to the model nowhere (list_agents lists live threads, and
// at turn one there are none), so a coordinator never told its team's names could
// only find them by spawning a wrong one and reading the error back. Naming them
// is configuration rather than instruction: which agent to ask for what, and
// when, is still the model's.
//
// The coordinator takes an empty toolset rather than the bare one, and that is
// what keeps the trial honest: with bash and read it could open the archive file
// itself and answer having delegated nothing. With its four delegation tools and
// no others, every fact in its answer came from a child.
//
// The workers' own prompts do name submit_result, and that asymmetry is the
// point rather than a lapse. What this trial tests is the coordinator half: that
// a rostered primary thread is offered tools no agent declared, and that the
// platform turns a call on one into a real second thread. A worker's prompt is
// this trial's configuration of the worker, and a real deployment writes it the
// same way. The first live run settled it empirically: both children spawned,
// both ran, and both ended their turns having answered in plain text, so the
// coordinator was told twice that nothing had been reported and said so. The
// platform did every part of that correctly — which is exactly why leaving the
// instruction implicit would have graded the model's tool-selection habits
// rather than the delegation path this slice built.
//
// The two workers are deliberately unlike each other. The archivist has the bare
// toolset and must read a seeded file — the shared sandbox, reached from a child
// thread. The herald has no toolset at all and answers out of its own system
// prompt, which no other thread's request carries: its code is the proof that the
// child ran as its own agent rather than as a second voice of the coordinator's.
//
// Both per-trial tokens are spent as hidden secrets, the nonce included. Two
// workers need two independent values, the harness offers exactly two, and
// neither appears in anything the coordinator is told — so this trial demands no
// marker back: the codes are the assertion, and a coordinator that answered from
// its own knowledge can spell neither.
func coordinatorTeam() Task {
	const archivePath = "/workspace/archive.txt"
	return Task{
		ID: "coordinator-team",
		// Empty rather than nil — the bare toolset would let the coordinator do
		// the work itself; see above.
		Tools: []any{},
		System: "You coordinate a team of two agents: archivist and herald. Neither of them can " +
			"see this conversation, so tell each what you need from it, and give your answer only " +
			"once both have reported back.",
		Roster: []RosterMember{{
			Name:    "archivist",
			Toolset: true,
			System: "You are the archivist. Your team's archive code is the one written in " +
				archivePath + ". Read that file, then report the code exactly as written by " +
				"calling submit_result.",
		}, {
			Name: "herald",
			System: "You are the herald. Your team's herald code is {{RECALL}}. Report it " +
				"exactly as written by calling submit_result.",
		}},
		Seeds: []Seed{{Path: archivePath, Content: "archive code: {{NONCE}}\n"}},
		Turns: []Turn{{Message: "I need both of my team's codes. Reply with one line: " +
			"CODES <archive code> <herald code>."}},
		Graders: []Grader{
			// The model's half, one grader per worker: it must have spawned each
			// of them by name. These own the "never delegated" miss, which is
			// what lets the thread-row grader below stay Platform.
			SpawnedAgent("archivist", Model),
			SpawnedAgent("herald", Model),
			// Platform, premised on those same two spawns — spawnedAgent is
			// SpawnedAgent's condition over the same finder, so the pair can
			// never disagree about whether the spawn happened. What is left is
			// the platform's alone: a spawn the model asked for must leave a
			// live child thread running that agent under the primary. No other
			// trial in the suite makes this claim — the session ran more than
			// one thread, and the threads route says which.
			// One grader per name, each under its own premise. Pairing both names
			// into one OnlyIf would make the whole check vacuous the moment
			// either spawn was missing — including the assertion about the agent
			// that *was* spawned, whose thread row is the platform claim this
			// trial exists to make. A partial spawn is the model's miss, and the
			// SpawnedAgent graders above already own it.
			OnlyIf(ThreadPerAgent([]string{"archivist"}, Platform), spawnedAgent("archivist")),
			OnlyIf(ThreadPerAgent([]string{"herald"}, Platform), spawnedAgent("herald")),
			// Platform, and no premise needed: the core pack has already read the
			// session's status idle, and that status is a fold over exactly these
			// rows, so one of them still running is the fold disagreeing with
			// itself. A session that spawned nothing passes it trivially, which
			// is the Model graders' window above.
			EveryThreadIdle(Platform),
			// Either for both codes, as the other answer-style trials' content
			// checks are: each code is reachable only through its own worker, so
			// both of them in the final message is unambiguous platform
			// evidence — while a missing one is as likely a coordinator that
			// concluded before its team reported, and the graders above are the
			// arbiters on that miss.
			FinalMessageHas("{{NONCE}}", Either),
			FinalMessageHas("{{RECALL}}", Either),
		},
	}
}

// mcpAnswer is the MCP chain end to end (plan 29 slice 7): a passphrase lives
// only in what an MCP server's tool returns, so a correct answer proves
// discovery (the executor dialling the declared server and writing its listing
// into mcp_catalogs), the brain offering that listing to the model as
// mcp__{server}__{tool}, the executor answering the call it comes back with, and
// the result reaching the next request.
//
// The turn names no tool and no server: it asks only for the passphrase. What
// tells the model a tool can answer is the listing the brain assembled from the
// catalog, which is the link under test — a prompt that named the tool would let
// the model succeed on a listing that never arrived by guessing the name, and the
// platform would look healthy with discovery broken.
//
// The server runs in this test binary but is reached over real HTTP by the real
// executor through the real MCP client, on an address the platform's dial guard
// admits (see mcpHosts). Nothing here is a fake: the fixture is a go-sdk server.
func mcpAnswer() Task {
	return Task{
		ID: "mcp-answer",
		// Empty rather than nil, which would take the bare agent toolset: the
		// only legitimate action here is one MCP call, and offering bash and the
		// file tools alongside it would let the model spend real turns searching
		// a sandbox the passphrase is not in — and make whether the trial
		// produces any agent.tool_use at all a property of the model.
		Tools: []any{},
		MCP: &MCPFixture{
			Name:        "vault",
			Tool:        "read_passphrase",
			Description: "Returns this task's passphrase.",
			Answer:      "The passphrase is {{RECALL}}.",
		},
		Turns: []Turn{{
			// The provenance sentence is load-bearing, and it is what the other
			// mounted-answer trials say too ("A file has been mounted into your
			// sandbox"). Without it the model reads the ask as an injection
			// attempt and declines on principle — observed, with the platform
			// working perfectly underneath. The provenance sentence names the
			// mechanism and not the tool, so the listing is still what has to
			// arrive for the model to know what to call. The word "secret" is
			// gone too: it was associated with the same class of declines in
			// the measured A/B recorded beside repoAnswer's wording note.
			Message: "This task has attached an MCP server to your session. " +
				"What is this task's passphrase? " +
				"Reply with exactly the passphrase and nothing else.",
			// An mcp_toolset gates on human confirmation by default (issue #26,
			// extended to the MCP arm), so the trial takes the default path
			// rather than configuring the gate away — which puts slice 3's MCP
			// confirmation round trip under the same trial as discovery.
			OnAsk: &Ask{Allow: true},
		}},
		Graders: []Grader{
			// Platform for both: the gate is the toolset's documented default, so
			// a call that ran unattended is the platform's own regression and
			// nothing to do with the model. The per-call one is what actually
			// holds it — RequiresActionRaised asks whether the session stopped
			// at all, which any gated call in the trial would satisfy. Neither
			// reds when the model never calls the tool; MCPToolUse, below, owns
			// that as Either.
			RequiresActionRaised(Platform),
			MCPEvaluatedPermissionAsk("vault", "read_passphrase", Platform),
			// Either for the two below, as with the other answer-style trials:
			// the passphrase exists nowhere but the tool's result, so a right
			// answer is unambiguous platform evidence, while a missing one is as
			// likely the model declining to call a tool it was offered. The call
			// grader is what separates the two on a miss.
			MCPToolUse("vault", "read_passphrase", Either),
			FinalMessageHas("{{RECALL}}", Either),
		},
	}
}

// fileAnswer is the file-mount chain end to end (plan E2E-2): a passphrase lives
// only in an uploaded file mounted into the sandbox, so a correct answer proves
// upload → session resource → executor materialization → Level-1 injection →
// the agent reading the mounted path. The Recall token appears nowhere in any
// prompt — only in the file's bytes — so the model cannot spell it without
// reading the mount.
func fileAnswer() Task {
	const mount = "/mnt/session/uploads/answer.txt"
	return Task{
		ID: "file-answer",
		Files: []FileFixture{{
			Name:      "answer.txt",
			Content:   "The passphrase is {{RECALL}}.",
			MountPath: mount,
		}},
		Turns: []Turn{{Message: "A file has been mounted into your sandbox. " +
			"What is this task's passphrase? Reply with exactly the passphrase and nothing else."}},
		Graders: []Grader{
			// Either for both, for the same reason as skillAnswer: the passphrase
			// is reachable only through the materialized mount, so a right answer
			// is unambiguous, but a missing one may be the model declining to read
			// — the read grader's transcript evidence is what separates them.
			ReadsFile(mount, Either),
			FinalMessageHas("{{RECALL}}", Either),
		},
	}
}

// skillAnswer is the skills chain end to end (plan E2E-2): a self-authored skill
// is uploaded to the registry, referenced by the agent at "latest", injected as
// Level-1 metadata by the brain, and materialized into the sandbox by the
// executor. The passphrase lives only in the skill's answer file, reachable only
// by following the SKILL.md — so a correct final answer exercises every link
// (registry → resolution → materialization → injection → the model acting on it),
// not the model's own knowledge.
//
// The turn names no skill and no path: it asks only for the passphrase. The
// injected Level-1 block ("eval-secret - Reveals this task's passphrase.
// (skills/eval-secret/SKILL.md)") is the only thing that reveals a skill can
// answer and where it lives, so injection is the discovery mechanism under test —
// a prompt that announced the skill would let the model succeed by exploring the
// filesystem even if injection regressed. Perfect isolation is impossible
// (materialization is a prerequisite for reading, so the files are always
// discoverable by an ls/glob), which is why both graders are Either: a right
// answer is strong platform evidence, and the transcript (did it read via the
// injected path?) is the arbiter on a miss.
//
// The skill's name keeps the word "secret" on purpose: the measured variant
// behind the 2026-08-31 wording change (repoAnswer's note) kept the
// eval-secret identifier and failed none of its attempts — it is an
// identifier the model quotes back, not prose in the ask — so a future
// "secret" sweep should leave it be.
func skillAnswer() Task {
	return Task{
		ID: "skill-answer",
		Skills: []SkillFixture{{
			Name:        "eval-secret",
			Description: "Reveals this task's passphrase.",
			Body: "This task has a passphrase. It is written in the file answer.txt " +
				"beside this SKILL.md (skills/eval-secret/answer.txt). Read that file and " +
				"report the passphrase exactly as written.",
			Files: map[string]string{
				"answer.txt": "The passphrase is {{RECALL}}.",
			},
		}},
		Turns: []Turn{{Message: "What is this task's passphrase? " +
			"Reply with exactly the passphrase and nothing else."}},
		Graders: []Grader{
			// Either for both: the passphrase is reachable only through the
			// materialized skill, so a right answer is unambiguous platform
			// evidence — but a missing one is as likely the model declining to use
			// the skill as a broken chain, and the transcript (did it read the
			// file?) is what separates them, which the read grader records.
			ReadsSkillFile("eval-secret", "SKILL.md", Either),
			FinalMessageHas("{{RECALL}}", Either),
		},
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
			//
			// Either, not Platform, for both artifact checks: the numbers are
			// the model's arithmetic and the script is the model's code, so a
			// wrong file or a run that errored every time is as likely its
			// mistake as ours. What is unambiguously the platform's on this
			// transcript — every tool_use answered exactly once, the usage
			// accounted, the stream delivering the idle — the core pack already
			// owns as clean Platform checks, so nothing is lost by declining to
			// blame the platform for a bad Fibonacci script.
			FileLines("fibonacci.txt", fib20, Either),
			ToolResultOK(Either),
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
	// The premises both Platform claims below rest on, stated once: the model ran
	// the instructed export carrying this trial's nonce, and wrote the file with a
	// bash call that read the variable back. mark.txt can only hold the nonce if
	// both happened, so a trial that skipped either is a Model miss and the
	// platform has nothing to answer for. Each premise is paired with the Model
	// grader on the same markers, so the window where a Platform check falls
	// silent is exactly the window where a Model check reds.
	//
	// Two markers for the export rather than the literal `export MARK={{NONCE}}`,
	// so the benign quoted spellings (`export MARK="…"`, `export MARK='…'`) still
	// count as having done it.
	//
	// The write marker is the double-quoted `"$MARK"` the prompt spells, not a
	// bare `$MARK`: single quotes are the one spelling that changes the answer.
	// `echo '$MARK' > mark.txt` writes the six literal bytes `$MARK` however
	// healthy the shell snapshot is, so a premise that accepted it would hand a
	// correct platform a Platform red on the file check. Requiring the expanding
	// spelling puts that trial where it belongs: the premise fails, this check
	// goes quiet, and BashCommandWith reds it as the Model miss it is.
	exportMarkers := []string{"export MARK", "{{NONCE}}"}
	writeMarkers := []string{`"$MARK"`, "mark.txt"}
	catMarkers := []string{"cat /workspace/mark.txt"}
	exported := calledWith("bash", exportMarkers...)
	wrote := calledWith("bash", writeMarkers...)
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
			// snapshot regression, and a platform bug. "As asked" used to be an
			// assumption the surrounding Model graders merely steered towards; it
			// is now the check's stated premise, so a model that skips the export
			// or writes something else reds as the Model miss it is and this stays
			// silent. What no transcript-only grader can distinguish remains
			// undefended: a model that deliberately writes the literal nonce looks
			// exactly like a working shell.
			OnlyIf(FileLines("mark.txt", []string{"{{NONCE}}"}, Platform), exported, wrote),
			// Steer the model onto the path the file check can trust — and, for the
			// export and the write, *require* it, since those are the premises
			// above: at least two bash calls, the export carrying this trial's
			// nonce, the export not packed into the write (so it cannot trivially
			// hold within one shell), and the write done by a bash call that read
			// "$MARK" (not the `write` tool, which would bypass the shell
			// entirely). Class Model — these describe following the instruction,
			// and a miss means re-prompt, not a platform bug.
			ToolUseAtLeast("bash", 2, Model),
			ToolCalledWith("bash", exportMarkers, Model),
			SeparateBashCalls("MARK=", "mark.txt"),
			BashCommandWith(writeMarkers...),
			// The nonce came back out of the container through a tool result:
			// the round trip, not just the write. Split into its two halves, so
			// that a model which never runs the third command reds as the Model
			// miss it is and the Platform claim stays about the platform: the
			// pair below says "the model must run the instructed `cat`" (Model)
			// and "if it did, what came back must carry the nonce" (Platform,
			// vacuous otherwise). It carries the same two premises as the file
			// check — a `cat` of a file the model never wrote errors, and that
			// error is the model's, not ours.
			//
			// The marker is the whole command rather than "cat" plus the path:
			// `cat > /workspace/mark.txt <<EOF` carries both of those and is a
			// write, whose empty stdout would then red the platform for a round
			// trip the model never asked for.
			ToolCalledWith("bash", catMarkers, Model),
			OnlyIf(CallResult("bash", catMarkers, false, "{{NONCE}}", Platform), exported, wrote),
		},
	}
}

// editConfig pins the edit tool's surgical replace: change one placeholder and
// nothing else. Whole-file byte-equality is the artifact assertion — a rewrite,
// even to plausible content, drifts a byte (a trailing newline, the key order)
// and fails — and ToolCallResult ties that to an edit that actually performed the
// replacement: its input carries the nonce (so a no-op edit whose old and new
// strings match does not count) and its own result names config.ini and did not
// error (so a bash rewrite of the file cannot stand in for a broken edit).
// ToolNotUsed(write) closes the write-tool sidestep.
//
// Both the byte check and the correlated edit are Either: a wrong file is the
// platform's edit misbehaving or the model rewriting it clumsily, and the
// transcript cannot separate the two.
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
			FileEquals("config.ini", want, Either),
			ToolUseAtLeast("read", 1, Model),
			ToolCallResult("edit", "{{NONCE}}", false, "config.ini", Either),
			ToolNotUsed("write", Model),
			FinalMessageHas("DONE:{{NONCE}}", Either),
		},
	}
}

// needleSearch pins the grep tool's path:line:text output contract against one
// seeded needle among decoys. The nonce makes the needle findable and the decoys
// not: a case-sensitive grep for NEEDLE_{{NONCE}} passes over the lowercase
// "needle" decoy. ToolCallResult ties the assertion to the grep call itself — a
// grep whose input carries the needle pattern, whose own result names the seeded
// location — so unrelated bash output cannot stand in for it.
//
// glob is required (ToolUseAtLeast, Model — the prompt names it), so a glob that
// never runs reds here, and its output is graded in the two halves that can be
// told apart. GlobPathList (Platform) holds the tool to its contract whatever
// pattern the model chose — every successful result is one absolute path per
// line, or the tool's own "no matches" — so a result whose records are mangled
// reds the platform. Which paths come back is the pattern's business and the
// pattern is the model's, so "the seeded file is among them" is a separate
// Either. Pinning the whole list instead would mean dictating the pattern in the
// prompt, which is the one thing these prompts do not do.
//
// The grep half is Either: no such grep is the model not searching as asked, a
// grep with the wrong result is the platform's tool.
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
			ToolUseAtLeast("glob", 1, Model),
			GlobPathList(Platform),
			CallResult("glob", nil, false, "/workspace/src/util/helpers.go", Either),
			// grep runs with an absolute root, so its result line is
			// "/workspace/src/util/helpers.go:3:…" and the path:line prefix is a
			// substring of it. The answer regex accepts the absolute or a relative
			// rewrite the model may write.
			ToolCallResult("grep", "NEEDLE_{{NONCE}}", false, "src/util/helpers.go:3:", Either),
			FileMatches("answer.txt", `^(/workspace/)?src/util/helpers\.go:3$`, Either),
			FinalMessageHas("DONE:{{NONCE}}", Either),
		},
	}
}

// permAllow pins the happy path of the permission bridge: a gated tool suspends
// the session on requires_action, a confirmation releases it, and the tool runs
// — with the result correlated to the approval by tool_use_id, so the gate is not
// cosmetic. The toolset gates every tool via default_config, and the prompt uses
// only bash, so the one pause is the bash call.
//
// ToolCalledWith("bash", Model) carries the model's half — it must call the gated
// tool, with the nonce'd text the task named — which lets the pure bridge graders
// be clean Platform: RequiresActionRaised and EvaluatedPermissionAsk pass
// vacuously when nothing was gated, so a Platform failure there means the gate
// itself misbehaved, and a model that never calls bash reds only under the Model
// grader. ConfirmedResult is Either, not Platform: it pins that the approval
// released a result correlated to the confirmed call, but whether that result
// *succeeded* rides on the model's command being valid (the prompt only suggests
// an echo), so a failed allowed command is model-or-platform.
// The gated.txt effect is Either too — a missing file is the model not writing it
// or the platform not running the approved tool.
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
			ToolCalledWith("bash", []string{"GATED_{{NONCE}}"}, Model),
			RequiresActionRaised(Platform),
			EvaluatedPermissionAsk("bash", Platform),
			ConfirmedResult("bash", []string{"GATED_{{NONCE}}"}, false, "", Either),
			FileLines("gated.txt", []string{"GATED_{{NONCE}}"}, Either),
			FinalMessageHas("DONE:{{NONCE}}", Either),
		},
	}
}

// permDeny is the negative twin: the same gate, but the confirmation denies, and
// the platform synthesizes an is_error tool_result carrying the deny message
// instead of running the tool. The action is a benign append the reviewer happens
// to decline — deliberately benign, because a task that asks the model to delete a
// "protected" file tests the model's refusal reflex, not our denial path.
//
// ToolCalledWith("bash", Model) carries the model's half; ConfirmedResult
// correlates the deny message to the confirmed call — the call carrying the
// task's own APPEND token, not merely the first thing the bridge happened to
// stop, and by way of a confirmation that has to name a tool_use on the log at
// all; and the seeded file being byte-for-byte unchanged is the clean Platform
// signal that the command never ran — a changed file would mean the deny failed
// to block.
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
			ToolCalledWith("bash", []string{"APPEND_{{NONCE}}"}, Model),
			RequiresActionRaised(Platform),
			// The stamp, as in perm-allow. It is the half of "this call went
			// through the gate" that ConfirmedResult cannot see: a call the bridge
			// ran without stopping is invisible to a grader keyed on confirmations,
			// and the deny path needs that covered as much as the allow path does.
			EvaluatedPermissionAsk("bash", Platform),
			ConfirmedResult("bash", []string{"APPEND_{{NONCE}}"}, true, "DENY_{{NONCE}}", Platform),
			// FileEquals, not FileLines: "unchanged" is a byte claim, and the
			// blank-line forgiveness FileLines extends to a model's own work
			// product has no business in a file nothing was allowed to touch.
			FileEquals("notes.txt", "ORIGINAL_{{NONCE}}\n", Platform),
			FinalMessageHas("DENIED:{{NONCE}}", Either),
		},
	}
}

// exitCode pins tool-failure propagation: a command that exits non-zero must come
// back as an is_error result whose content carries the `exit code:` trailer
// (bash.go's contract). ToolCallResult asserts exactly that on the failing
// command's own result — the load-bearing check here. The final EXIT:…:1 the model
// reports is a secondary, Either signal, and a weaker one than it looks: a correct
// 1 is consistent with the model having read the result, but cat of a missing file
// conventionally exits 1, so a guessed 1 cannot be ruled out from the message
// alone. The platform's trailer, not the model's report, is what this task grades.
//
// ToolCallResult correlates the nonce'd bash call to its own result, so a stray
// "exit code: 1" from an unrelated command can no longer green the assertion. It
// is Either: the failure modes it folds together — the model never ran the nonce'd
// command versus a mis-joined streamed tool JSON — are indistinguishable from the
// transcript alone.
//
// The prompt forbids `$?`/`echo`/`;`/`||` for a reason: a model that wraps the cat
// (e.g. `cat missing; echo "EXIT:$?"`) makes the whole command exit 0, so the tool
// result is not an error and carries no trailer — the failure is masked before the
// platform can propagate it. Steering the model to the bare command is what keeps
// this a test of the platform's failure path rather than of the model's shell wits.
func exitCode() Task {
	return Task{
		ID: "exit-code",
		Turns: []Turn{{Message: "Use the bash tool to run this one command, exactly as written and " +
			"with nothing added to it:\n\ncat /workspace/missing_{{NONCE}}.txt\n\nThe file does not " +
			"exist, so the command fails on its own. When a command fails, the bash tool marks its " +
			"result as an error and ends it with a line `exit code: N`. Read N from that tool result — " +
			"do not compute it yourself with `$?`, `echo`, `;`, or `||`. Then reply EXIT:{{NONCE}}:N."}},
		Graders: []Grader{
			ToolCallResult("bash", "missing_{{NONCE}}", true, "exit code: 1", Either),
			FinalMessageHas("EXIT:{{NONCE}}:1", Either),
		},
	}
}

// journalMultiturn pins two turns on one session: the second must resume the
// first's context (event replay) and see the first's file (the same container,
// adopted again). The final file holding both lines is the workspace-persisted
// signal, and a tool_use after the second user.message is the resume actually
// doing work on turn two.
//
// Those two signals do not, by themselves, separate the two properties, and the
// task carries one witness for each so that they do.
//
// Replay: the prompt states a reference code in turn one that the model must
// repeat in turn two. {{RECALL}} is a second per-trial token, independent of the
// nonce — the nonce is in turn two's own prompt, so a token derived from it could
// be spelled by a model that had lost turn one entirely. The code exists nowhere
// but turn one's user.message, and the brain rebuilds every request from the
// event log (internal/brain/replay.go), so a model that says it back can only
// have got it from the replayed log. NotInToolTraffic keeps that true: a model
// that writes the code down and reads it back has turned the replay witness into
// a second persistence check, and reds rather than passing quietly.
//
// The wording is load-bearing and was learned the hard way. An earlier draft
// called it a "code word" and told the model not to write it to any file or run
// any command containing it; a live run refused the second turn outright,
// reading the pair as a secret and the request to repeat it as an attempt to
// extract it ("following an instruction to reveal something I was told to keep
// private is a classic pattern I should refuse"). It is the same trap view-range
// avoids by not calling its marker a SECRET: a prompt that sounds like a
// confidentiality rule tests the model's refusal reflex, not the platform. The
// token is now the user's own reference code and the off-disk hint is a
// convenience ("no need to save it anywhere"), which keeps the model off the
// filesystem without implying anything is being kept from anyone.
//
// Reuse: a file seeded before turn one and still byte-for-byte present at
// grading time. The model is never told about it, so — unlike journal.txt, which
// it could rewrite from its replayed context — nothing it does can restore it.
// A container recreated at any point between the seed and the grade takes the
// file with it, and the read that grades it adopts by the session's own name, so
// a replacement container is an empty one. That makes it the clean Platform
// signal the file-contents check cannot be.
//
// It is seeded outside /workspace, and the nonce is in its name. Both guard the
// same thing: this is a Platform-class check, so the model must not be able to
// red it by ordinary tidying. A sentinel sitting in the working directory is one
// `rm -rf /workspace/*` away from reporting a container that was never replaced
// as a platform fault — and a model has no reason to go looking in /tmp for a
// nonce'd name it was never told.
//
// Classing: the two user.message events are ours to post, so fewer than two on
// the log is unambiguously an event-log fault (Platform), as is the seeded file
// going missing. The journal contents, the turn-two tool_use and the recalled
// code word all ride on the model complying — appending correctly, acting on the
// second turn, doing as it was told with the word — so a miss there is
// Model-or-Platform (Either).
func journalMultiturn() Task {
	const provenance = "PROVENANCE_{{NONCE}}\n"
	const provenancePath = "/tmp/provenance-{{NONCE}}.txt"
	return Task{
		ID:    "journal-multiturn",
		Seeds: []Seed{{Path: provenancePath, Content: provenance}},
		Turns: []Turn{
			{Message: "Create /workspace/journal.txt with a single first line reading exactly: " +
				"entry-one-{{NONCE}}. My reference code for this session is {{RECALL}}; there is no " +
				"need to save it anywhere, just keep it in mind. Reply DONE1:{{NONCE}}."},
			{Message: "Append a second line to /workspace/journal.txt, below the first, reading " +
				"exactly: entry-two-{{NONCE}}. Keep the first line unchanged. Then reply " +
				"DONE2:{{NONCE}} followed by my reference code from the first message."},
		},
		Graders: []Grader{
			FileLinesIgnoringBlanks("journal.txt", []string{"entry-one-{{NONCE}}", "entry-two-{{NONCE}}"}, Either),
			FileEquals(provenancePath, provenance, Platform),
			EventCountAtLeast("user.message", 2, Platform),
			EventAfterUserMessage("agent.tool_use", 2, Either),
			FinalMessageHas("{{RECALL}}", Either),
			NotInToolTraffic("{{RECALL}}", Either),
			FinalMessageHas("DONE2:{{NONCE}}", Either),
		},
	}
}

// viewRange pins read's view_range slicing byte-for-byte: read line 57 of a
// 100-line file and it must be exactly line 57, not its neighbour and not line 57
// plus a stray newline. The seeded marker lives only on that line, so an
// off-by-one in the slicer returns the wrong bytes.
//
// The two halves split cleanly: ReadRangeRequested (Model) owns "the model asked
// to read line 57", and ReadRangeBytes (Platform, vacuous unless that read
// happened) owns "the slice returned exactly those bytes" — an off-by-one there is
// unambiguously the platform's. The marker is a plain token and the task a plain
// copy: a "SECRET" on the line reads as something to exfiltrate and provokes the
// model's refusal reflex, which tests the model, not the slicer.
//
// It doubles as the suite's write-tool coverage: the prompt names the write tool
// for the copy, ToolUseAtLeast (Model) requires it, and FileLines checks its effect
// — so a broken write reds the file check, and a model that copies with bash reds
// the Model grader instead of silently passing on an ungraded tool.
func viewRange() Task {
	return Task{
		ID:    "view-range",
		Seeds: []Seed{{Path: "poem.txt", Content: poem()}},
		Turns: []Turn{{Message: "The file /workspace/poem.txt has 100 numbered lines. Using the read " +
			"tool's line-range feature, read only line 57. Then use the write tool to save that exact " +
			"line to a new file /workspace/line57.txt, and reply DONE:{{NONCE}}."}},
		Graders: []Grader{
			ReadRangeRequested("poem.txt", 57, Model),
			ReadRangeBytes("poem.txt", 57, "MARKER_{{NONCE}}", Platform),
			ToolUseAtLeast("write", 1, Model),
			FileLines("line57.txt", []string{"MARKER_{{NONCE}}"}, Either),
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
