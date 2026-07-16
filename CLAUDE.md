# CLAUDE.md

Guidance for Claude Code (and contributors) working in this repository.

## What this is

An **open-source, self-hostable platform for long-horizon agents**, written in **Go**, Apache-2.0.
Goal: let enterprises run the whole thing **on-prem / in their own VPC** — data and compute never leave their boundary.

We take Anthropic's **Claude Managed Agents** as our **reference implementation**: we adopt its domain model and keep the public REST API **wire-compatible** with it, so the real `ant` CLI and Anthropic SDKs can drive our server unchanged. Referencing it is a deliberate compatibility and design choice, not an attempt to reproduce it — where our goals (self-hosting, pluggable model backends, OTel) call for something different, we diverge on purpose and say so.

Full design doc: `~/.claude/plans/agent-managed-agent-encapsulated-moonbeam.md` (approved plan; read it before large changes).

**Current state: [STATE.md](./STATE.md)** — a slim resumption file: snapshot, where everything lives, environment gotchas. Read it at the start of a session. It has a hard size budget: completed-work narrative goes to [docs/HISTORY.md](./docs/HISTORY.md) (same PR), the backlog lives in GitHub issues — never grow STATE.md with either.

## Core architecture — decouple brain / hands / session

An agent is three independently-swappable pieces (a pattern we take from the reference):

- **Session** = append-only **event log** in Postgres. The *single source of truth*. All durable state lives here.
- **Brain / Harness** = the loop that calls the model and routes tool calls. **Stateless, horizontally scalable.** Crash → any fresh brain replays the log and continues.
- **Sandbox / Executor ("hands")** = disposable per-session container that runs tools. "Cattle not pets": a container dying is one tool-call error, not a lost session.

Processes (each a `cmd/` binary): `controlplane` (REST + event log + queue + state machine) · `brain` (harness pool) · `executor` (built-in sandbox worker) · `worker` (distributable BYOC worker).

**Execution is fully async through the event log + work queue.** The brain never runs tools in-process — it emits `agent.tool_use`, an executor pulls the work, runs it in a sandbox, posts `agent.tool_result` (platform-managed) or `user.tool_result` (self-hosted/BYOC), and the brain wakes and continues. Platform-managed `cloud` and customer `self_hosted` are the **same pull protocol at two deployment points**.

## Non-negotiable design principles

