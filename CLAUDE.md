# CLAUDE.md

Guidance for Claude Code (and contributors) working in this repository. [AGENTS.md](./AGENTS.md) is its distilled mirror for external AI coding agents and reviewers.

## What this is

An **open-source, self-hostable platform for long-horizon agents**, written in **Go**, Apache-2.0.
Goal: let enterprises run the whole thing **on-prem / in their own VPC** — data and compute never leave their boundary.

We take Anthropic's **Claude Managed Agents** as our **reference implementation**: we adopt its domain model and keep the public REST API **wire-compatible** with it, so the real `ant` CLI and Anthropic SDKs can drive our server unchanged. Referencing it is a deliberate compatibility and design choice, not an attempt to reproduce it — where our goals (self-hosting, pluggable model backends, OTel) call for something different, we diverge on purpose and say so.

## Plans, state, and backlog

- **Plans live in [docs/plan/](./docs/plan/)** — one file per plan, named `NN_short-name.md` (two-digit sequence, ascending, assigned when the file enters the repo). Each opens with YAML frontmatter: `status: draft | approved | in-progress | archived`, plus an optional `issue:` naming the tracking issue. Lifecycle: `draft` = authored in-repo for discussion; `approved` = accepted but not started — a plan approved in plan mode is copied here in the first PR that touches it, and the repo copy is canonical from then on; `in-progress` = flipped in the PR that starts development (a plan whose first landing PR already starts the work lands as `in-progress` directly, skipping a committed `approved` state); `archived` = completed or superseded (the file says which), flipped in the final PR.
- **Plan files carry no progress tracking** — no checklists ticked as work lands, whatever the status. While a plan is active its progress lives in STATE.md; once it archives, the delivery record is docs/HISTORY.md and CHANGELOG.md.
- **Current state: [STATE.md](./STATE.md)** — the active-work tracker, and nothing else: the current plan or issue (Active work — **none** when nothing is in flight) and its task checklist with progress and evidence links (Tasks), within a ~30-line budget. Read it at the start of a session; update it in every PR that starts, advances, or finishes the work it tracks — a plan's PRs and plan-less issue-driven work alike. It holds no snapshot, no doc index, no environment notes — a change's narrative is written **once**, in [CHANGELOG.md](./CHANGELOG.md); [docs/HISTORY.md](./docs/HISTORY.md) receives only what a changelog structurally cannot hold (acceptance-run and review-hardening records, decisions evaluated and rejected, archived plans' progress summaries — an archiving plan's summary moves there in the archiving PR); the as-built system is [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md); the backlog lives in GitHub issues. Never grow STATE.md with any of them.
- **The backlog is GitHub issues** — the only backlog. Neither plan files nor STATE.md accumulate future work. Post-v1 deferrals are #50–#57 (+ #77) — do not build ahead of them; wire assumptions awaiting a real managed-agents recording are cross-linked from [docs/DIVERGENCES.md](./docs/DIVERGENCES.md)'s INFERRED section.
- **Starting work from a GitHub issue?** Dispatch the **`issue-triage` subagent** (`.claude/agents/issue-triage.md`) first — read-only, pinned to Sonnet 5. It reads the issue and surveys the affected code, then returns one strict-JSON judgment: `needs_plan` true (multi-PR scope, an architectural decision, ambiguity needing the user, or wire-schema verification → author a docs/plan/ file before implementing) or false (single-PR work, with suggested `direct_tasks`). Its output is advisory input only — drafting the plan, or turning the tasks into STATE.md's Tasks, stays with the main agent. It is not dispatched for work that already has a plan, or for conversation-driven requests that never touch an issue.

The v1 design plan is [docs/plan/01_v1-managed-agent-platform.md](./docs/plan/01_v1-managed-agent-platform.md) (archived; mostly implemented) — consult it for rationale before large architectural changes.

## Core architecture — decouple brain / hands / session

