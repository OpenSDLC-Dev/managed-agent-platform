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

`.claude/agents/verifier.md`'s frontmatter pins the verifier's model and effort; this skill
does not restate them. Dispatch it with **no** `model` or `effort` override, so the pins win:

- From the main session: `Agent({subagent_type: "verifier", …})`. A `model` or `effort`
  passed per invocation outranks the frontmatter.
- From a Workflow script: `agent(prompt, {agentType: "verifier", schema})`. A general agent
  told to read verifier.md gets every tool and neither pin.
- Three environment variables outrank the pins (Claude Code 2.1.296):
  - `CLAUDE_CODE_EFFORT_LEVEL` outranks every `effort`.
  - `CLAUDE_CODE_SUBAGENT_MODEL_FORCE=1` ignores the frontmatter `model` and every `model`
    passed per invocation.
  - `CLAUDE_CODE_WORKFLOW_SUBAGENT_MODEL` replaces every model in a Workflow run.

  Leave them unset, or name them in the PR's verification section.
- Settings can still lower the pins without a word in a background agent: a
  `maxEffortLevel` or an organization effort cap clamps `effort`, and an `availableModels`
  allowlist substitutes a blocked model. So read the model and the effort from the agent's
  transcript, which records both on every turn, and name them in the PR.

Do not override to opus by default; that was a temporary quota workaround, lifted
2026-07-15. If the pinned model's quota runs out, re-dispatch with `model: "opus"`, tell the
verifier it runs by override and must hold the same bar, and disclose the override in the
PR. The Agent tool takes only the alias, which can resolve to the session's own model, so
read the exact model id from the agent's transcript before you name it. Never skip
verification.

## /code-review (Claude side)

Run it on the current Opus, `claude-opus-5-5` (user decision, 2026-07-25: a reviewer weaker
than the implementer finds nothing, and its silence reads as a clean bill of health), at
level `xhigh` with no finding cap (user decision, 2026-10-11).

1. Name the level and the cap first, then the scope:
   `Skill({skill: "code-review", args: "xhigh --max-findings all <scope>"})`. Both choices
   are sticky: without a level the command reuses the level typed last, so an unpinned pass
   drifts the way Codex's config did. Three recorded `/code-review` passes in
   docs/HISTORY.md stopped at exactly fifteen findings; the verify rule above is the filter,
   not a cap.
2. The Skill route runs as one background fork on the session's model. If the session runs
   another model (another family, or an older Opus after a flagged message switched it),
   take step 3's route. A review on the weaker model is not this pass.
3. If the Skill route is refused or the session runs another model, launch
   `Workflow({name: "code-review", args: …})` with the same arguments. Its `agent()` calls
   inherit the session's model when their opts name none, so on another model add
   `model: "claude-opus-5-5"` to every opts object in the persisted script. Use the full id,
   because the `opus` alias follows the session's own Opus. Re-invoke with `{scriptPath}`
   only. Never add `resumeFromRunId`: it replays cached results from the old model.

   That workflow is not registered in every session: the route worked on 2026-08-08 and
   answered `not found. Available: deep-research` on 2026-08-22. If it is missing and the
   session runs another model, ask the user to run `/model claude-opus-5-5`. Only the user
   can switch the session's model.
4. Confirm the model before citing it. A fork's transcript
   (`~/.claude/projects/<project>/<session>/subagents/agent-<id>.jsonl`) records `"model"`
   on every turn; a Workflow run's transcripts sit under `subagents/workflows/<runId>/`.
   In the PR description, name what ran: `/code-review (<model id>, xhigh)`.

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
  and never emit a verdict. Watch the log for the `Turn completed` marker. Elapsed time
  alone never makes a stall: an `xhigh` pass can run 20 to 30 minutes and return its best
  findings late, so budget about 25 minutes. Do not suspect a stall before about 12 minutes;
  after that, a stall is a run none of whose rollout logs has grown between two checks a
  few minutes apart. A run's sub-sessions write logs of their own, and the parent's log
  stops while it waits on them. To find the logs, touch a marker file in your scratch
  directory just before the launch, then run
  `find ~/.codex/sessions -name 'rollout-*.jsonl' -newer <marker> | xargs grep -lF '"cwd":"<worktree-path>"'`.
  The marker leaves out earlier runs, and the anchored `cwd` leaves out another worktree
  whose path shares a prefix. Remove the marker when the run ends. Do not let a stalled pass
  block a PR the verifier and the Claude review already covered — note the stall in the PR
  description instead.
