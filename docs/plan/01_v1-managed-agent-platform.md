---
status: archived
---

# Open-source self-hosted Managed Agent Platform — technical design

> **Archived.** The v1 design plan, authored in plan mode before this repository existed
> and imported on the plan-management restructure (translated from the Chinese original at
> `~/.claude/plans/agent-managed-agent-encapsulated-moonbeam.md`; terminology normalized in
> translation, e.g. 线兼容 → wire-compatible throughout). Content is preserved as
> written — including details that later drifted during implementation (e.g. the toolset
> shipped six tools, without `web_fetch`/`web_search`); CHANGELOG.md, docs/HISTORY.md and
> docs/DIVERGENCES.md record what actually landed, and this file is not back-edited.
> Implemented as v0.1.0.

## Context (why build this)

Build a **high-quality open-source product benchmarked against Anthropic's "Claude Managed Agents"**. The core value proposition: let **enterprises deploy a long-horizon agent platform fully privately / on-prem**, onto their own Kubernetes or data centers, with data and compute never leaving the enterprise boundary.

The reference implementation's core idea (borrowed directly) — an agent splits into three **independently replaceable** virtualized components:

- **Session**: an append-only event log, the **single source of truth** for durable state;
- **Harness ("brain")**: the loop that calls the model and routes tool calls to infrastructure — **stateless, horizontally scalable**;
- **Sandbox ("hands")**: disposable containers that execute code and edit files — "cattle, not pets".

The decoupling payoff: a container crash = one tool-call error (no session lost); a harness crash = any new harness `wake(sessionId)` replays the log and continues; sessions need not wait on container cold starts (the reference implementation credits this with p50 TTFT −60% / p95 −90%). Two security invariants adopted wholesale: **tokens never enter the sandbox**, and **a session ≠ a context window**.

The key research conclusion (which shaped the entire harness design): `google/adk-go` (module `google.golang.org/adk/v2`, Apache-2.0) is a high-quality **single-process, in-process** agent runtime, but its `Runner` assumes the model loop, tool execution, and session writes happen **synchronously in one process** — a structural conflict with brain/hands/session decoupling. This plan therefore treats **adk-go as a "library", never as a "runtime"**.

## Settled decisions (agreed with the user)

| Dimension | Decision |
|---|---|
| Positioning | Open-source product for enterprise on-prem/private deployment; benchmarked against Claude Managed Agents |
| License | **Apache-2.0, pure open source** (no open-core, no edition gating) |
| Language / stack | **All Go** (control plane + harness unified); built on `google.golang.org/adk/v2` (Apache-2.0), used as a library |
| Model backends | **Pluggable, config-driven providers**: each provider exposes `model` / `base_url` / `api_key` (+ optional headers); **the Anthropic-protocol provider accepts any endpoint speaking Anthropic Messages** (api.anthropic.com is not hard-coded); Anthropic-protocol + OpenAI-compatible ship first; Bedrock/Vertex/local later |
| Domain model | **Anthropic managed-agents is the sole authority** (agents/environments/sessions/events/spans all Anthropic-native); **adk-go is reference only** — on conflict, Anthropic wins |
| Harness | **Clean-room, model-agnostic**; Anthropic-native domain types, self-built orchestration loop; adk-go borrowed only narrowly where it does **not** conflict (see "adk-go usage boundary") — its domain model / `Runner` / `session` / `server` are **not** adopted |
| Observability | **First-class**: native OTel / OTLP (traces + metrics + logs); Anthropic `span.*` events correspond one-to-one with OTel spans |
| Sandbox | **Abstract sandbox-provider interface**; Docker first (v1), Kubernetes right after; unified pull-based execution (cloud and self_hosted/BYOC share one protocol) |
| API | **Wire-compatible with Anthropic managed-agents** (`managed-agents-2026-04-01`), so the existing `ant` CLI / Anthropic SDKs connect to our server directly |
| Identity / tenancy | v1 is **single-tenant + API key**; the data model reserves `org/workspace/project` scoping from day 1; RBAC/SSO later |
| v1 first-class scenario | **General task agent** (bash + file + web toolset; git/repo mounting is not first-class) |
| Execution decoupling | **Fully async, uniformly through the event log / work queue**: the brain emits `agent.tool_use` → an executor claims and runs it → posts the tool_result → the brain wakes and continues |
| v1 scope | **Core loop first**: Agents + Environments + Sessions + Events + sandbox execution + permissions/approval |

