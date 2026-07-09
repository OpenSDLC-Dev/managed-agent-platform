# CLAUDE.md

Guidance for Claude Code (and contributors) working in this repository.

## What this is

An **open-source, self-hostable platform for long-horizon agents**, written in **Go**, Apache-2.0.
Goal: let enterprises run the whole thing **on-prem / in their own VPC** — data and compute never leave their boundary.

We take Anthropic's **Claude Managed Agents** as our **reference implementation**: we adopt its domain model and keep the public REST API **wire-compatible** with it, so the real `ant` CLI and Anthropic SDKs can drive our server unchanged. Referencing it is a deliberate compatibility and design choice, not an attempt to reproduce it — where our goals (self-hosting, pluggable model backends, OTel) call for something different, we diverge on purpose and say so.

Full design doc: `~/.claude/plans/agent-managed-agent-encapsulated-moonbeam.md` (approved plan; read it before large changes).

**Current progress: [STATE.md](./STATE.md)** — which delivery slice is done, what's next, and the open questions. Read it at the start of a session; update it whenever a slice changes status.

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

## Wire-compatibility rules

- Mirror Anthropic's resource model, paths, JSON fields, and **ID prefixes**: `agent_` `env_` `sesn_` (accept `session_`) `sevt_` `work_` `vlt_` `sesrsc_` `depl_` `drun_` `file_` `skill_`.
- Accept and ignore `anthropic-version` / `anthropic-beta` headers; honor `?beta=true` where the reference does.
- Auth: management via `x-api-key`; workers via environment key (`Authorization: Bearer`, scoped to one environment's work queue).
- Event taxonomy is `{domain}.{action}` — see the plan's Component 2 for the full list. SSE deltas use `content_delta` (NOT Messages API's `content_block_delta`).
- Never guess at a wire shape. Resolution order: public docs → the local reference checkouts below (the Go SDK's typed schema first, then the `ant` CLI source) → recording a real `ant` CLI stream for behavior the types can't capture (ordering, SSE framing, defaults).

## Reference source checkouts (local)

Three read-only local checkouts serve as ground truth where the public docs are silent. In a new session, `/add-dir` them when needed.

- `/Users/hele/Projects/anthropic-sdk-go` — official Go SDK (also our primary dependency). The **typed wire schema** for everything managed-agents: `betasessionevent.go` (full event taxonomy, both directions), `betaagent.go` / `betaenvironment.go` / `betasession.go` (resources), `betaenvironmentwork.go` (work API).
- `/Users/hele/Projects/anthropic-cli` — the real `ant` CLI source. `pkg/cmd/beta*.go` and `pkg/cmd/worker.go` show **client-side behavior**: polling, SSE/stream handling, defaults, headers.
- `/Users/hele/Projects/claude-code-source` — Claude Code source. **Design reference only** for harness concerns (agent loop, tool orchestration, permission flow). Not a wire-schema source; never copy code from it.

Caveats: these checkouts track the API's tip and already contain post-plan surface (`agent.thread_*` events, memory-store betas). Wire-compat is judged against the SDK version pinned in `go.mod` (once that dependency lands — today `go.mod` has none); new surface in the checkouts is not an invitation to build ahead of the current slice.

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

Intended commands (wire up in a `Makefile` as the code lands):

```
go build ./...            # build all binaries
go test ./...             # unit + contract tests
go vet ./... && gofmt -l . # lint
docker compose -f deploy/compose/docker-compose.yml up   # local: controlplane+brain+executor+Postgres(+Jaeger)
```

CI (`.github/workflows/ci.yml`) runs the build/vet/gofmt/test commands above and additionally enforces a **total statement coverage gate ≥ 90%**, computed exactly from the coverage profile over `./internal/...` (logic packages; `cmd/` main glue is deliberately outside the denominator).

`.env` (gitignored) holds the model endpoint for real end-to-end integration verification: `MODEL_PROTOCOL` (`anthropic`|`openai`), `MODEL_BASE_URL`, `MODEL_API_KEY`, `MODEL_ID`. Nothing consumes it yet — the provider slice's integration tests will read these variables. Never commit real credentials.

Verify wire-compat end-to-end by pointing the real `ant` CLI at the local server (`ANTHROPIC_BASE_URL=http://localhost:PORT`) and running `ant beta:agents/environments/sessions ...` and `ant beta:worker poll`.

## Iteration workflow (branch → review → PR → CI → squash merge)

Every change lands through a PR; **never commit directly to `main`**.

1. Branch off a fresh `main`: `git checkout main && git pull && git checkout -b <type>/<short-name>` (e.g. `feat/telemetry`, `fix/event-seq`, `chore/ci`).
2. Develop on the branch (TDD as below). A slice's STATE.md status flip belongs in the same PR as the slice.
3. Run the **verifier subagent** (see "Independent verification"); fix findings before review.
4. **Dual code review**, one pass each: `/codex:review --background` (Codex reviewer) and `/code-review` (Claude reviewer). `/codex:review` is user-invocable only (`disable-model-invocation`); from a session, run the underlying reviewer as a background Bash task: `node "<plugin-root>/scripts/codex-companion.mjs" review "--scope branch --base main"`, where `<plugin-root>` is the newest directory under `~/.claude/plugins/cache/openai-codex/codex/` (backgrounding comes from the Bash task, not a flag) and the scope flags are this workflow's choice. Branch scope reviews the **committed** diff against `main` — commit before launching it, or uncommitted fixes escape the review. Read the task's output file when it completes. Address findings from both reviewers; if a fix changes behavior, re-run the verifier.
5. Push and open the PR (`gh pr create`); include the verifier verdict and both review outcomes in the description.
6. Wait for CI (`.github/workflows/ci.yml`) to be fully green: `gh pr checks --watch`. Red CI → fix on the branch; never merge red.
7. **Squash merge** (`gh pr merge --squash --delete-branch`), then sync local: `git checkout main && git pull`.

## How to work in this repo

These bias toward caution over speed. For trivial changes, use judgment.

**Think before coding.** State assumptions explicitly rather than picking silently. This codebase has a specific failure mode: *guessing at the wire schema*. When an exact JSON shape isn't in the public docs, do not invent it — read the reference checkouts (SDK types first), and record a real `ant` CLI stream when only behavior can answer. Likewise, if a requirement admits two readings (e.g. whether a field is session-local or agent-level), surface both and ask. If something is confusing, stop and name what's confusing. If a simpler approach exists, say so — push back when warranted.

**Simplicity first.** Write the minimum code that solves the problem; nothing speculative. The plan deliberately *reserves seams* for vaults, deployments, memory, multi-agent threads, and skills — reserving a seam means a column or an interface boundary, **not** an implementation. Do not build ahead of the current slice. No abstractions for single-use code, no configurability nobody asked for, no error handling for impossible states. If 200 lines could be 50, rewrite it. The test: would a senior engineer call this overcomplicated?

**Surgical changes.** Every changed line should trace directly to the request. Don't "improve" adjacent code, comments, or formatting; match the existing style even where you'd choose differently. Clean up orphans *your* change created (now-unused imports, functions); leave pre-existing dead code alone — mention it instead of deleting it.

**Goal-driven execution.** Turn each task into a verifiable goal before writing code, and state the plan as steps with their checks:

```
1. [Step] → verify: [check]
```

Concretely here: "add validation" → write the failing test for invalid input first; "fix the bug" → write the reproducing test first; "refactor X" → tests green before and after. Strong success criteria let you loop to done without check-ins; weak ones ("make it work") don't.

**Independent verification (definition of done).** The implementer never certifies their own work. Before a slice or any nontrivial change is declared done — before STATE.md flips a status, before a commit that claims working behavior — dispatch the **`verifier` subagent** (`.claude/agents/verifier.md`) with what changed and the success criteria. It re-derives expectations from the docs, reruns every check itself (`go test -count=1`, no cached results), exercises runtime behavior where a surface exists, and diffs wire-compat claims field-by-field against the reference checkouts. A FAIL or unresolved blocker finding means the work is **not done**: fix, then re-verify. The verifier's verdict and evidence belong in the final report to the user.

**Testing conventions.**
- **TDD** for anything with behavior: contract test first, then implement. This matters most for provider adapters, event/JSON round-trips against the wire schema, sandbox providers, and the work-queue lease state machine.
- Keep files focused and small; one clear responsibility per package.
- Provider-, sandbox-, and queue-backend variability lives behind interfaces with a **shared contract test suite** — every new backend must pass the same suite.
- Confine lossy conversions to a single package (`provider/openai`) and test them hard; the Anthropic-protocol provider should be near-zero-conversion.