1. **Anthropic's domain model is the single source of truth.** All internal types (`agent`/`environment`/`session`/`event`/`span`/`stop_reason`/`permission_policy`) are Anthropic-native and match the wire schema. Never bend them to a third-party library's shape.
2. **adk-go (`google.golang.org/adk/v2`) is a source of ideas, never a foundation.** (Distinct from Claude Managed Agents, which *is* our authoritative reference for the domain model.) adk-go is NOT a dependency of the domain layer. Where its abstractions conflict with the Anthropic model — its genai-centric `Event`/`session.Service`, its in-process `Runner`, `server/adkrest` — **do not use them**. Only borrow narrow, non-conflicting helpers, and only when they clearly save work. If a borrow ever conflicts, drop it and hand-roll.
3. **Observability is built in, not bolted on.** Every cross-process call propagates OTel context (W3C `traceparent`, including through work items to BYOC workers). Anthropic `span.*` events and OTel spans are emitted from the **same** instrumentation point so they never drift.
4. **Model providers are config-driven.** A provider is constructed from config: `protocol` (`anthropic`|`openai`) · `model` · `base_url` · `api_key` (+ optional headers). The Anthropic-protocol provider must work against **any** endpoint speaking Anthropic Messages (gateway, proxy, self-hosted model) — never hard-code `api.anthropic.com`. `model` string → provider is resolved via the `model_providers` config/table.
5. **Sessions are NOT bound to an end-user.** Scoping keys are `org`/`workspace`/`project` (reserved now, single-tenant defaults in v1). There is no `user_id` on a session (this is a deliberate divergence from adk's `AppName`+`UserID`). End-user ↔ session ownership is an **application-layer** concern; the platform stays user-agnostic. Apps use session `metadata` and the audit-only `created_by` as hooks.

Two standing product decisions travel with these principles: **v1's first-class scenario is a general task agent** (bash + file + web toolset — git/repo mounting is *not* a first-class v1 concern), and the project is **Apache-2.0, pure open source — no open-core edition gating**.

## Wire-compatibility rules

- Mirror Anthropic's resource model, paths, JSON fields, and **ID prefixes**: `agent_` `env_` `sesn_` (accept `session_`) `sevt_` `work_` `vlt_` `sesrsc_` `depl_` `drun_` `file_` `skill_`.
- Accept and ignore `anthropic-version` / `anthropic-beta` headers; honor `?beta=true` where the reference does.
- Auth: management via `x-api-key`; workers via environment key (`Authorization: Bearer`, scoped to one environment's work queue).
- Event taxonomy is `{domain}.{action}` — see the plan's Component 2 for the full list. SSE deltas use `content_delta` (NOT Messages API's `content_block_delta`).
- Never guess at a wire shape. Resolution order: public docs → the local reference checkouts (the Go SDK's typed schema first, then the `ant` CLI source; see [docs/REFERENCE_PROJECTS.md](./docs/REFERENCE_PROJECTS.md)) → recording a real `ant` CLI stream for behavior the types can't capture (ordering, SSE framing, defaults).
- Deliberate divergences from the reference, and inferences about reference behavior not yet confirmed, are recorded in **[docs/DIVERGENCES.md](./docs/DIVERGENCES.md)** — the single registry. The verifier's wire-compat rung and external reviewers resolve intentional mismatches against it; a divergence not in the registry is a finding.

## Reference source checkouts (local)

Four read-only local reference sources serve as ground truth and design reference. Their GitHub URLs, repo-relative local paths, per-project roles, authority order, and caveats live in **[docs/REFERENCE_PROJECTS.md](./docs/REFERENCE_PROJECTS.md)**. In short: `anthropic-sdk-go` is the typed wire schema, `anthropic-cli` is client-side behavior, `claude-code-source` is harness design reference only (a local source snapshot, not a git checkout — never a wire-schema source, never copy code from it), and `adk-go` is ideas only per design principle 2. In a new session, `/add-dir` them when needed.

## Repo layout

```
cmd/{controlplane,brain,executor,worker}   # the four binaries
internal/
  domain/     # Anthropic-native types — the source of truth; no adk/genai here
  api/        # wire-compatible REST handlers, ID prefixes, auth adapter
  events/     # append-only store, SSE (Postgres LISTEN/NOTIFY), delta reconciliation
  brain/      # orchestration loop, replay, provider request assembly
  provider/   # ModelProvider iface + anthropic/ + openai/ + registry (model→provider)
  mcp/        # MCP client wrapper (github.com/modelcontextprotocol/go-sdk)
  sandbox/    # SandboxProvider iface + docker/ + k8s/
  queue/      # work queue (Postgres FOR UPDATE SKIP LOCKED; redis optional later)
  policy/     # permission policy + HITL confirmation bridge
  telemetry/  # OTel/OTLP init; span ↔ span.* same-source instrumentation
  store/      # Postgres schema/migrations, reserved multi-tenant columns
deploy/{helm,compose}
```

Primary deps: `github.com/anthropics/anthropic-sdk-go`, `github.com/modelcontextprotocol/go-sdk`, `go.opentelemetry.io/otel` (+ OTLP), `github.com/jackc/pgx`. `google.golang.org/adk/v2` is **not** a hard dependency.

## Development

> Go 1.26 is installed (via Homebrew). Docker is available; `psql` is **not** — use the Postgres container.

The merge gate has one executable source — the root **`Makefile`**; prose and CI name its targets instead of duplicating commands:

```
make verify               # the whole gate: build + crossbuild + vet + fmt-check + test + cover-gate
make build crossbuild     # host build + linux/arm cross-compile of ./internal/... (worker portability)
make vet fmt-check        # lint
make test cover-gate      # go test -count=1 with the coverage profile, then the ≥90% gate
docker compose -f deploy/compose/docker-compose.yml up   # local: controlplane+brain+executor+Postgres(+Jaeger)
```

CI (`.github/workflows/ci.yml`) invokes the same targets, so the gate cannot drift between the docs, the verifier, and the merge check. The coverage gate is **total statement coverage ≥ 90%** over the **logic packages** under `./internal/...` — deliberately outside the denominator: `cmd/` main glue and the test-support packages (`internal/pgtest`, `internal/sandbox/sandboxtest`), whose only uncovered statements are assertion branches that fire when a suite fails. `make test` needs Docker (store/API/sandbox suites) and a Kubernetes cluster (the K8s sandbox contract test; a local kind cluster works) — a missing daemon or cluster is a hard failure, not a skip.

`.env` (gitignored) holds the model endpoint for real end-to-end integration verification: `MODEL_PROTOCOL` (`anthropic`|`openai`), `MODEL_BASE_URL`, `MODEL_API_KEY`, `MODEL_ID`. Nothing consumes it yet — the provider slice's integration tests will read these variables. Never commit real credentials.

Verify wire-compat end-to-end by pointing the real `ant` CLI at the local server with `--base-url http://localhost:PORT` and running `ant beta:agents/environments/sessions ...` and `ant beta:worker poll`. (Management commands ignore `ANTHROPIC_BASE_URL` — the CLI builds its client with `WithoutEnvironmentDefaults` and its global `--base-url` flag has no env source; only the worker/auth subcommands honor the env var.)

## Iteration workflow (branch → review → PR → CI → squash merge)

Every change lands through a PR; **never commit directly to `main`**.

1. Branch off a fresh `main`: `git checkout main && git pull && git checkout -b <type>/<short-name>` (e.g. `feat/telemetry`, `fix/event-seq`, `chore/ci`).
2. Develop on the branch (TDD as below). **Docs move with code, in the same PR:** STATE.md's snapshot updated (within its size budget) with the work's narrative appended to docs/HISTORY.md; a CHANGELOG.md entry for every notable change; a docs/DIVERGENCES.md entry for any new wire divergence or inference; README.md (status line, development notes) whenever the change alters what it describes — README's roadmap section deliberately defers to CHANGELOG.md and the issue tracker instead of tracking work itself. A doc that overclaims or lags the code is a defect, not a nice-to-have — the verifier checks docs consistency as a dedicated rung.
3. Run the **verifier subagent** (see "Independent verification"); fix findings before review.
4. **Dual code review**, one pass each: `/codex:review --background` (Codex reviewer) and `/code-review` (Claude reviewer). `/codex:review` is user-invocable only (`disable-model-invocation`); from a session, run the underlying reviewer as a background Bash task (backgrounding comes from the Bash task, not a flag), where `<plugin-root>` is the newest directory under `~/.claude/plugins/cache/openai-codex/codex/`. **Model and reasoning effort are not defaults you may accept — a weak reviewer finds nothing and its silence reads like a clean bill of health.** See "Reviewer settings" below; it governs both reviewers and the verifier. Branch scope reviews the **committed** diff against `main` — commit before launching it, or uncommitted fixes escape the review. Read the task's output file when it completes. Address findings from both reviewers; if a fix changes behavior, re-run the verifier. **Verify every finding against the source before acting on it** — both reviewers have produced confidently-argued findings that were false (see the `dec.More()` note in `internal/provider/config.go`); refute with evidence rather than "fixing" working code.
5. Push and open the PR (`gh pr create`); include the verifier verdict and both review outcomes in the description.
6. Wait for CI (`.github/workflows/ci.yml`) to be fully green: `gh pr checks --watch`. Red CI → fix on the branch; never merge red.
7. **Squash merge** (`gh pr merge --squash --delete-branch`), then sync local: `git checkout main && git pull`.

### Reviewer settings

**A reviewer running on the wrong model or too little reasoning effort finds nothing, and its silence is indistinguishable from a clean bill of health.** Choose both deliberately, for both reviewers. Evidence from slice 5: two low-effort Codex passes returned one finding between them, and it was a false positive; the same diff at `gpt-5.5`/`xhigh` returned five real defects, four of which were fixed pre-merge.

#### Claude side (verifier subagent, `/code-review`)

Subagents **inherit the main loop's model** unless told otherwise, so a session running a weaker or rate-limited model silently hands that model to its reviewers. Pin it deliberately:

- Verifier: its model is pinned in the definition file — `.claude/agents/verifier.md` sets `model: claude-fable-5`. Dispatch it with **no** `model` override (`Agent({subagent_type: "verifier", …})`) so it runs on that pinned model instead of inheriting the session's. Do **not** override to `model: "opus"`: that was a temporary workaround while fable-5's quota was exhausted, and it has been lifted.
- Review workflow: the `code-review` script's `agent()` calls omit `model`. When it matters, edit the persisted script (its path is returned by the `Workflow` tool) to pass `model: "opus"` in every `agent()` opts object, then re-invoke with `scriptPath`. Confirm afterwards by grepping the run's agent transcripts under `~/.claude/projects/<project>/<session>/subagents/workflows/<runId>/` for `"model":"claude-opus-4-8"`.

Re-invoking with `scriptPath` alone starts a fresh run; adding `resumeFromRunId` replays cached results from the **old** model, which defeats the point of re-running.

#### Codex side

The `review` subcommand of `codex-companion.mjs` passes `--model` but **never passes `--effort`**, so it silently inherits `model_reasoning_effort` from `~/.codex/config.toml` (currently `ultra`). That default is now strong, but it is the user's setting and can change back, and a low-effort review of a concurrency-heavy diff returns shallow findings and misses the real defects. Do not edit the user's `~/.codex/config.toml`. Run the review through the `task` subcommand instead (it sandboxes `read-only` when `--write` is omitted). Note that `ultra` is **not** a `--effort` value: the CLI's `--effort` accepts only `none`/`minimal`/`low`/`medium`/`high`/`xhigh` and rejects `--effort ultra` outright — `ultra` exists only as the config's `model_reasoning_effort`. So for the strongest review, **omit** `--effort` so the run inherits the config's `ultra`:

```
node "<plugin-root>/scripts/codex-companion.mjs" task --model gpt-5.6-sol \
  "<read-only review prompt: name the diff range and the invariants to attack>"
```

Omitting `--effort` inherits the config defaults (`gpt-5.6-sol` + `ultra`), the strongest review. When you need an effort that can't drift with the user's config, pin `--effort xhigh` — the strongest value the CLI accepts, one notch below `ultra` but guaranteed regardless of config. Do **not** pass `--effort ultra`; the CLI rejects it.

Model choice on this machine (`codex-cli 0.144.4`): **`gpt-5.6-sol` is the strongest usable model, and it is the config default.** It is verified real rather than a silent fallback — an invented name such as `gpt-5-9-totally-fake` is rejected with HTTP 400, while `gpt-5.6-sol` runs clean with no fallback-metadata warning (it returned a substantive `ultra` review and a clean trivial-task probe). Under the older `codex-cli 0.144.1` it was rejected as requiring a newer CLI than the then-published `@openai/codex`; the `0.144.4` upgrade lifted that. `gpt-5.5` still runs clean and is the fallback if `gpt-5.6-sol` ever regresses; `gpt-5.3-codex-spark` (the `spark` alias) works but is weaker. Re-check all three when the CLI is upgraded.

For a plain `review`-subcommand pass, pin the model explicitly. The old note that accepting the config default "fails outright" held only while the default `gpt-5.6-sol` was rejected by the older CLI; that rejection is gone, but pinning still guarantees the review's model can't silently follow a config change:

```
node "<plugin-root>/scripts/codex-companion.mjs" review "--scope branch --base main --model gpt-5.6-sol"
```

## How to work in this repo

These bias toward caution over speed. For trivial changes, use judgment.

**Think before coding.** State assumptions explicitly rather than picking silently. This codebase has a specific failure mode: *guessing at the wire schema*. When an exact JSON shape isn't in the public docs, do not invent it — read the reference checkouts (SDK types first), and record a real `ant` CLI stream when only behavior can answer. Likewise, if a requirement admits two readings (e.g. whether a field is session-local or agent-level), surface both and ask. If something is confusing, stop and name what's confusing. If a simpler approach exists, say so — push back when warranted.

**Assessment is a deliverable too.** When the user is describing a problem, asking a question, or thinking out loud rather than requesting a change, the deliverable is your assessment: report the findings and stop. Don't apply a fix until they ask for one. The same evidence discipline applies to actions: before running a command that changes system state (restarts, deletes, config edits — here typically: killing a server, dropping a test database, rewriting `.claude/` or workflow config), check that the evidence actually supports *that specific* action — a signal that pattern-matches a known failure may have a different cause.

**Pause only when the work genuinely requires the user.** That means: a destructive or irreversible action, a real scope change, or input only they can provide (credentials, account approvals, a product decision). Hitting one of these, ask and end the turn. Everything else — retries, missing information you can gather, long verification loops — is yours to push through; never end the turn on a promise of work not yet done.

**Simplicity first.** Write the minimum code that solves the problem; nothing speculative. The plan deliberately *reserves seams* for vaults, deployments, memory, multi-agent threads, and skills — reserving a seam means a column or an interface boundary, **not** an implementation. Do not build ahead of the current slice. No abstractions for single-use code, no configurability nobody asked for, no error handling for impossible states. If 200 lines could be 50, rewrite it. The test: would a senior engineer call this overcomplicated?

**Surgical changes.** Every changed line should trace directly to the request. Don't "improve" adjacent code, comments, or formatting; match the existing style even where you'd choose differently. Clean up orphans *your* change created (now-unused imports, functions); leave pre-existing dead code alone — mention it instead of deleting it.

**Goal-driven execution.** Turn each task into a verifiable goal before writing code, and state the plan as steps with their checks:

```
1. [Step] → verify: [check]
```

Concretely here: "add validation" → write the failing test for invalid input first; "fix the bug" → write the reproducing test first; "refactor X" → tests green before and after. Strong success criteria let you loop to done without check-ins; weak ones ("make it work") don't.

**Independent verification (definition of done).** The implementer never certifies their own work. Before any nontrivial change is declared done — before STATE.md's snapshot claims new behavior, before a commit that claims working behavior — dispatch the **`verifier` subagent** (`.claude/agents/verifier.md`) with what changed and the success criteria. It re-derives expectations from the docs, reruns every check itself (`go test -count=1`, no cached results), exercises runtime behavior where a surface exists, diffs wire-compat claims field-by-field against the reference checkouts, and checks that the project docs (STATE.md, README.md, CHANGELOG.md, docs/HISTORY.md) correctly describe the change. A FAIL or unresolved blocker finding means the work is **not done**: fix, then re-verify. Pin its model (see "Reviewer settings") — an unpinned verifier inherits the session's, and a weak verifier certifies anything. The verifier's verdict and evidence belong in the final report to the user.

**Report only what you can evidence.** Before reporting progress, audit each claim against a tool result from this session — a test run, a diff, a CLI transcript. Only report work you can point to evidence for; if something is not yet verified, say so explicitly. Report outcomes faithfully: if tests fail, say so with the output; if a step was skipped, say that; when something is done and verified, state it plainly without hedging.

**Write the final report for a reader who wasn't there.** Terse shorthand between tool calls is fine — that's thinking out loud, and brevity there is good. The final summary is different: after a long stretch of unattended work, it is the user's first look at any of it, so write it as a re-grounding, not a continuation of the working thread. Open with the outcome — one sentence on what happened or what was found — then the supporting detail, then the one or two things needed from the user, each explained as if new. Drop the working vocabulary: complete sentences, terms spelled out, no arrow chains, no hyphen-stacked compounds, no labels invented mid-session unless re-introduced. When mentioning files, commits, or flags, give each its own plain-language clause. If short and clear conflict, choose clear.

**Testing conventions.**
- **TDD** for anything with behavior: contract test first, then implement. This matters most for provider adapters, event/JSON round-trips against the wire schema, sandbox providers, and the work-queue lease state machine.
- Keep files focused and small; one clear responsibility per package.
- Provider-, sandbox-, and queue-backend variability lives behind interfaces with a **shared contract test suite** — every new backend must pass the same suite.
- Confine lossy conversions to a single package (`provider/openai`) and test them hard; the Anthropic-protocol provider should be near-zero-conversion.
