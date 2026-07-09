# CLAUDE.md

Guidance for Claude Code (and contributors) working in this repository.

## What this is

An **open-source, self-hostable platform for long-horizon agents**, written in **Go**, Apache-2.0.
Goal: let enterprises run the whole thing **on-prem / in their own VPC** — data and compute never leave their boundary.

We take Anthropic's **Claude Managed Agents** as our **reference implementation**: we adopt its domain model and keep the public REST API **wire-compatible** with it, so the real `ant` CLI and Anthropic SDKs can drive our server unchanged. Referencing it is a deliberate compatibility and design choice, not an attempt to reproduce it — where our goals (self-hosting, pluggable model backends, OTel) call for something different, we diverge on purpose and say so.

Full design doc: `~/.claude/plans/agent-managed-agent-encapsulated-moonbeam.md` (approved plan; read it before large changes).

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
- When a receive-direction event's exact JSON isn't in the public docs, verify against the real `ant` CLI (record the stream) rather than guessing.

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

Verify wire-compat end-to-end by pointing the real `ant` CLI at the local server (`ANTHROPIC_BASE_URL=http://localhost:PORT`) and running `ant beta:agents/environments/sessions ...` and `ant beta:worker poll`.

## How to work in this repo

These bias toward caution over speed. For trivial changes, use judgment.

**Think before coding.** State assumptions explicitly rather than picking silently. This codebase has a specific failure mode: *guessing at the wire schema*. When a receive-direction event's exact JSON isn't in the public docs, do not invent it — record the real `ant` CLI stream and match it. Likewise, if a requirement admits two readings (e.g. whether a field is session-local or agent-level), surface both and ask. If something is confusing, stop and name what's confusing. If a simpler approach exists, say so — push back when warranted.

**Simplicity first.** Write the minimum code that solves the problem; nothing speculative. The plan deliberately *reserves seams* for vaults, deployments, memory, multi-agent threads, and skills — reserving a seam means a column or an interface boundary, **not** an implementation. Do not build ahead of the current slice. No abstractions for single-use code, no configurability nobody asked for, no error handling for impossible states. If 200 lines could be 50, rewrite it. The test: would a senior engineer call this overcomplicated?

**Surgical changes.** Every changed line should trace directly to the request. Don't "improve" adjacent code, comments, or formatting; match the existing style even where you'd choose differently. Clean up orphans *your* change created (now-unused imports, functions); leave pre-existing dead code alone — mention it instead of deleting it.

**Goal-driven execution.** Turn each task into a verifiable goal before writing code, and state the plan as steps with their checks:

```
1. [Step] → verify: [check]
```

Concretely here: "add validation" → write the failing test for invalid input first; "fix the bug" → write the reproducing test first; "refactor X" → tests green before and after. Strong success criteria let you loop to done without check-ins; weak ones ("make it work") don't.

**Testing conventions.**
- **TDD** for anything with behavior: contract test first, then implement. This matters most for provider adapters, event/JSON round-trips against the wire schema, sandbox providers, and the work-queue lease state machine.
- Keep files focused and small; one clear responsibility per package.
- Provider-, sandbox-, and queue-backend variability lives behind interfaces with a **shared contract test suite** — every new backend must pass the same suite.
- Confine lossy conversions to a single package (`provider/openai`) and test them hard; the Anthropic-protocol provider should be near-zero-conversion.