## Design principles

1. **Anthropic's domain model is the single source of truth.** Our internal types (agent/environment/session/event/span/stop_reason/permission_policy, etc.) are Anthropic-native throughout, aligned directly with the wire-compatible schema, never bent to any third-party library's shape.
2. **adk-go is a reference, not a foundation.** Wherever an adk abstraction conflicts with the Anthropic domain model (especially its genai-centric event/session model, the in-process `Runner`, `server/adkrest`), **do not use it**. Only narrow libraries that clearly do not conflict and clearly save work are borrowed on demand (see below). This also avoids being held hostage by adk v2's active evolution.
3. **Observability is built in, not bolted on.** Every cross-component call carries OTel context propagation from day 1; `span.*` domain events and OTel spans come from the same source.

### adk-go usage boundary (only where non-conflicting)

| adk component | Use? | Notes |
|---|---|---|
| Domain model / `session.Service` / `Event` (genai-centric) | **No** | Conflicts directly with Anthropic event/session → build Anthropic-native ourselves |
| `Runner` / `internal/llminternal.Flow` | **No** | Assumes in-process synchronous execution + holds session handles → conflicts with brain/hands decoupling |
| `server/adkrest` | **No** | Routes/events are not Anthropic → build the wire-compatible REST ourselves |
| MCP client | **No (use the official one directly)** | Depend on `github.com/modelcontextprotocol/go-sdk` directly (adk merely wraps it); avoids adk coupling |
| Anthropic provider | **No (use the official one directly)** | Implement our provider on `github.com/anthropics/anthropic-sdk-go` directly |
| `tool/functiontool`'s jsonschema inference, `skilltoolset` (SKILL.md parsing), multi-agent/`plugin` **patterns** as reference | **Reference / optional** | Borrow code or copy the pattern only where non-conflicting and work-saving; never a hard dependency |

Net effect: adk-go is demoted from "foundation" to "reference implementation + a few optional narrow tools"; the primary dependencies are `anthropic-sdk-go` + `modelcontextprotocol/go-sdk` + `go.opentelemetry.io/otel` + our own Anthropic-native domain layer.

## Goals and non-goals

**v1 goals**: an enterprise can `helm install` onto its own k8s; a REST API wire-compatible with Claude Managed Agents; the complete loop "create agent → create environment → create session → post `user.message` → brain calls the model → emits `agent.tool_use` → the sandbox executor runs it → posts `agent.tool_result` → SSE streams events back → one human approval through `always_ask` → `session.status_idle`" runs end to end; the model backend can point at the enterprise's own Anthropic key or an internal OpenAI-compatible gateway.

**v1 non-goals (seams reserved, later versions)**: Vaults / egress secret injection, Deployments (cron), Memory stores, multi-agent thread orchestration, Skills distribution and execution, git/repo mounting and the Files API, multi-tenant RBAC/SSO, Redis-backed queue scaling.

## Overall architecture

