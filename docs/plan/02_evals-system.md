---
status: archived
issue: "#30"
---

# Eval test system

The plan for [#30](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/30)'s
phase 1: an end-to-end **eval suite** that drives the whole stack against a real model
endpoint, plus the ten regression tasks it runs. Phase 1 is delivered — all ten tasks run
10/10 green live via `make eval`; the delivery narrative lives in
[HISTORY.md](../HISTORY.md) and [CHANGELOG.md](../../CHANGELOG.md), and the leftovers are
issues (#96, #99, and phase 1.5 on #30).

## Why

Issue #30 audited what the live-model coverage actually proves. One test calls a real
endpoint (`TestIntegrationRealEndpoint`): one text-only turn, no tools, no brain, no
queue, no executor, no API. Every loop test scripts its provider; every sandbox test
fakes its model. **Nothing exercises the product path** — public REST API → brain → real
model → work queue → executor → Docker sandbox → SSE → idle. The one full-loop run this
project ever made was the manual slice-8 acceptance, run by hand once.

So a gateway upgrade, an SDK bump, or a model snapshot can break tool calling while CI
stays green at 90% coverage: the scripted SSE frames those tests assert against are
authored in this repository, and they keep agreeing with themselves. The same audit found
a second defect: a populated `.env` alone made an ordinary `go test ./...` spend money.

## Shape

Two ideas, borrowed and cited:

- **The [quickstart](https://platform.claude.com/docs/en/managed-agents/quickstart) flow
  is the harness's spine.** Create agent (`agent_toolset_20260401`) → environment →
  session → POST `user.message` → stream SSE → `session.status_idle`. The reference's own
  Fibonacci task is eval task 1, so our first case is one the reference implementation
  documents as working.
- **The vocabulary and the discipline come from
  [Demystifying evals for AI agents](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents)**:
  task / trial / grader / transcript / outcome / harness / suite. These are **regression
  evals**, not a capability benchmark — they target ~100% pass for any competent model and
  exist to catch backsliding. Hence: deterministic code-based graders only (no LLM judge),
  structural assertions and per-trial nonces instead of prose matching, a clean
  environment per trial, and balanced positive/negative cases.

### Architecture

- **In-process stack, production code paths.** `pgtest` Postgres → `httptest.NewServer(api.NewHandler(pool))`
  → real `brain.Run` with a real `provider.NewRegistry` → real `executor.Run` with a real
  `docker` sandbox provider. Only `main()` glue (env parsing, TCP listen) is bypassed, and
  CI's `compose` job already smokes that. Hermetic per run, deterministic teardown, one log
  stream, no image build in CI.
- **Test-only top-level `evals/`.** `go test` already gives subtests, timeouts, and
  panic-safe `t.Cleanup`; a runner binary would duplicate it. Top-level keeps it out of
  the coverage denominator (which is `./internal/...`), exactly as `cmd/` is — but
  `go test ./...` still compiles and runs it, so the live tier must gate itself.
- **Grading surfaces**: `[transcript]` the event log via `GET /v1/sessions/{id}/events`;
  `[fs]` the sandbox filesystem (containers persist past idle, and `docker.Provider.Provision`
  is idempotent-adopt, so the harness reads files back through the same production code the
  executor used); `[status]` `GET /v1/sessions/{id}`. There is no Files API — that is a
  deferred seam (#55), and the suite must not assume one.
- **Failure classing** decides retries: **P** platform bug (never retried — it is the
  signal), **M** model non-compliance (one retry, reported), **E** either. Default one
  trial per task.

### Opt-in contract (#30's acceptance)

| Variable | Tier | Cost |
|---|---|---|
| *(none)* | unit, contract, dependency integration | free, every PR |
| `RUN_LIVE_MODEL_TESTS=1` | provider live-contract: one real turn through the adapter whose protocol the endpoint speaks | cents |
| `RUN_EVALS=1` | live-system evals: whole sessions, real sandboxes | minutes, dollars |

Unset → skip (an ordinary `go test ./...` calls no model, even with a populated `.env`).
Set but misconfigured → **fail, never skip**: a safety net that skips itself when its
credentials rot is not a safety net. `.env` supplies configuration; the tier variable
supplies consent. `internal/modeltest` owns this contract.

## Delivery slicing

Phase 1 lands as five PRs, each green on its own (docs move with the code, per CLAUDE.md):
the live-test tier + `internal/modeltest` (the opt-in contract, removing the `.env`
auto-opt-in defect); OTel traces and metrics on the execution chain; the OTel log bridge
(split out of the metrics PR); the harness + tasks 1–3 + `make eval`; and tasks 4–10 with
the wrap-up. The delivery record — what each PR actually shipped and found — lives in
[HISTORY.md](../HISTORY.md) and [CHANGELOG.md](../../CHANGELOG.md), not here.

## The ten tasks

Every prompt carries a per-trial `{{NONCE}}`; every "final message" check asserts a
prompt-demanded marker (`DONE:{{NONCE}}`), never incidental prose. **G0**, the core pack,
runs on all of them: reaches `idle` with `stop_reason.type == "end_turn"`; no
`session.error`; every `agent.tool_use` joined by exactly one `agent.tool_result`; usage
accounting populated; the idle event observed on the SSE stream.

| # | ID | What it pins down | Seeds |
|---|----|-------------------|-------|
| 1 | `fib-quickstart` | The reference's own task: write a script, run it, verify. `fibonacci.txt` must hold exactly 0…4181 | — |
| 2 | `echo-notool` | Text-only baseline. Negative: no `tool_exec` ⇒ **no container may exist** | — |
| 3 | `shell-state` | The persistent shell: `export` in call 1 must survive into call 2 | — |
| 4 | `edit-config` | `read` + `edit`. Whole-file equality proves the edit was surgical, not a rewrite | `config.ini` |
| 5 | `needle-search` | `grep`'s `path:line:text` output contract against a seeded needle among decoys; `glob` is invoked (its output not pinned) | 4 files |
| 6 | `perm-allow` | The permission bridge: `requires_action` → confirm → resume, with the tool result correlated to the approval by `tool_use_id` | — |
| 7 | `perm-deny` | Its negative twin: a denied append synthesizes an `is_error` result, and the seeded file is left untouched | `notes.txt` |
| 8 | `exit-code` | Tool failure propagation: a failed command's `exit code:` trailer, correlated to the failing call's own result | — |
| 9 | `journal-multiturn` | Two turns, one session: event replay and sandbox reuse (the executor adopts the session's container) | — |
| 10 | `view-range` | `read` `view_range` slicing, byte-exact — an off-by-one guard | `poem.txt` |

Coverage: all six tools are exercised across the tasks, at two strengths. `edit` (4), `grep`
(5) and `bash` (8, the failing command) are pinned by a result contract that ties a call to its
own output, and `read` (10) byte-exact by the view-range slice grader. `bash` (3/6/7), `read`
(4), `glob` (5) and `write` (10) ride on a required tool-use floor (the prompts name the tool);
`write`'s effect is further pinned by its written artifact (line57.txt), while `glob` is
invocation-only — its result is joined by the core pack, but a bare path list has no stable
order to pin, so a broken glob that still returns something is not caught here. Single and multi
turn; allow and deny; seeded and unseeded; three negatives (2, 7, 8). Every trial exercises SSE
and usage accounting through G0.

One image for the whole run, `python:3.12-slim` (Debian-slim underneath, so the toolset's
`grep`/`stat`/`sort` probes see the same userland as the default `debian:stable-slim`).
Only task 1 needs Python.

## Deliberately not in phase 1

- **`tool_choice` / `disable_parallel_tool_use`** — #30's case 1, tracked there. The ten
  tasks steer with prompts and grade structurally, so they do not need it; forcing a strict
  single tool call is the right way to prove the *provider* contract, and it changes
  `provider.Request` plus both adapters. Phase 1.5, alongside #48's shared provider contract
  suite.
- **Production sandbox reaping** — already filed as
  [#64](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/64):
  `Sandbox.Destroy` has no production call site, so a session leaks its container. The
  harness reaps its own and does not wait for #64.
- **A daily scheduled CI run.** Phase 1 is `make eval` on a developer's machine. The
  scheduled workflow needs repo `MODEL_*` secrets and a hard-fail guard so the net cannot
  vanish silently — its own PR once someone configures the secrets. **PR 4 files the issue.**
- **Black-box mode against the compose stack.** Mostly redundant with CI's compose job; no
  issue filed, and none intended unless the in-process stack proves to be hiding something.
