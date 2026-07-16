---
name: run-reviews
description: Run this repo's dual code review (Codex + Claude) and the verifier with deliberately chosen models and effort — pinned, or intentionally inherited from config. Use when executing step 4 of CLAUDE.md's iteration workflow, or whenever launching the verifier, /code-review, or the Codex reviewer.
---

# Running the reviewers — pinned models, deliberate effort

**A reviewer running on the wrong model or too little reasoning effort finds nothing, and
its silence is indistinguishable from a clean bill of health.** Evidence from slice 5: two
low-effort Codex passes returned one finding between them — a false positive — while the
same diff at `gpt-5.5`/`xhigh` returned five real defects, four fixed pre-merge.

Two ground rules for every pass:

- Branch scope reviews the **committed** diff against `main` — commit before launching, or
  uncommitted fixes escape the review.
- **Verify every finding against the source before acting on it.** Both reviewers have
  produced confidently-argued findings that were false (see the `dec.More()` note in
  `internal/provider/config.go`); refute with evidence rather than "fixing" working code.

## Verifier (Claude side)

The model is pinned in `.claude/agents/verifier.md` (`model: claude-fable-5`). Dispatch
with **no** `model` override (`Agent({subagent_type: "verifier", …})`) so the pin wins. Do
not override to opus — that was a temporary quota workaround, lifted 2026-07-15.

## /code-review (Claude side)

Run its agents on **Opus 4.8** (user decision, 2026-07-16), not the session model.
Subagents inherit the main loop's model unless told otherwise, and the code-review
workflow's `agent()` calls omit `model`, so:

1. Launch `/code-review`; the Workflow tool result names the persisted script path.
2. Edit that script, adding `model: "opus"` to **every** `agent()` opts object.
3. Re-invoke with `{scriptPath}` only — a fresh run. Never add `resumeFromRunId`: it
   replays cached results from the old model, which defeats the re-run.
4. Confirm from the run's agent metadata (or the transcripts under
   `~/.claude/projects/<project>/<session>/subagents/workflows/<runId>/`) that agents ran
   on `claude-opus-4-8`.

## Codex reviewer

Preferred invocation — the `task` subcommand (it sandboxes read-only when `--write` is
omitted), inheriting the strongest effort from the user's config:

```
node "<plugin-root>/scripts/codex-companion.mjs" task --model gpt-5.6-sol \
  "<read-only review prompt: name the diff range and the invariants to attack>"
```

`<plugin-root>` is the newest directory under `~/.claude/plugins/cache/openai-codex/codex/`.
Run it as a background Bash task (backgrounding comes from the Bash task, not a flag) and
read the task's output log for the verdict when it completes; `/codex:review` itself is
user-invocable only (`disable-model-invocation`).

- **Effort:** omitting `--effort` inherits `model_reasoning_effort` from
  `~/.codex/config.toml` (currently `ultra`, the strongest — it exists only as a config
  value; `--effort ultra` is rejected — the flag accepts only
  `none`/`minimal`/`low`/`medium`/`high`/`xhigh`). Pin
  `--effort xhigh` when the effort must not drift with the user's config. Never edit
  `~/.codex/config.toml`.
- **Model:** `gpt-5.6-sol` is the strongest usable model on `codex-cli 0.144.4` and the
  config default — verified real, not a silent fallback (an invented name is rejected with
  HTTP 400; this one runs clean with no fallback-metadata warning). `gpt-5.5` is the
  fallback if it regresses; `gpt-5.3-codex-spark` (`spark`) works but is weaker. Re-check
  all three when the CLI is upgraded.
- **Plain `review` subcommand:** pin the model explicitly
  (`review "--scope branch --base main --model gpt-5.6-sol"`); it passes `--model` but
  never `--effort`, so effort silently follows the config.
- **Stall mode:** the `task` subcommand can hang on its internal "wait" collaboration tool
  and never emit a verdict. Watch the log for the `Turn completed` marker, cap the wait at
  ~12 minutes, and do not let a stalled Codex pass block a PR the verifier and the Claude
  review already covered — note the stall in the PR description instead.