```
  ant CLI / Anthropic SDK ──REST(x-api-key)──▶ ┌── Control Plane (Go) ─────────────────────┐
  (wire-compatible)                            │  /v1/agents /environments /sessions       │
                                               │  /sessions/{id}/events  (POST + SSE)      │
                                               │  /environments/{id}/work/* (BYOC worker)  │
                                               │  resource CRUD + optimistic version       │
                                               │  session state machine (idle/running/…)   │
                                               └──┬───────────────┬────────────────┬───────┘
                                 enqueue "model-turn" │           │ append-only     │ enqueue "tool-exec"
                                                      ▼           ▼ event log       ▼  (work queue)
                                          ┌────────────────┐  ┌──────────┐   ┌──────────────────────────┐
                                          │ Brain / Harness│  │ Postgres │   │ Executor (sandbox worker)│
                                          │ pool (Go,      │◀▶│ events   │◀─▶│  Docker / K8s provider   │
                                          │ stateless):    │  │ sessions │   │  runs bash/file/web tools│
                                          │ adk model.LLM  │  │ agents…  │   │  in a per-session sandbox│
                                          │ + genai + tools│  └──────────┘   └──────────────────────────┘
                                          └────────────────┘                     ▲ same pull protocol
                                                                                 │
                                                              customer BYOC worker (ant beta:worker
                                                              or our worker binary) pulls /work/poll
```

**Two kinds of internal work + one event log**:

- **model-turn work**: a session needs one model inference → a brain claims it, replays the log, calls the provider, and emits `agent.*`/`span.*` events.
- **tool-exec work**: an `agent.tool_use` the brain emitted → one executor claims it, runs it inside the sandbox, and posts `agent.tool_result` (platform-managed cloud) or `user.tool_result` (self_hosted/BYOC).
- Brains and executors communicate **only** through the control plane (event log + work API), never directly → brain/hands decoupling and "zero inbound holes" fall out naturally.

## Component 1: Control plane (wire-compatible REST)

Goal: the real `ant` CLI and the official Anthropic SDKs work against our server **without code changes** — just point the base URL at it. Mirror the resource model, paths, fields, ID prefixes, and event semantics.

**v1 wire-compatible endpoints** (base `/`; uniformly accept and ignore the `anthropic-version`/`anthropic-beta` headers; some take `?beta=true`):

- Agents: `POST/GET /v1/agents`, `GET /v1/agents/{id}`, `PATCH /v1/agents/{id}` (optimistic-lock `version`; mismatch → 409), `GET /v1/agents/{id}/versions`, `POST /v1/agents/{id}/archive`
- Environments: `POST/GET /v1/environments`, `GET /v1/environments/{id}`, `POST /v1/environments/{id}/archive`, `DELETE /v1/environments/{id}`
- Sessions: `POST/GET /v1/sessions`, `GET /v1/sessions/{id}`, `PATCH /v1/sessions/{id}`, `POST /v1/sessions/{id}/archive`, `DELETE /v1/sessions/{id}`
- Events: `POST /v1/sessions/{id}/events`, `GET /v1/sessions/{id}/events` (filterable by `types[]`), `GET /v1/sessions/{id}/events/stream` (SSE)
- Self-hosted worker (BYOC): `GET /v1/environments/{id}/work/poll`, `GET/POST /v1/environments/{id}/work/{work_id}`, `POST .../ack`, `POST .../heartbeat`, `POST .../stop`, `GET .../work`, `GET .../work/stats`

**ID prefixes** (aligned verbatim): `agent_`, `env_`, `sesn_` (also accepting `session_`), `sevt_`, `work_`, `vlt_`, `sesrsc_`, `depl_`, `drun_`, `file_`, `skill_`.