An agent is three independently-swappable pieces (a pattern we take from the reference): the **session** — an append-only event log in Postgres, the single source of truth; the **brain/harness** — the stateless, horizontally-scalable loop that calls the model and routes tool calls (a crashed brain loses nothing: any fresh brain replays the log and continues); and the **sandbox ("hands")** — a disposable per-session container that runs tools ("cattle not pets": a dying container is one tool-call error, not a lost session). Four `cmd/` binaries: `controlplane` · `brain` · `executor` · `worker`.

**Execution is fully async through the event log + work queue.** The brain never runs tools in-process — it emits `agent.tool_use`, an executor pulls the work, runs it in a sandbox, posts the result event, and the brain wakes and continues. Platform-managed `cloud` and customer BYOC `self_hosted` are the **same pull protocol at two deployment points**.

The as-built depth — process topology, the full execution flow (permissions/HITL, crash recovery), the wire-compatibility model, a per-package reference, security invariants, observability, testing architecture — is **[docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md)**.

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
- Event taxonomy is `{domain}.{action}` — see [docs/plan/01_v1-managed-agent-platform.md](./docs/plan/01_v1-managed-agent-platform.md)'s Component 2 for the full list. SSE deltas use `content_delta` (NOT Messages API's `content_block_delta`).
- Never guess at a wire shape. Resolution order: public docs → the local reference checkouts (the Go SDK's typed schema first, then the `ant` CLI source; see [docs/REFERENCE_PROJECTS.md](./docs/REFERENCE_PROJECTS.md)) → recording a real `ant` CLI stream for behavior the types can't capture (ordering, SSE framing, defaults).
- Deliberate divergences from the reference, and inferences about reference behavior not yet confirmed, are recorded in **[docs/DIVERGENCES.md](./docs/DIVERGENCES.md)** — the single registry. The verifier's wire-compat rung and external reviewers resolve intentional mismatches against it; a divergence not in the registry is a finding.

## Reference source checkouts (local)

Four read-only local reference sources serve as ground truth and design reference. Their GitHub URLs, repo-relative local paths, per-project roles, authority order, and caveats live in **[docs/REFERENCE_PROJECTS.md](./docs/REFERENCE_PROJECTS.md)**. In short: `anthropic-sdk-go` is the typed wire schema, `anthropic-cli` is client-side behavior, `claude-code-source` is harness design reference only (a local source snapshot, not a git checkout — never a wire-schema source, never copy code from it), and `adk-go` is ideas only per design principle 2. In a new session, `/add-dir` them when needed.

## Repo layout

```
cmd/{controlplane,brain,executor,worker}   # the four binaries
internal/
  domain/     # Anthropic-native types — the source of truth; no adk/genai here
  api/        # wire-compatible REST handlers, ID prefixes, auth, work API
  events/     # append-only store, SSE (Postgres LISTEN/NOTIFY), delta reconciliation
  brain/      # orchestration loop, replay, provider request assembly
  provider/   # ModelProvider iface + anthropic/ + openai/ + registry (model→provider)
  executor/   # tool_exec consumer — the platform-managed half of the pull protocol
  worker/     # BYOC worker — the customer-hosted twin of executor/, wire-only
  toolset/    # the built-in tools (agent_toolset_20260401)
  sandbox/    # Sandbox/Provider iface + docker/ + k8s/ + backend selection + shell/
  blob/       # object-storage seam: Store iface + s3/ (S3-compatible via minio-go)
  skills/     # skill-upload validation + canonical-zip normalization (SKILL.md frontmatter)
  queue/      # work queue (Postgres FOR UPDATE SKIP LOCKED; redis optional later)
  telemetry/  # OTel/OTLP init; span ↔ span.* same-source instrumentation
  store/      # Postgres schema/migrations, reserved multi-tenant columns
deploy/{helm,compose}
```

(Plus test-support: `internal/{pgtest,modeltest}`, `internal/sandbox/sandboxtest`, `internal/blob/blobtest`, `internal/provider/providertest`, and the top-level `evals/` live suite. There is no `internal/mcp` or `internal/policy`: no MCP client is built yet, and permission policy lives across `domain`/`toolset`/`brain`/`api`.) What each package's files actually do: [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md) → "Package reference".

Primary deps: `github.com/anthropics/anthropic-sdk-go`, `go.opentelemetry.io/otel` (+ OTLP), `github.com/jackc/pgx`, `github.com/minio/minio-go` (S3-compatible blob storage). Neither `github.com/modelcontextprotocol/go-sdk` (no MCP client yet) nor `google.golang.org/adk/v2` is a dependency.

## Development

> Go 1.26 is installed (via Homebrew). Docker is available; `psql` is **not** — use the Postgres container.

The Go merge gate has one executable source — the root **`Makefile`**; prose and CI name its targets instead of duplicating commands (CI additionally runs its `helm` and `compose` jobs — chart lint/render and a compose smoke test — which stay in ci.yml, and a PR needs the whole workflow green):

```
make verify               # the whole Go gate: build + crossbuild + vet + fmt-check + test + cover-gate
make build crossbuild     # host build + linux/arm cross-compile of ./internal/... (worker portability)
make vet fmt-check        # lint
make test cover-gate      # go test -count=1 with the coverage profile, then the ≥90% gate
docker compose -f deploy/compose/docker-compose.yml up   # local: controlplane+brain+executor+Postgres+MinIO(+Jaeger)
```

CI (`.github/workflows/ci.yml`) invokes the same targets, so the gate cannot drift between the docs, the verifier, and the merge check. The coverage gate is **total statement coverage ≥ 90%** over the **logic packages** under `./internal/...` — deliberately outside the denominator: `cmd/` main glue and the test-support packages (`internal/pgtest`, `internal/sandbox/sandboxtest`, `internal/modeltest`, `internal/blob/blobtest`, `internal/provider/providertest`), whose uncovered statements are the branches that fire only when a suite fails or a live tier is misconfigured. `make test` needs Docker (store/API/sandbox suites) and a Kubernetes cluster (the K8s sandbox contract test; a local kind cluster works) — a missing daemon or cluster is a hard failure, not a skip.

`.env` (gitignored) holds the model endpoint for real end-to-end integration verification: `MODEL_PROTOCOL` (`anthropic`|`openai`), `MODEL_BASE_URL`, `MODEL_API_KEY`, `MODEL_ID`. Never commit real credentials. `internal/modeltest` reads them for the tiers that call a real endpoint, and owns their opt-in contract: the file supplies configuration, an environment variable supplies consent (`RUN_LIVE_MODEL_TESTS=1` for the provider live-contract tests, `RUN_EVALS=1` for the eval suite being built), and once opted in, missing configuration fails rather than skips. So `make test` never spends money, and a rotted credential never masquerades as a green build. The tiers, and the eval system built on them, were planned in [docs/plan/02_evals-system.md](./docs/plan/02_evals-system.md).

Parallel sessions run in git worktrees (`claude --worktree`), and a worktree is a fresh checkout: it has no `.env` unless one is copied in, and `modeltest` resolves the file from the *worktree's* root, not the main checkout's. **[.worktreeinclude](./.worktreeinclude)** lists what gets copied — `.env` and a filled-in `model-providers.json`, both gitignored precisely because they carry a live key. Keep it to files something reads and the worktree cannot regenerate; the file itself says why each entry is there and why the obvious candidates (caches, locks, `go.work`) are not.

Verify wire-compat end-to-end by pointing the real `ant` CLI at the local server with `--base-url http://localhost:PORT` and running `ant beta:agents/environments/sessions ...` and `ant beta:worker poll`. No `ant` binary is installed — build it from the read-only checkout (path in docs/REFERENCE_PROJECTS.md): `go build -o <scratch>/ant ./cmd/ant`. (Management commands ignore `ANTHROPIC_BASE_URL` — the CLI builds its client with `WithoutEnvironmentDefaults` and its global `--base-url` flag has no env source; only the worker/auth subcommands honor the env var.)

The module path `github.com/OpenSDLC-Dev/managed-agent-platform` carries the owner's mixed case deliberately — it must match the GitHub owner exactly (Go escapes the uppercase letters in the module cache).

## Iteration workflow (branch → review → PR → CI → squash merge)

Every change lands through a PR; **never commit directly to `main`**.

Review tiering: a diff whose changed paths (by `git diff main...HEAD --name-only`) are exclusively documentation markdown — `*.md` files outside `.claude/`, excluding CLAUDE.md and AGENTS.md themselves — may run a **single** code reviewer instead of the dual pass in step 4, but always keeps the verifier run, including its docs-consistency rung. Markdown that steers behavior (`.claude/` agents and skills, CLAUDE.md, AGENTS.md), anything else, or any ambiguity takes the full ritual.

1. Branch off a fresh `main`: `git checkout main && git pull && git checkout -b <type>/<short-name>` (e.g. `feat/telemetry`, `fix/event-seq`, `chore/ci`).
2. Develop on the branch (TDD as below). **Docs move with code, in the same PR:** a CHANGELOG.md entry for every notable change — the **one** place a change's narrative is written (docs/HISTORY.md receives only acceptance-run and review-hardening records, decisions evaluated and rejected, and archived plans' progress summaries — never a per-PR narrative); the active plan's frontmatter status and STATE.md's Active work/Tasks (truthful, within its ~30-line budget) whenever the change starts, advances, or archives a plan — or starts, advances, or finishes plan-less issue work STATE.md tracks; a docs/DIVERGENCES.md entry for any new wire divergence or inference; README.md (status line, development notes) whenever the change alters what it describes — README's roadmap section deliberately defers to CHANGELOG.md and the issue tracker instead of tracking work itself. A doc that overclaims or lags the code is a defect, not a nice-to-have — the verifier checks docs consistency as a dedicated rung.
3. Run the **verifier subagent** (see "Independent verification"); fix findings before review.
4. **Dual code review**, one pass each: the Codex reviewer and `/code-review` (Claude reviewer). **Model and reasoning effort are not defaults you may accept — a weak reviewer finds nothing and its silence reads like a clean bill of health.** The exact invocations and pins (verifier stays on its pinned `claude-fable-5`; `/code-review` agents run on Opus 4.8 via the persisted-script edit; Codex runs `gpt-5.6-sol` at the config's `ultra` effort through the `task` subcommand) live in the **`run-reviews` skill** (`.claude/skills/run-reviews/SKILL.md`), which governs both reviewers and the verifier — read it before launching any of them. Branch scope reviews the **committed** diff against `main` — commit before launching, or uncommitted fixes escape the review. Address findings from both reviewers; if a fix changes behavior, re-run the verifier. **Verify every finding against the source before acting on it** — reviewers have produced confidently-argued findings that were false (see the `dec.More()` note in `internal/provider/config.go`); refute with evidence rather than "fixing" working code.
5. Push and open the PR (`gh pr create`); include the verifier verdict and the outcome of every review the tier required in the description — a stalled reviewer pass (see the skill's stall note) is reported as such, never silently dropped.
6. Wait for CI (`.github/workflows/ci.yml`) to be fully green (`gh pr checks --watch`) **and settle every review thread** — CodeRabbit and other bots included: each thread closed by a fix or by an evidence-backed refutation posted as a reply, then resolved. Red CI → fix on the branch; never merge red.
7. **Squash merge** only with CI green and zero unresolved review threads (`gh pr merge --squash --delete-branch`), then sync local: `git checkout main && git pull`.

## How to work in this repo

These bias toward caution over speed. For trivial changes, use judgment.

**Think before coding.** State assumptions explicitly rather than picking silently. This codebase has a specific failure mode: *guessing at the wire schema*. When an exact JSON shape isn't in the public docs, do not invent it — read the reference checkouts (SDK types first), and record a real `ant` CLI stream when only behavior can answer. Likewise, if a requirement admits two readings (e.g. whether a field is session-local or agent-level), surface both and ask. If something is confusing, stop and name what's confusing. If a simpler approach exists, say so — push back when warranted.

**Assessment is a deliverable too.** When the user is describing a problem or asking a question rather than requesting a change, report your findings and stop — don't fix until asked. Before any state-changing command (killing a server, dropping a test database, rewriting `.claude/` or workflow config), check the evidence supports *that specific* action — a pattern-matched signal may have a different cause.

**Pause only when the work genuinely requires the user.** That means: a destructive or irreversible action, a real scope change, or input only they can provide (credentials, account approvals, a product decision). Hitting one of these, ask and end the turn. Everything else — retries, missing information you can gather, long verification loops — is yours to push through; never end the turn on a promise of work not yet done.

**Simplicity first.** Write the minimum code that solves the problem; nothing speculative. The plan deliberately *reserves seams* for vaults, deployments, memory, multi-agent threads, and skills — reserving a seam means a column or an interface boundary, **not** an implementation. Do not build ahead of the current slice. No abstractions for single-use code, no configurability nobody asked for, no error handling for impossible states. If 200 lines could be 50, rewrite it. The test: would a senior engineer call this overcomplicated?

**Surgical changes.** Every changed line should trace directly to the request. Don't "improve" adjacent code, comments, or formatting; match the existing style even where you'd choose differently. Clean up orphans *your* change created (now-unused imports, functions); leave pre-existing dead code alone — mention it instead of deleting it.

**Goal-driven execution.** Turn each task into a verifiable goal before writing code, and state the plan as steps with their checks:

```
1. [Step] → verify: [check]
```

Concretely here: "add validation" → write the failing test for invalid input first; "fix the bug" → write the reproducing test first; "refactor X" → tests green before and after. Strong success criteria let you loop to done without check-ins; weak ones ("make it work") don't.

**Independent verification (definition of done).** The implementer never certifies their own work. Before any nontrivial change is declared done — before STATE.md's task progress claims new behavior, before a commit that claims working behavior — dispatch the **`verifier` subagent** (`.claude/agents/verifier.md`) with what changed and the success criteria. It derives the change scope itself, reruns the whole gate (`make verify`, no cached results), exercises runtime behavior where a surface exists, diffs wire-compat claims field-by-field against the reference checkouts, and checks that the project docs (STATE.md, README.md, CHANGELOG.md, docs/HISTORY.md) correctly describe the change. A FAIL or unresolved blocker finding means the work is **not done**: fix, then re-verify. Dispatch it with no model override so its pinned model wins (pinning rules: the `run-reviews` skill) — a weak verifier certifies anything. The verifier's verdict and evidence belong in the final report to the user.

**Report only what you can evidence.** Every progress claim cites a tool result from this session — a test run, a diff, a CLI transcript. Unverified work is labeled as such; failures are reported with their output; skipped steps are named. Done-and-verified is stated plainly, without hedging.

**Write the final report for a reader who wasn't there.** Open with the outcome, then the supporting detail, then what's needed from the user. Complete sentences, terms spelled out, no arrow chains, no labels invented mid-session unless re-introduced. If short and clear conflict, choose clear.

**Testing conventions.**
- **TDD** for anything with behavior: contract test first, then implement. This matters most for provider adapters, event/JSON round-trips against the wire schema, sandbox providers, and the work-queue lease state machine.
- Keep files focused and small; one clear responsibility per package.
- Provider-, sandbox-, and queue-backend variability lives behind interfaces with a **shared contract test suite** — every new backend must pass the same suite.
- Confine lossy conversions to a single package (`provider/openai`) and test them hard; the Anthropic-protocol provider should be near-zero-conversion.
