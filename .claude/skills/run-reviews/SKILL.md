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

Run its agents on **Opus 5** (user decision, 2026-07-25, superseding the Opus 4.8 pin of
2026-07-16), not the session model. Subagents inherit the main loop's model unless told
otherwise, and the code-review workflow's `agent()` calls omit `model`, so:

1. Launch `/code-review`; the Workflow tool result names the persisted script path.
2. Edit that script, adding `model: "opus"` to **every** `agent()` opts object — the alias
   resolves to the current Opus generation.
3. Re-invoke with `{scriptPath}` only — a fresh run. Never add `resumeFromRunId`: it
   replays cached results from the old model, which defeats the re-run.
4. Confirm from the run's agent metadata (or the transcripts under
   `~/.claude/projects/<project>/<session>/subagents/workflows/<runId>/`) that agents ran
   on the current Opus (`claude-opus-5-5` as of 2026-10-04). The alias is what makes step
   2 short, and the only thing that can go wrong with it: if it resolved to an older Opus,
   put the exact model id in the `model` field instead and re-run.

## Codex reviewer

Preferred invocation — the `task` subcommand (it sandboxes read-only when `--write` is
omitted), with the model and the effort both pinned (user decision, 2026-10-04):

```
node "<plugin-root>/scripts/codex-companion.mjs" task --model gpt-6.1-sol --effort xhigh \
  "<read-only review prompt: name the diff range and the invariants to attack>"
```

`<plugin-root>` is the newest directory under `~/.claude/plugins/cache/openai-codex/codex/`.
Run it as a background Bash task (backgrounding comes from the Bash task, not a flag) and
read the task's output log for the verdict when it completes; `/codex:review` itself is
user-invocable only (`disable-model-invocation`).

- **Effort:** always pass `--effort xhigh`. Omitting it inherits `model_reasoning_effort`
  from `~/.codex/config.toml`, which drifts with the user's own use of Codex: it read
  `ultra` when this skill was written and `low` on 2026-10-04, so a pass that inherits it
  runs at whatever the config says that day. The companion's flag
  accepts only `none`/`minimal`/`low`/`medium`/`high`/`xhigh`; the models also list `max`
  and `ultra`, reachable only through the config. Never edit `~/.codex/config.toml`.
- **Model:** `gpt-6.1-sol` ("Latest workhorse model for coding and everyday work") on
  `codex-cli 0.160.0`, verified 2026-10-04 to run clean at `xhigh` with no
  fallback-metadata warning. The server refuses a model the account cannot use rather than
  substituting one: on `codex-cli 0.155.1` the same name failed with HTTP 400 `The
  'gpt-6.1-sol' model is not supported when using Codex with a ChatGPT account`, so a
  pass that errors is not a clean verdict. Fallbacks, both verified at `xhigh` the same
  day: `gpt-6-astra` ("Frontier intelligence for the most demanding work"), then
  `gpt-5.6-sol`. The CLI's model list, with each model's description and effort levels,
  is `~/.codex/models_cache.json`; re-check the pin against it when the CLI is upgraded.
- **Plain `review` subcommand:** it passes `--model` but never `--effort`, so its effort
  silently follows the config — use `task` instead. If `review` must be used, pin the
  model at least (`review "--scope branch --base main --model gpt-6.1-sol"`) and say in
  the PR that its effort was the config's.
- **Stall mode:** the `task` subcommand can hang on its internal "wait" collaboration tool
  and never emit a verdict. Watch the log for the `Turn completed` marker, cap the wait at
  ~12 minutes, and do not let a stalled Codex pass block a PR the verifier and the Claude
  review already covered — note the stall in the PR description instead.