**Auth adaptation layer**: the management side uses `x-api-key` (platform-issued org API keys); the worker side uses environment keys (`Authorization: Bearer`, format modeled on `sk-ant-oat01-...`, scoped to a single environment's work queue). Both are platform-issued, stored in Postgres, rotatable.

**Key request/response shapes** (to be implemented verbatim in v1; details in the research notes):

- `POST /v1/agents` body: `name` (required), `model` (a string or `{"id","speed":"standard|fast"}`), `system`, `tools[]`, `mcp_servers[]`, `skills[]`, `multiagent`, `description`, `metadata`; the response carries `id/type/version (from 1)/created_at/updated_at/archived_at`.
- The `tools[]` union: `{"type":"agent_toolset_20260401", default_config?, configs?[]}` (tool names `bash read write edit glob grep web_fetch web_search`), `{"type":"custom", name, description, input_schema}`, `{"type":"mcp_toolset", mcp_server_name, default_config?, configs?[]}`. `permission_policy`: `{"type":"always_allow"|"always_ask"}` (default: agent toolset = allow, mcp toolset = ask).
- `POST /v1/environments` body: `name`, `config{type:"cloud"|"self_hosted", packages{apt|cargo|gem|go|npm|pip:[...]}, networking{type:"unrestricted"} | {type:"limited", allowed_hosts[], allow_mcp_servers, allow_package_managers}}`.
- `POST /v1/sessions` body: `agent` (a string | `{type:"agent",id,version}` | `{type:"agent_with_overrides",id,model?,system?,tools?,mcp_servers?,skills?}`), `environment_id` (required), `vault_ids[]?`, `resources[]?`, `title?`, `metadata?`; status `idle|running|rescheduling|terminated`; `stop_reason`: `{type:"requires_action", event_ids[]}` | `{type:"end_turn"}`; `usage{input_tokens,output_tokens,cache_creation_input_tokens,cache_read_input_tokens}`.

**Rate limits** (aligned): create-class 300 req/min, read-class 1200 req/min (per org).

## Component 2: Event log (Postgres, append-only)

The event log is the **single source of truth**, serving brain replay, the SSE stream, audit, and worker dispatch all at once.

- Table `events(id sevt_…, session_id, seq bigint, thread_id?, type, payload jsonb, processed_at, created_at)`; `(session_id, seq)` unique, `seq` monotonic (one counter per session; writes allocate it with `INSERT …` + an advisory lock, or `SELECT … FOR UPDATE`).
- **Event taxonomy** (aligned verbatim with Anthropic):
  - user (inbound): `user.message`, `user.interrupt`, `user.tool_confirmation`, `user.custom_tool_result`, `user.tool_result` (self_hosted posting agent_toolset results), `user.define_outcome`, `system.message`
  - agent (outbound): `agent.message`, `agent.thinking`, `agent.tool_use`, `agent.tool_result`, `agent.mcp_tool_use`, `agent.mcp_tool_result`, `agent.custom_tool_use`
  - session: `session.status_running`, `session.status_idle` (with `stop_reason`), `session.status_rescheduled`, `session.status_terminated`, `session.error` (with typed `error` + `retry_status`), `session.updated`, `session.deleted`
  - span (observability): `span.model_request_start`, `span.model_request_end` (with `model_usage`)
- **SSE stream**: `GET /events/stream`, each frame `data: {json}\n\n`; open the stream before posting events to avoid the race; reconnection seeds seen `id`s by listing history and resumes by `seq`. Multi-replica control planes fan out via **Postgres LISTEN/NOTIFY** (v1, zero extra dependencies); Redis pub/sub is the scale-out option.
- **Event-delta previews** (opt-in `?event_deltas[]=agent.message`; only `agent.message`/`agent.thinking`): `{"type":"event_start","event":{"type","id"}}` and `{"type":"event_delta","event_id","delta":{"type":"content_delta","index","content":{"type":"text","text"}}}`. Reconciliation: concatenate by `(event_id, index)`; the buffered event replaces the preview on arrival; `span.model_request_end` closes unreconciled previews; previews are neither persisted nor replayed. **Note the delta type is `content_delta`, not the Messages API's `content_block_delta`.**

## Component 3: Brain / harness (Anthropic-native, self-built orchestration)

**Stateless.** Claim model-turn work → replay the event log to rebuild context → call the provider → emit events → suspend (await the tool_result, or go idle). After a crash, a new brain replays and continues. **Domain types are Anthropic-native throughout; adk is borrowed only narrowly.**

1. **Provider abstraction (self-built, Anthropic-native, config-driven)**: define our own `ModelProvider` interface (inputs/outputs speak Anthropic messages semantics directly: system, messages, tools, streaming) rather than adopting adk's `model.LLM`/genai. **Each provider instance is constructed from config**, exposing at least: `protocol` (`anthropic`|`openai`), `model` (the upstream model id), `base_url` (the endpoint URL), `api_key` (or a credential reference), optional `headers`/`timeout`/`extra`. Two protocol adapters ship first:
   - **Anthropic-protocol provider**: uses `github.com/anthropics/anthropic-sdk-go` with a configurable `base_url` → connects to api.anthropic.com or **any endpoint speaking the Anthropic Messages protocol** (an enterprise gateway, a reverse proxy, Bedrock/Vertex's Anthropic-compatible entry points, a self-hosted model exposing the protocol). Near-zero conversion.
   - **OpenAI-compatible provider**: `/chat/completions` (covers vLLM, internal gateways, most self-hosted models), converting Anthropic↔OpenAI requests/responses/tool-calls/streaming.
   - Later: native Bedrock/Vertex SDKs, local runtimes.
   - **model → provider routing**: platform-level configuration (a `model_providers` config/table) maps an agent's `model` string or pattern to one provider config instance (protocol + base_url + model + credentials); an enterprise plugs in its own model endpoint by changing config, never code. Credentials are managed through `internal/store`, vault-attachable later.
2. **Brain orchestration loop (self-built, replacing adk's `Runner`)**: no tools run in-process. The loop body: replay the event log → assemble the provider request → `provider.Generate(streaming)` → convert chunks into Anthropic events written to the log (`agent.message`/`agent.thinking` + `span.model_request_start/end`; `event_start`/`event_delta` when streaming) → on a tool call: emit `agent.tool_use` (or `agent.mcp_tool_use`/`agent.custom_tool_use`), **enqueue tool-exec work and suspend**; if the tool's `permission_policy` is `always_ask`, first go `session.status_idle` + `stop_reason.requires_action` and await `user.tool_confirmation`. When the tool_result event arrives → wake and continue.
3. **Session / event storage (self-built, Anthropic-native)**: the append-only Postgres event log *is* the session state; do **not** implement adk's `session.Service`, do **not** use its genai `Event`. Replay = read events in order and rebuild the provider messages.
4. **MCP tools**: use the official `github.com/modelcontextprotocol/go-sdk` directly (the brain acts as MCP client for tool schemas; actual calls happen on the executor/egress side), bypassing adk's `mcptoolset`.
5. **Optional borrowings (only if non-conflicting)**: function-tool jsonschema inference, SKILL.md parsing, and similar can reference or borrow the corresponding adk code, never as a hard dependency.

**The key point**: the brain only produces "intent" (which model to call, which tool to run); all execution leaves the process through the queue — this satisfies brain/hands decoupling and naturally sidesteps the structural conflict with adk's `Runner` / in-process `tool.Run`.

## Component 4: Executor + sandbox provider (hands)

The executor consumes tool-exec work, runs tools inside a **per-session** disposable container via the SandboxProvider, and posts tool_result events. **Platform-managed cloud and customer BYOC are the same pull protocol at two deployment points.**

```go
type SandboxProvider interface {
    Provision(ctx, spec EnvironmentSpec) (Sandbox, error)  // aligns with provision({resources})
    Attach(ctx, sandboxID string) (Sandbox, error)
}
type Sandbox interface {
    Exec(ctx, ExecRequest) (ExecResult, error)             // bash etc.
    ReadFile / WriteFile / Glob / Grep …                   // backs the built-in toolset
    Checkpoint(ctx) (ref, error)                           // idle snapshot (later)
    Destroy(ctx) error
}
```

- **DockerProvider** (v1): one container = one session's sandbox; `Exec` goes through the Docker API; `/bin/bash` must exist at that exact path; workdir `/workspace`.
- **K8sProvider** (right after): a sandbox = a Pod (optional gVisor/Kata RuntimeClass for hard isolation); `networking` is realized as `unrestricted|limited` via NetworkPolicy.
- **Built-in toolset = `agent_toolset_20260401`**: `bash/read/write/edit/glob/grep` execute inside the sandbox; `web_fetch/web_search` execute on the executor/brain side under egress policy.
- **BYOC / self_hosted**: an environment of `type:"self_hosted"` *is* a work queue; the customer runs our worker binary (or the real `ant beta:worker poll`) to pull `/work/poll`, execute, and post `user.tool_result`. Work API semantics (aligned verbatim): `poll` (`block_ms 1–999`, `reclaim_older_than_ms` default 5000), `ack` (queued→starting), `heartbeat` (optimistic concurrency: first call `expected_last_heartbeat='NO_HEARTBEAT'`, mismatch → 412), `stop` (`{force}`); work states `queued|starting|active|stopping|stopped`; `work.data.id` = the session id; `/work/stats` returns `{depth,pending,workers_polling,oldest_queued_at}`.
- **v1 internal execution shares the same queue**: platform-managed cloud execution is our built-in executor process (embedding the DockerProvider) consuming the same tool-exec queue → a single execution path. The v1 queue is **Postgres `FOR UPDATE SKIP LOCKED`** (zero extra dependencies, offline-first); Redis Streams is the scale-out option (the reference implementation itself uses a Redis Stream consumer group to compute `workers_polling`).

## Component 5: Security seams

- **Permission policy + HITL (implemented in v1, part of the core loop)**: `always_allow`/`always_ask`; `always_ask` → the brain goes `session.status_idle` + `stop_reason{type:"requires_action",event_ids}` → the client posts `user.tool_confirmation{tool_use_id,result:"allow"|"deny",deny_message?}` → once all are resolved, back to `running`. Bridge adk's `tool/toolconfirmation` primitives onto this protocol.
- **Vaults + egress MCP proxy (seam reserved in v1, implemented later)**: the sessions table reserves `vault_ids`; sandbox egress passes through one egress proxy point (pass-through in v1); later, three credential kinds (`mcp_oauth`/`static_bearer`/`environment_variable`) with egress-time substitution (tokens never enter the sandbox). adk's `plugin` before-tool hook is the injection point.
- **Audit**: the event log is naturally the audit substrate; v1 records tool calls and approval decisions.
- **Self-hosted security responsibility**: document the shared-responsibility model (image hardening, capability drops, non-root, read-only rootfs, egress limits, environment-key rotation).

## Component 6: Observability (OTel / OTLP, first-class)

The platform natively supports **OpenTelemetry**, exporting over standard **OTLP**, pluggable into an enterprise's existing Jaeger/Tempo/Prometheus/Grafana or any OTLP collector; offline-friendly (collector endpoint configurable, no external dependencies).

- **Traces**: the span hierarchy mirrors the domain — `session` (root) → `turn` → `model_request` (matching `span.model_request_start/end`) and `tool_exec` (matching `agent.tool_use`→`agent.tool_result`). **Anthropic `span.*` domain events and OTel spans share one source**: a single model request both writes `span.model_request_*` events (for client SSE/audit) and opens an OTel span (for operators). Context propagates as W3C traceparent across control plane → brain → executor (including BYOC workers, via traceparent carried in work items). Span attributes: `session_id`/`event_id`/`work_id`/`agent_id`/`model`/`provider`/`environment_kind`.
- **Metrics** (OTLP metrics): TTFT (work received → first token), model latency, tool-execution latency, queue `depth`/`pending`/`workers_polling`, token usage (input/output/cache_creation/cache_read), session-status counts, approval wait times, provider error rates.
- **Logs**: structured logs correlated by trace/span ids (`--log-format json`, matching the reference worker).
- **Implementation**: one `internal/telemetry` package on `go.opentelemetry.io/otel` + the OTLP exporter; each `cmd/*` initializes tracer/meter providers at startup, endpoint/sampling via config (env + Helm values). Event-log writes and OTel spans are instrumented at the same point, so `span.*` events and traces cannot drift.

## Data model (Postgres, single-tenant with multi-tenant reservations)

Every core table reserves `org_id`/`workspace_id`/`project_id` (v1 fills single-tenant defaults). **The scoping keys are org/workspace/project, not user** — this is an adk-vs-Anthropic conflict point, resolved per the principles in Anthropic's favor.

- `agents` + `agent_versions` (immutable version snapshots; optimistic locking)
- `environments` (config jsonb, kind `cloud|self_hosted`, state)
- `sessions` (resolved agent snapshot jsonb, environment_id, status, vault_ids, resources jsonb, title, usage jsonb)
  - **No `user_id` binding**: Anthropic session ownership is determined by the API key's org/workspace; the wire schema has no `user_id` field, and list filters by `agent_id` (contrast adk's `AppName`+`UserID` two-part key — a conflict; adk semantics dropped).
  - Only a **nullable, audit-only** `created_by` (which API key/principal created it); it takes no part in isolation/partitioning and does not enter the wire schema.
  - **The integration boundary**: end-user ↔ session ownership is maintained by the **application layer** (the app's own `app_user_id ↔ session_id` table). The platform offers two convenience hooks: session `metadata` (an app can store `app_user_id` for filtering/attribution) and the audit `created_by`. "Which sessions can this user see" is application-layer authorization; the platform stays user-agnostic. When multi-tenancy/RBAC lands, scoping remains org/workspace/project (+ principals for authz), and end-user-level ownership stays with the application.
- `events` (see Component 2; append-only, `(session_id,seq)` unique)
- `work_items` (id `work_…`, environment_id, session_id, kind `model_turn|tool_exec`, state, lease_expires_at, last_heartbeat, metadata)
- `api_keys` / `environment_keys` (platform-issued credentials: scope, hash, rotation)

## Go module layout (greenfield proposal)

The working directory is currently empty (not a git repository). Proposal: a single module, multiple binaries. **Primary dependencies**: `github.com/anthropics/anthropic-sdk-go` (Anthropic provider), `github.com/modelcontextprotocol/go-sdk` (MCP client), `go.opentelemetry.io/otel` (+ OTLP exporter), `github.com/jackc/pgx` (Postgres); `google.golang.org/adk/v2` is **not** a hard dependency, imported on demand only when borrowing a narrow tool.

```
cmd/
  controlplane/   # REST API + event log + queue + state machine
  brain/          # harness pool (independently scalable)
  executor/       # built-in sandbox worker (Docker/K8s provider)
  worker/         # distributable BYOC worker (pull protocol; ant beta:worker compatible)
internal/
  domain/         # Anthropic-native domain types (agent/env/session/event/span/…), the single source of truth
  api/            # wire-compatible REST handlers, resource schemas, ID prefixes, auth adaptation
  events/         # event taxonomy, append-only store, SSE (LISTEN/NOTIFY), delta reconciliation
  brain/          # orchestration loop, replay, provider request assembly
  provider/       # ModelProvider interface (config-driven: protocol/model/base_url/api_key) + anthropic/ + openai/ + registry (model→provider routing)
  mcp/            # MCP client wrapper (modelcontextprotocol/go-sdk)
  sandbox/        # SandboxProvider interface + docker/ + k8s/
  queue/          # work queue (pg SKIP LOCKED; redis optional)
  policy/         # permission policy + HITL bridging
  telemetry/      # OTel tracer/meter init, OTLP export, span ↔ span.* same-source events
  store/          # Postgres schema/migrations, reserved multi-tenant columns
deploy/
  helm/           # production (incl. OTLP endpoint values)
  compose/        # dev/trial (optional bundled OTel collector + Jaeger)
```

## Packaging and deployment

- **Helm chart** (production): controlplane + brain + executor as separate Deployments (independently scalable) + Postgres (dependency/subchart) + optional gVisor RuntimeClass.
- **docker-compose** (dev/trial): one command brings up controlplane + brain + executor + Postgres + DockerProvider.
- **Offline-first**: images pre-pullable into a private registry; no hard dependency on any external SaaS (point the model backend at an in-network gateway).

## v1 delivery slices (suggested order)

0. `internal/domain` (Anthropic-native domain types) + `internal/telemetry` (OTel/OTLP init, context propagation) — the two cross-cutting foundations, in place from day 1.
1. Postgres schema + migrations + reserved multi-tenant columns; `store/`.
2. Control-plane CRUD (agents/environments/sessions) + optimistic versions + ID prefixes + `x-api-key` auth.
3. Event log (append-only, seq allocation) + `POST /events` + the SSE stream (with `event_start`/`event_delta` reconciliation) + `span.*` events instrumented at the same source as OTel spans.
4. `ModelProvider` (config-driven: protocol/model/base_url/api_key) + `model_providers` routing + one protocol adapter (Anthropic-protocol or OpenAI-compatible, whichever the enterprise can reach) driving a single model turn; verify a custom `base_url` works.
5. The brain orchestration loop (replay + provider request assembly + Anthropic-native events written to the log), no adk runtime.
6. The tool-exec queue (pg SKIP LOCKED) + executor + DockerProvider + the built-in toolset (bash/files really executing inside the sandbox).
7. Permission policy + the `requires_action`/`user.tool_confirmation` approval loop.
8. The work API (`/work/poll` etc.) wire-compatible + the distributable BYOC worker (verified against the real `ant beta:worker`) + traceparent propagated through work items.
9. K8sProvider + the Helm chart (incl. OTLP endpoint values).

## Verification strategy (end-to-end)

- **Wire compatibility**: drive our server with the real `ant beta:agents/environments/sessions create` + `sessions:events send/stream`; assert resource lifecycle, state machine, and event stream field-for-field. Connect a real `ant beta:worker poll` to a self_hosted environment to verify the work protocol.
- **Loop verification (general task agent)**: one general task (generate/process files with bash inside the sandbox, then summarize), observed over SSE as `session.status_running` → `span.model_request_start` → `agent.message` → `agent.tool_use` → (executor runs) → `agent.tool_result` → `session.status_idle`; set one tool to `always_ask` and complete a `requires_action`/`user.tool_confirmation` round-trip.
- **Resilience**: kill a brain process, assert a new brain replays the event log and resumes where it left off; kill a sandbox container, assert it degrades to a tool error rather than a lost session; kill an executor, assert the work is `reclaim`ed by another executor after lease expiry.
- **Provider consistency**: the same agent behaves identically on AnthropicProvider and OpenAIProvider (a shared contract test suite).
- **Sandbox-provider consistency**: the same agent behaves identically on DockerProvider and K8sProvider (a shared SandboxProvider contract suite).
- **Observability**: after one end-to-end session, assert the OTLP collector (in-memory/Jaeger in tests) received the complete `session→turn→model_request/tool_exec` trace with traceparent carried across control plane → brain → executor (including the BYOC worker); assert `span.model_request_*` events agree with their OTel spans on `event_id`/timing; assert the key metrics (TTFT, token usage, queue depth) were reported.

## Risks and explicit trade-offs

- **OpenAI↔Anthropic semantic conversion**: the domain is Anthropic-native, but the `OpenAIProvider` must convert tool calls, streaming (`content_delta` vs OpenAI deltas), and system/role mapping → confined to the single `provider/openai` package and tested hard; the Anthropic provider is near-zero-conversion.
- **The latency cost of full asynchrony**: every tool call takes one extra hop (brain → queue → executor → log → brain). Acceptable (the reference's self_hosted semantics are exactly this); if cloud ever needs lower latency, add a "nearby executor" fast path without changing event semantics.
- **adk-go as optional borrowing only**: never a hard dependency (see "adk-go usage boundary"); any borrowed code is version-pinned and confined to one package — on conflict, drop it and hand-roll.
- **Documentation-level wire gaps**: some **inbound-direction** events (`agent.tool_use`/`session.status_idle`/`session.error`, etc.) have no verbatim raw JSON in the public docs → align by recording a real `ant` CLI stream during implementation, or capture the OpenAPI reference.
