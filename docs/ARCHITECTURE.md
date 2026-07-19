# ARCHITECTURE.md — the platform as built

The as-built architecture reference: how the platform actually works, component by
component. Related documents divide the labor: **[CLAUDE.md](../CLAUDE.md)** carries the
behavioral guardrails (the non-negotiable design principles and wire-compatibility rules,
in compressed form — this file is their descriptive depth);
**[docs/DIVERGENCES.md](./DIVERGENCES.md)** is the single registry of deliberate wire
divergences and unconfirmed inferences;
**[docs/plan/01_v1-managed-agent-platform.md](./plan/01_v1-managed-agent-platform.md)**
(archived) preserves the original design rationale; **[CHANGELOG.md](../CHANGELOG.md)**
records how each piece landed.

## System overview

An open-source, self-hostable platform for long-horizon agents, wire-compatible with
Anthropic's Claude Managed Agents: the real `ant` CLI and the Anthropic SDKs drive this
server unchanged. An agent is three independently-swappable pieces:

- **Session** — an append-only **event log** in Postgres. The single source of truth:
  all durable state lives here, and everything else can be rebuilt from it.
- **Brain / harness** — the loop that calls the model and routes tool calls.
  **Stateless and horizontally scalable**: a crashed brain loses nothing, because any
  fresh brain replays the log and continues.
- **Sandbox ("hands")** — a disposable per-session container that runs tools. Cattle,
  not pets: a dying container is one tool-call error, not a lost session.

```
  ant CLI / Anthropic SDK ──REST(x-api-key)──▶ ┌── controlplane ───────────────────────────┐
  (wire-compatible)                            │  /v1/agents /environments /sessions       │
                                               │  /sessions/{id}/events  (POST + SSE)      │
                                               │  /environments/{id}/work/* (BYOC worker)  │
                                               │  resource CRUD + optimistic versions      │
                                               │  session state machine (idle/running/…)   │
                                               └──┬───────────────┬────────────────┬───────┘
                                 model_turn work  │               │ append-only    │ tool_exec work
                                                  ▼               ▼ event log      ▼  (work queue)
                                          ┌──────────────┐  ┌──────────┐   ┌──────────────────────────┐
                                          │ brain pool   │  │ Postgres │   │ executor                 │
                                          │ (stateless:  │◀▶│ events   │◀─▶│  Docker / K8s sandbox    │
                                          │ replay log,  │  │ sessions │   │  providers; runs the     │
                                          │ call model,  │  │ agents…  │   │  built-in toolset in a   │
                                          │ emit events) │  └──────────┘   │  per-session container   │
                                          └──────────────┘                 └──────────────────────────┘
                                                                               ▲ same pull protocol
                                                                               │
                                                            customer BYOC worker (ant beta:worker
                                                            or cmd/worker) pulls /work/poll
```

## Process topology

Four binaries under `cmd/`, each independently deployable and scalable; all state in
Postgres, all coordination through it:

| Binary | Role |
|---|---|
| `controlplane` | The wire-compatible REST surface: resource CRUD, the event log endpoints (POST/list/SSE), the work API for BYOC workers, auth (management `x-api-key`, worker environment keys), and the session state machine. |
| `brain` | The harness pool. Claims `model_turn` work, replays the session's event log to rebuild context, calls the model provider, writes the resulting events, enqueues tool work, suspends. |
| `executor` | The built-in sandbox worker for platform-managed (`cloud`) environments. Claims `tool_exec` work, runs the tool inside the session's sandbox container, posts `agent.tool_result`. |
| `worker` | The distributable BYOC worker for `self_hosted` environments. Same pull protocol as the executor, run on customer compute, posting `user.tool_result` — the real `ant beta:worker` works against the same API. |

Processes never talk to each other directly. The brain and the executors communicate
only through the control plane's event log and work queue — which is what makes
"customer-run worker with zero inbound network access" the same code path as the
platform's own executor, just deployed elsewhere.

## Execution flow

**Fully asynchronous through the event log and the work queue.** One turn:

1. A client POSTs `user.message`; the session goes `running` and a `model_turn` work
   item is enqueued.
2. A brain claims it, replays the log into provider messages, and streams the model's
   response — writing `agent.message` / `agent.thinking` events (with opt-in
   `event_start`/`event_delta` SSE previews) and `span.model_request_start/_end`.
3. A tool call becomes an `agent.tool_use` event plus a `tool_exec` work item; the
   brain suspends (it holds nothing in memory a crash could lose).
4. For a platform-managed (`cloud`) environment the executor claims the item straight
   off the Postgres queue (`FOR UPDATE SKIP LOCKED`, lease + reclaim); for a
   `self_hosted` environment a BYOC worker claims the same kind of item over the wire
   work API (`poll`/`ack`/`heartbeat`/`stop`, lease expiry, dead-worker reclaim) — the
   same pull semantics at two deployment points. Either runs the tool in the session's
   sandbox and posts the result event (`agent.tool_result` platform-managed,
   `user.tool_result` self-hosted).
5. The commit that appends the result also enqueues the next `model_turn` — only once
   every tool use in the turn is answered. A brain claims it (brains wake by polling the
   queue; Postgres LISTEN/NOTIFY serves the SSE fan-out, not the brain), replays, and
   continues until the model stops calling tools, then writes `session.status_idle`
   with `stop_reason.end_turn`.

**Permissions / human-in-the-loop.** A tool whose resolved `permission_policy` is
`always_ask` suspends the session *before* execution: the brain writes
`session.status_idle` with `stop_reason:{type:"requires_action", event_ids:[…]}` naming
the blocked `agent.tool_use` events (stamped `evaluated_permission:"ask"`). A client
answers each with `user.tool_confirmation{tool_use_id, result:"allow"|"deny",
deny_message?}`; allow releases the tool to the queue, deny synthesizes an
`is_error:true` `agent.tool_result` carrying the deny message, and the turn resumes
either way.

**Crash recovery is replay.** Sessions are never bound to a brain: any brain can pick up
any session's next turn from the log. A sandbox container dying surfaces as one
tool-call error; a worker dying strands its lease, which `poll` reclaims after expiry.

## Wire-compatibility model

The public REST API mirrors Anthropic's Claude Managed Agents resource model — paths,
JSON fields, ID prefixes (`agent_` `env_` `sesn_`/`session_` `sevt_` `work_` …),
pagination and error envelopes, and the `{domain}.{action}` event taxonomy (SSE deltas
use `content_delta`, not the Messages API's `content_block_delta`). The typed schema in
the pinned `anthropic-sdk-go` checkout is the ground truth; client behavior comes from
the `ant` CLI source (see [REFERENCE_PROJECTS.md](./REFERENCE_PROJECTS.md)). Where we
deliberately diverge — or infer behavior the references don't pin down —
[DIVERGENCES.md](./DIVERGENCES.md) is the single registry; the verifier resolves
wire-compat findings against it.

Model access is **config-driven**: a provider is constructed from `protocol`
(`anthropic` | `openai`) + `model` + `base_url` + `api_key` (+ optional headers), and a
`model_providers` routing table maps an agent's model string to a provider instance.
The Anthropic-protocol adapter is near-zero-conversion and works against any endpoint
speaking Anthropic Messages; the OpenAI-compatible adapter is the platform's one lossy
conversion seam, confined to `internal/provider/openai` and tested hard. Providers are
built with `WithoutEnvironmentDefaults`, so ambient `ANTHROPIC_*` credentials can never
leak to a configured third-party endpoint.

## Package reference

What each package does and where its pieces live, in repo-layout order. Descriptions
originate from the delivery-time records (migrated from HISTORY.md, 2026-07-18) and were
freshness-checked against the code on migration; treat the code as authoritative when
they drift. Where one file carries several distinct responsibilities, it gets one row per
responsibility (`file.go — aspect`). Ordering is by layer: the domain and wire surface
first, then the execution chain, then the shared infrastructure.

### internal/domain

Zero external dependencies (stdlib only), enforcing the rule that the domain layer never
depends on adk-go, genai, or a provider SDK.

| File | Contents |
|---|---|
| `id.go` | `ID` with wire-compatible prefixes (`agent_`, `env_`, `sesn_`, `sevt_`, `work_`, `vlt_`, `sesrsc_`, `depl_`, `drun_`, `file_`, `skill_`); accepts the alternate `session_` form on input. CSPRNG + Crockford base32 generator. |
| `event.go` | Full `{domain}.{action}` event taxonomy (user / agent / session / span) plus stream-only `event_start` / `event_delta`. Helpers `Domain()`, `Inbound()`, `Persisted()`. `Event` envelope, `StopReason`, `ContentBlock`. |
| `session.go` | `SessionStatus` state machine (`idle` / `running` / `rescheduling` / `terminated`), `Usage`, `Scope` (org/workspace/project), `Session`, `SessionResource`. |
| `agent.go` | `Agent`, `ResolvedAgent`, `AgentSpec`, `Model` (accepts both bare-string and object wire forms). The tools / `mcp_servers` / `skills` unions are kept as raw `[]json.RawMessage` entries so configs round-trip byte-for-byte. `PermissionPolicy`, and `EvaluatedPermission` (`allow` / `ask` / `deny`) — the resolved decision the brain stamps on an `agent.tool_use`; `deny` is reserved: no configurable policy produces it (a denial is expressed as an error result), but the wire enum carries it. |
| `environment.go` | `Environment`, `EnvironmentConfig`, `EnvironmentKind` (`cloud` / `self_hosted`), `Networking` (`unrestricted` / `limited`). |

### internal/api

The wire-compatible control-plane surface: resource CRUD, the events endpoints and their
state-machine triggers, auth, and the work API.

| File | Contents |
|---|---|
| `server.go` | Route table (Go 1.22 method patterns) + `NewHandler`. **Updates are `POST /v1/{resource}/{id}`, not PATCH** (SDK is authoritative). Envelope-shaped 404/405 fallbacks. `?beta=true` and `anthropic-*` headers accepted and ignored. Per-request OTel server span continuing the caller's `traceparent`. |
| `auth.go` | `x-api-key` middleware against `api_keys` (SHA-256 hash only); `EnsureAPIKey` gives **rotation-by-restart** semantics: ensuring a new key under a name revokes the previous ones, so a leaked `CONTROLPLANE_API_KEY` dies on rotation. Authenticated key ID becomes the audit-only `sessions.created_by`. |
| `envauth.go` | Environment-key auth: `EnsureEnvironmentKey` — one live worker credential per environment, hash-only, revoke-others-on-re-mint — plus the `Authorization: Bearer` resolution and session-scope middlewares that confine a worker to its own environment. |
| `errors.go` | Wire error envelope `{"type":"error","request_id":…,"error":{type,message}}` + `request-id` header on every response. Version conflicts are `invalid_request_error` with HTTP 409 (the reference SDK has no dedicated conflict type); oversize bodies (>4 MiB) are 413 `request_too_large`. |
| `page.go` | Cursor pagination: `{"data":[…],"next_page":…}` (+ `prev_page` on sessions), opaque **keyset** cursors via `?page=` — positions on `(created_at, id)` (version number for agent versions), so concurrent writes never duplicate or skip rows — `limit` default 20 / max 100, except the session-events list, whose cap is 1000 (`maxEventLimit`): the reference worker reconciles with `limit=1000`, and a 100-cap 400ed it — the slice-8 acceptance's one bug (HISTORY.md's acceptance record). |
| `wire.go` | Body parsing with omitted/null/value distinction (reference updates are patches), **strict unknown-field rejection** (typos error instead of silently vanishing, matching the reference's extra-inputs behavior), tools/mcp_servers union validation (raw bodies preserved so configs round-trip byte-for-byte; skills are re-normalized to `{type, skill_id, version}`), UTC-normalized timestamps (`Z`, never a local offset). |
| `agents.go` | CRUD + optimistic `version` in the update body (mismatch → 409), immutable `agent_versions` snapshots, `GET ?version=N` pinned reads, versions list, archive (idempotent; **archived resources are read-only** — updates 400). No DELETE — the wire has none. |
| `environments.go` | CRUD incl. update (exists in the SDK though the original plan omitted it) + delete (`environment_deleted`; refused while sessions reference it) + archive; config union normalized strictly (cloud → full networking/packages surface, self_hosted → type only; unknown networking fields rejected); **config updates merge**: omitted cloud sub-fields preserve their stored values per the reference's update semantics — a packages-only patch cannot reset `limited` networking to `unrestricted`; `scope` rendered as the constant `"organization"`; metadata updates delete on empty string as well as null (an environments-only rule in the reference). The `state` column is never rendered — the SDK's `BetaEnvironment` has no `state` field, so it stays internal. |
| `sessions.go` | Create is one transaction (environment `FOR SHARE` + agent resolution + insert, FK-violation backstop) resolving the agent union (id string / `{type:"agent"}` / `agent_with_overrides`, `system:null` clears the prompt, explicit `version` must be ≥ 1) into a full `resolved_agent` snapshot; `session_` accepted for `sesn_`; update limited to title/metadata/`agent.tools`+`agent.mcp_servers` (vault_ids update rejected, matching the reference); list filters (agent_id/agent_version — ignored without agent_id per the reference — statuses[]/order/created_at ranges) + bidirectional keyset cursors; archive/delete (`session_deleted`). |
| `events.go` — endpoints | `POST /v1/sessions/{id}/events` (batch `{"events":[…]}` → `{"data":[…]}` echo with server-assigned ids, `processed_at` null until processed), `GET …/events` (PageCursor envelope `{"data","next_page"}` — no `prev_page` on events — opaque seq-keyset cursor, `types[]` in both spellings, `created_at[gt|gte|lt|lte]`, `order`), `GET …/events/stream` (SSE `event:`+`data:` framing — the reference decoder drops unnamed frames — `ping` keepalive, `?event_deltas[]` opt-in previews filtered per subscriber, live tail from connect time). `DELETE /v1/sessions/{id}` broadcasts an ephemeral `session.deleted` event that terminates active streams. |
| `events.go` — state machine | `POST /events` is one transaction (`FOR UPDATE OF s`): `user.message` on an idle session → running + `session.status_running` + model_turn enqueue; a tool result while running → next model_turn **only when it completes the set** (every tool use answered — the Messages API rejects a partial replay, so parallel tool calls wait for their last result), no status event (awaiting a tool is still `running`); everything else appends only. Tool results are validated against the log before anything commits: an unknown, kind-mismatched, or already-answered reference is a 400. The response echoes only the posted events. Session updates emit `session.updated` with only the changed fields (title / non-empty metadata / agent snapshot), compared semantically — stored jsonb never byte-matches a fresh marshal. |
| `events.go` — confirmations | On a batch of `user.tool_confirmation`s against an `idle` session, `POST /events` appends an `agent.tool_result{is_error:true}` answering each **denied** tool (content = the client's `deny_message`, or a default — never an empty text block), then computes the remaining blocking set: still blocked → re-emit `session.status_idle` with the shrunken `event_ids` (status stays `idle`); fully resolved → flip `running` and enqueue the work that finishes the turn — a `tool_exec` only when an allowed **platform** tool is unanswered (`HasUnansweredPlatformToolUse`); nothing at all when the only remaining work is a client-executed custom tool (the turn waits for the client's result, mirroring the non-ask suspend); a `model_turn` directly only when every tool use is answered (all gated tools denied — the brain resumes on the error results). |
| `workapi.go` | The wire work API handlers: `/work/poll`, `get`/`ack`/`heartbeat`/`stop`, list, stats, and the metadata update, mapping `queue` errors to 404/409/412. |

### internal/events

The event log is the single source of truth for session state.

| File | Contents |
|---|---|
| `log.go` — append/list | `Log.Append` — per-session `seq` allocation under the session row lock (`SELECT … FOR UPDATE`; concurrent appends serialize per session, sessions don't contend), `sevt_` id assignment, `pg_notify` on commit only. `Log.List` — types / `created_at` ranges / seq-keyset / order / limit. Sentinels `ErrSessionNotFound` / `ErrSessionArchived`; stream-only types are unpersistable by construction. |
| `log.go` — atomic side effects | `AppendWith`/`AppendInTx`: session-state side effects under the append's session row lock — `SetStatus` (resource column and status event can never disagree), `AddUsage` (fold a turn into `sessions.usage`), `MarkProcessedThrough` (stamp consumed inbound events), `Then` (join work enqueue to the same commit). `AppendInTx` lets the API decide the batch under the lock. |
| `inbound.go` | `NormalizeInbound` — the POST contract: only the wire's 7 inbound types; field-exact validation (content-block unions per carrier, source unions, `deny_message` only with `result:"deny"`, `user.tool_result` only on `self_hosted` environments, `system.message` at most one / final / immediately after a user payload event); nullable fields normalized to explicit nulls; validated blocks kept as the client's raw bytes so they round-trip byte-for-byte. |
| `broker.go` | Postgres LISTEN/NOTIFY fan-out: one listening connection per process, held only while subscribers exist; wake signals are coalesced pointers ("re-read the log"), so a dropped notification can delay but never lose an event; reconnect re-wakes every subscriber; `Ready` lets the SSE handler snapshot its tail position only after LISTEN coverage is active (no subscribe-window gap). Frames (previews, `session.deleted`) are best-effort broadcast by contract. |
| `preview.go` | `event_start` / `event_delta` preview frames (delta type is literally `content_delta`, **not** the Messages API's `content_block_delta`); `agent.message` streams text fragments, `agent.thinking` is start-only; the preview pre-allocates the buffered event's id for reconciliation; long fragments auto-split at the same index to fit the 8000-byte NOTIFY cap (JSON-escape-aware chunking). Previews are never persisted and never replayed. |
| `span.go` | `StartModelRequest`/`End` — the `span.model_request_start`/`_end` wire events and the OTel client span come from one instrumentation point (design principle 3), linked via `model_request_start_id` and carrying `model_usage`. |
| `toolflow.go` | `ValidateToolConfirmations` rejects a confirmation that does not name an ask-gated, not-yet-confirmed tool use (the append-only-log discipline the tool-result validation already had). `UnconfirmedAskEvents` returns the still-blocking ask ids, treating a validated-but-uninserted batch's confirmations as resolved — the API's input for deciding resume-vs-re-idle. `ToolConfirmationRefs` collects a batch's referenced ids. |
| `metrics.go` | The execution chain's OTel GenAI metrics — turn duration/usage counters with an `error.type` split between model failures and commit failures. |

### internal/brain

The orchestration loop: claim a `model_turn`, replay the log, call the provider, commit
the turn atomically.

| File | Contents |
|---|---|
| `brain.go` — the turn | Claim → replay → generate → **settle the turn atomically**: the emitted events (`agent.message`, tool intents), `span.model_request_end`, the status change, the usage fold, the processed watermark, and the work item's fate are ONE settlement transaction under the session row lock, with the queue's lease proof inside it. (Two things commit mid-stream, outside it: `span.model_request_start` and completed `agent.thinking` events — a brain that dies mid-turn can leave a dangling span start or a duplicate thinking event on the log; replay skips both. The recorded crash-window residue.) That settlement commit is both the liveness guarantee (API triggers serialize on the same lock — a tool result posted mid-settle either sees the live item, suppressed, or the completed one, enqueued; never the gap) and the integrity guarantee (a brain that lost its claim rolls the settlement back — the log never carries a loser's tool intents, which could never all be answered and would poison every future replay). `tool_use` suspends and completes the item; `end_turn` idles unless pending input (a mid-turn `user.message` **or** a suppressed tool result) chains the turn by **requeueing its own item**; failures append `session.error` (`model_request_failed_error`) and idle with `retries_exhausted` — unless input is pending, which chains a fresh turn instead of stranding it. The error's `retry_status` follows that decision, not the code path: `exhausted` (terminal) when the session idles, `retrying` when a chained turn is about to run. Both settlements go through one `settle` helper, so the chain-or-idle contract cannot drift. Errors are classified: provider/model and deterministic input failures fail the turn visibly; brain-side infra failures (database, lost lease) abandon the turn to lease expiry with nothing committed and nothing on the wire. A lease keeper goroutine re-extends the lease at TTL/3 during streaming; each renewal is bounded by the lease it is racing (`TTL − TTL/3`, a duration, so the deadline never depends on the database and this process agreeing on the clock — an `Extend` that outlives the budget has let the lease lapse anyway), and any renewal failure aborts the in-flight stream and abandons the turn at once, with nothing committed. Reclaimed items surface `session.status_rescheduled` + `status_running` with the lease asserted in the same transaction; the staleness check (status/archived, under the session lock) runs first, so a reclaim of finished work never flips an idle session back to running. |
| `brain.go` — intents & gating | `turnEvents` stamps each model-emitted tool intent with the type `classify` resolved (a custom tool → `agent.custom_tool_use`, client-executed, never enters the queue; a built-in → `agent.tool_use`; unknown names default to custom, the safe direction — a client-executed intent never strands a session) and stamps `evaluated_permission` (`allow` or `ask`) on every platform tool use, pre-minting the `sevt_` ids of the `ask` intents so the same ids can name them in the stop_reason. When a turn suspends on platform tools and every intent is `allow`, `commitTurn` enqueues one `tool_exec` item in the same transaction that commits the intents and completes the model_turn item. When any intent is `always_ask`, it gates the **whole** turn: `session.status_idle` carrying `stop_reason:{type:"requires_action", event_ids:[…ask ids]}`, session `idle`, **no** `tool_exec` — even the `always_allow` tools in that turn wait for confirmation. A turn that suspends only on custom tools enqueues nothing (the client answers those). |
| `replay.go` | The log IS the conversation: role-run merging, tool_result blocks sorted ahead of text within a user turn, `tool_use` blocks rebuilt under their **event ids** (the provider-side tool id is discarded at emission; result events reference the event id), string content normalized, `system.message` text appended to the system prompt. `buildRequest` expands each tool entry by type: a `custom` tool becomes a client-executed definition, and an `agent_toolset_20260401` entry is expanded through `toolset.Tools` into the built-in definitions the model sees. One `classify(agent)` pass resolves both maps the brain stamps from: tool name → event type (custom vs platform), and platform tool → resolved permission policy (via `toolset.Policies`; custom tools are client-executed and carry none). `mcp_toolset` expansion waits for the MCP client. |
| `stream.go` | Provider chunks → wire: `agent.message` preview opened at the first **non-empty** text delta (provider block index → content entry index), `agent.thinking` preview per block (start-only, buffered event under the preview id), tool_use collected, the buffered `agent.message` lands **before** `span.model_request_end` (the SDK accumulator closes previews at span end). Empty text deltas are skipped before anything is allocated, so a block that never produces text takes no content index — the preview's delta indices and the stored content array can never disagree. A `tool_use` whose input is not a JSON object (truncated, or a bare string/array/number) fails the turn visibly rather than reaching the append-only log. Database failures mid-stream are marked infra — never reported as model failures. |

### internal/provider

Config-driven model access (design principle 4).

| File | Contents |
|---|---|
| `provider.go` | `Config` (`protocol` / `model` / `base_url` / `api_key` / optional headers), `Request`/`Message` in Anthropic Messages semantics with content blocks and tool definitions as **raw wire JSON** (the Anthropic adapter is near-zero-conversion; lossy mapping stays confined to the non-Anthropic adapters), `Chunk` stream (`text_delta` / `thinking_delta` / complete `tool_use` after input accumulation / `done` with `stop_reason` + `domain.ModelUsage`), `Provider`/`Stream`/`Factory` interfaces, and the model→provider `Registry` (exact match + `"*"` default; a route without an upstream `model` passes the agent's model string through to the endpoint). |
| `anthropic/anthropic.go` | The Anthropic-protocol adapter over the official SDK: `base_url` is **required** (no silent api.anthropic.com fallback), extra headers pass through for gateway routing, streaming events translate to chunks (tool_use inputs accumulate from `input_json_delta`, `message_delta` carries stop reason + output usage). Retry policy is the SDK's default — no override. |
| `openai/openai.go` | The OpenAI-compatible adapter (OpenAI, vLLM, or an internal gateway) — the platform's **lossy seam**, confined here. Anthropic-native turns translate to Chat Completions on the way out (system prepended; text→content; assistant `tool_use`→`tool_calls` with object input→JSON-string arguments; user `tool_result`→`tool` role messages; tools→function tools) and the stream back (delta.content→`text_delta`, accumulated `tool_calls`→`tool_use`, usage→`ModelUsage`); **`stop_reason` is `tool_use` whenever the stream carried any tool call** (not read from `finish_reason`, which some servers set to `stop`/`length` on a tool turn — honoring that would strand the tool the brain never runs). `base_url` is required and is the API root (adapter appends `/v1/chat/completions`); `[DONE]` completes a turn, a body ending with neither `finish_reason` nor `[DONE]` (or a mid-stream error frame) fails loudly. Known lossy gaps, documented not silent: thinking blocks dropped, image blocks (top-level or inside a `tool_result`) fail loudly, and a tool_result's `is_error` boolean dropped (the error text in the result content is still forwarded). |
| `config.go` | `LoadRoutes` reads the `model_providers` JSON file (`model` / `protocol` / `base_url` / `upstream_model` / `api_key` or `api_key_env`; unknown keys rejected). |

There is no `internal/mcp` (reserved in early sketches; no MCP client is built — the
brain's replay treats `mcp_toolset` as awaiting it) and no `internal/policy` (permission
policy lives across `domain` / `toolset` / `brain` / `api` — see those rows).

### internal/sandbox

The hands: the sandbox boundary, its Docker and Kubernetes backends, the shared contract
suite, and the persistent shell.

| File | Contents |
|---|---|
| `sandbox.go` | The boundary: `Spec` (session / image / workdir / networking), `ExecRequest` (command + timeout), `ExecResult` (`Stdout`/`Stderr`/`ExitCode`/`TimedOut`/`Truncated`), `Sandbox` (`ID`/`Exec`/`ReadFile`/`WriteFile`/`Destroy`), `Provider` (`Provision`). Sentinels `ErrNotFound` / `ErrFileNotExist` / `ErrIsDirectory` / `ErrNotRegularFile` / `ErrFileTooLarge`; the shared `DefaultWorkdir` (`/workspace`) both backends and the toolset root relative paths against; caps `MaxOutputBytes` (1 MiB per stream, drained past the cap) and `MaxFileBytes` (4 MiB, refused rather than truncated — the sandbox filesystem is agent-controlled, so a read is an untrusted-length allocation). |
| `backend/backend.go` | Selects the sandbox provider by name (`SANDBOX_BACKEND`: `docker` or `k8s`), so `cmd/executor`/`cmd/worker` construct their "hands" from one config point. |
| `docker/api.go` | ~10 Engine API endpoints over `net/http` on the daemon socket (`DOCKER_HOST`, `unix://` or `tcp://`), the exec stream's frame demultiplexer, and `processAlive` (the `top` endpoint: is this pid still in the container?). Hand-rolled: the official client would pull the whole moby module tree in for this. |
| `docker/docker.go` | `Provision` (inspect → create → pull-on-missing-image → adopt-on-409 → start; adopts only a container carrying this session's ownership label), `Exec` (the command `exec`s to become the exec's own process, so there is no wrapper pid to kill; an in-container watchdog kills at the deadline, but `Exec` stops waiting on its own clock and decides the timeout from two out-of-container probes of that process), tar-based file transfer with parent creation on the archive endpoint's 404, `Destroy` (idempotent). |
| `k8s/k8s.go` | The Kubernetes `sandbox.Provider`: pod-per-session `Provision` with get-or-create-then-wait-ready and UID-guarded unready-pod reclaim. `Destroy` deletes with a zero grace period so the sandbox is final at the API-object level at once (a force delete does not block on the kubelet, so a partitioned node could lag, but the handle is dead either way). |
| `k8s/client.go` | client-go construction and the pod-exec transport; documents the image contract (`/bin/bash`, `setsid`, `tee`/`wc` for the write path's delivered-byte count, and a `stat` accepting `-c`). |
| `k8s/deadline.go` | The in-pod exec wrapper and pid/exit-file scripts adapting the Docker deadline discipline to Kubernetes' stream-coupled exec; the watchdog marks its own kill on the exit line, which is what classifies a punctual timeout here. |
| `k8s/probe.go` | The two-instant deadline probes (alive-at-deadline / overran-plus-slop) answered by a second exec reading the pid file; sticky overrun verdict. Reach around the watchdog's mark, not the primary evidence — the probe answers an apiserver round trip late. |
| `sandboxtest/contract.go` | The one suite every backend must pass: stream capture, exit codes, workdir, timeout-kills-and-survives, a command that kills the deadline's guards and then either runs long or exits clean, a command timed by its own life rather than by a straggler holding its output, output cap, binary file round-trip with parent creation, a megabyte round trip spanning many stream buffers, missing/at-the-cap/oversize/directory/non-regular (FIFO) reads, shared filesystem between `Exec` and the file primitives, idempotent provision, final-and-idempotent destroy, and networking isolation. |
| `shell/shell.go` | `Run(ctx, sb, session, id, Request)` — one `bash` tool call. `Request{Command, Restart, Timeout}`, `Result` mirrors `ExecResult` plus `Restarted`. It writes the command to a file, mints a directory for this call's snapshot, `Exec`s the embedded template, and then — only if the call did **not** time out **and** left a complete snapshot (its `done` marker) — points `head` at that directory. The snapshot directory is minted per *call*, not named after the tool id: the executor may retry a tool call under the id it already used, and a retry must not inherit the previous attempt's files, least of all its marker. Restart empties `head` through the sandbox's file API (not a container `rm`, which resolves against the container PATH — a prior call that shadowed `rm` on disk would make the reset exit 0 and reset nothing); its write error, not a command's exit code, gates it — a reset that did not run is not a reset. |
| `shell/template.sh` | The only bash the tool introduces, embedded once as data. Go substitutes the (quoted) state dir, tool id and snapshot dir; the user's command is **not** here — it is delivered as a file and `source`d, so no command bytes ride the argument or a sentinel. A prologue restores the committed snapshot (errexit forced off first, options applied last); the shell is then snapshotted into *this call's own* directory, preserving the command's own exit status, and the save ends by creating the `done` marker that makes the snapshot committable. |

### internal/queue

The internal work queue over the `work_items` table.

| File | Contents |
|---|---|
| `queue.go` | `FOR UPDATE SKIP LOCKED` over `work_items`. `Enqueue` is idempotent per (session, kind) while a live item exists (partial unique index, migration `0003`); `Claim` leases the oldest queued item and reclaims expired-active ones (flagged so the brain surfaces recovery); `Extend`/`Complete`/`Requeue` carry the lease expiry as an ownership proof (the reference work API's `expected_last_heartbeat` shape) — a claimant that lost its lease gets `ErrLeaseLost`, never silently finishes a reassigned item. `Poll` is the soft-reservation `self_hosted` poll with dead-worker reclaim. |
| `lifecycle.go` | The wire work-item state machine: `GetWork`/`ListWork`/`Ack`/`Heartbeat`/`Stop`/`UpdateMetadata` with `ErrWorkNotFound`/`ErrWorkConflict`/`ErrHeartbeatMismatch`. `UpdateMetadata` merges its null-deletes/string-upserts patch atomically in SQL, with nil-slice guards forcing empties (a nil `deletes` would encode as SQL `NULL`, and `jsonb − NULL` is `NULL`, nulling the `NOT NULL` column; a nil `upserts` map would marshal to JSON `null`, and `jsonb \|\| 'null'` makes an array). |
| `stats.go` | The queue-stats derived view: `depth`/`pending`/`oldest_queued_at` from the `work_items` snapshot plus `workers_polling` from the `worker_polls` poll-time record. |

### internal/toolset

The built-in tools — what an `agent_toolset_20260401` entry enables.

| File | Contents |
|---|---|
| `toolset.go` | `Runner` (sandbox + session + workdir) and `Run(ctx, id, name, input)`. The one line the package draws: a **tool** failure (missing file, bad regex, nonzero exit, a non-regular target, a NUL in a path) is a `Result` with `IsError` — the model reads it and recovers — while a **backend** failure (sandbox gone, daemon unreachable) is an `error` and never a result the model would try to reason about. Tool output is capped at `MaxOutputBytes` (100 KiB on a rune boundary, the reference's limit — a truncated result carries an `[output truncated]` marker just past it) because the tool result goes on the event log forever; `capWithTrailer` caps bash's output *with* its exit-code/timeout line so the load-bearing "did it fail" signal survives a huge output rather than being lopped off the end. `singleQuote` is what makes a model-supplied path or pattern reach bash as data rather than code, and `badField` rejects a NUL byte in a path or pattern before it can reach the sandbox as a broken tar header. |
| `definitions.go` | `Tools(entry)` → the model-facing definitions an `agent_toolset_20260401` entry enables, in the wire's order — schemas are the wire's field for field (SDK `BetaManagedAgentsAgentToolset20260401{Bash,Read,Write,Edit,Glob,Grep}Input`) — and `Policies(entry)` → each enabled tool's resolved `permission_policy`, keyed by name. A shared `resolveToolset` backs both, so enable resolution (per-tool config > `default_config` > on) and policy resolution (same precedence, default `DefaultAgentToolsetPolicy`) can never disagree about which tools exist. An unknown `permission_policy.type` on an enabled tool is a hard error — a policy the platform cannot evaluate never silently resolves to "run it anyway". |
| `bash.go` | The shell package's persistent session, plus the wire's `restart` / `timeout_ms`. A nonzero exit is an error result carrying the code; a timeout is an error result carrying **no** code (`TimedOut` is the sandbox's authoritative field) and saying that the call's shell state was dropped. |
| `file.go` | `read` / `write` / `edit` over the sandbox's file primitives — no shell, so a command that shadowed `cat` or broke `PATH` cannot reach them. `edit`'s unique-match rule, `read`'s 1-indexed inclusive `view_range` (end ≤ 0 means EOF), and their messages are the reference's. Line ranges stay `int64` end to end: a range the model invented must not overflow an index on the 32-bit build CI cross-compiles. `fileFault` classifies the sandbox's file sentinels — not-found, is-a-directory, not-a-regular-file (a FIFO/device/socket read), and too-large — as tool errors the model can act on; anything else is the sandbox failing and stays an `error`. |
| `search.go` | `glob` and `grep` as bash scripts in the container. glob expands the pattern with bash's own **globstar** — which is where doublestar semantics already live (`**` spans directories, `*` does not cross a separator, dotglob makes a leading dot ordinary) — then stamps mtimes and sorts newest first, capped at 200. The whole glob pipeline is **NUL-delimited** (`stat --printf … \0` / `sort -z`), so a matched filename that itself contains a newline stays one record and cannot inject a fabricated path; `pipefail` is on so a broken pipeline (an image whose `stat` lacks `--printf`, a missing tool the up-front `command -v` guard also names) is a reported error rather than a silent "no matches". grep uses the image's GNU grep, PCRE where it has it (a model writes `\d` far more readily than `[0-9]`), ERE where it does not; exit 1 is the answer "no matches", not a failure. |
| `telemetry.go` | The tool-run duration metric — deliberately the platform's own metric name, not a `gen_ai.*` one. |

### internal/executor

The `tool_exec` consumer — the platform-managed half of the pull protocol.

| File | Contents |
|---|---|
| `executor.go` | `Run` polls the queue; `step` claims the oldest `tool_exec` item (reclaiming an expired lease) and `process` runs it: load the session's egress policy, provision the sandbox (idempotent per session), gather the session's **unanswered** tool uses, run them, and commit the results, the resume, and the item's fate together. The append's `Then` — under the session row lock — enqueues a `model_turn` **only when every tool use is answered** (`events.HasUnansweredToolUse`, the same full-set gate the control plane uses for client results), and completes the item only when every tool actually ran. |
| `toolwork.go` | `unansweredToolUses` diffs the session's `agent.tool_use` events against the answering `agent.tool_result` / `user.tool_result` events already on the log, oldest first — the work this item must run, and the reclaim ledger. The lease keeper (`keepLease`) renews the claim at TTL/3 while tools run and cancels the work context if it ever loses the lease, mirroring the brain's; the renewed lease is the proof the settling commit uses. |

### internal/worker

The BYOC worker — the customer-hosted twin of `internal/executor`, wire-only, no
database.

| File | Contents |
|---|---|
| `client.go` | The SDK client a worker authenticates to the control plane with — environment key as `Authorization: Bearer`. |
| `lease.go` | The lease loop: poll with `block_ms`/`Anthropic-Worker-ID`, ack, session-liveness gate, `traceparent` extraction from the poll response headers. A finished item is force-stopped only while this worker still exclusively owns the lease, and a dead session's item is force-stopped unconditionally (nothing live to disrupt); a fault or an observed lease loss leaves the item live for the queue's reclaim instead — force-stop is terminal, and no reclaim recovers a stopped item. The heartbeat goroutine starts **before** the liveness check and the run — the reference's ordering, because every moment between the ack and the first heartbeat is a window the control plane sees no liveness signal in; the cadence is derived from the response TTL (`ttl/2`, clamped `[1s, 30s]`), and `desired_ttl_seconds` is not sent, per the reference. |
| `toolexec.go` | `RunSessionTools`, the tool-exec driver: diff unanswered `agent.tool_use` over the wire, run each in a local sandbox via the shared `toolset.Runner`, post `user.tool_result` per tool as each completes. |

Two cross-package notes travel with the worker. The "answered" diff — an
`agent.tool_result` **or** `user.tool_result` answers an `agent.tool_use` — is expressed
three times against three data sources (`events.HasUnansweredToolUse`, the canonical SQL;
`executor.unansweredToolUses`, DB; `worker.unansweredToolUses`, wire), tied together by a
code comment so a new answering type is added to all. And the control plane's 400-refusal
on bad or duplicate result references is only a *partial* backstop for a misbehaving
worker — a post to a session that is merely not running (not archived) appends without
resuming.

### internal/telemetry

OTel init + W3C trace-context propagation.

| File | Contents |
|---|---|
| `telemetry.go` | `Config` (`ServiceName` / `Endpoint` / `Insecure` / `SampleRatio`) + `Init`: installs the global W3C propagator; with an endpoint configured, installs OTLP/gRPC-exporting tracer + meter + logger providers (`ParentBased(TraceIDRatioBased)` sampler, `service.name` resource). Empty endpoint = fully offline no-op. Returns a flush-at-exit shutdown func. |
| `logs.go` | The slog → OTLP bridge. A configured endpoint points `slog.SetDefault` at a fan-out handler: a `TextHandler` on the console, plus `otelslog` on the logger provider. The fan-out's `Enabled` answers for itself at `bridgeLevel` (Info — the floor slog's own default handler already had) rather than asking its branches, because the OTLP branch has no floor to ask about: `otelslog`'s `Enabled` delegates to `sdk/log`'s `BatchProcessor.Enabled`, which is unconditionally true. So adding an endpoint changes where records go, never which records exist. |
| `propagation.go` | `Inject` / `Extract` — W3C `traceparent`/`tracestate` over any `map[string]string` carrier (HTTP request/response headers and a work item's stored trace context both flatten to this shape). Fixed propagator, works without `Init`. |

### internal/store

Postgres schema + migrations.

| File | Contents |
|---|---|
| `migrations/0001_init.sql` | Core schema: `agents` + `agent_versions` (optimistic `version`, immutable snapshots), `environments` (kind CHECK `cloud/self_hosted`, config required with a CHECK forcing `config->>'type' = kind` — the wire keeps the discriminator inside the config union), `sessions` (resolved-agent jsonb snapshot, status CHECK, composite FK `(agent_id, agent_version) → agent_versions` so the audit trail can't dangle, `vault_ids` seam, audit-only `created_by`, **no user_id**), `events` (append-only log, `UNIQUE (session_id, seq)`), `work_items` (`state` CHECK matches the wire enum; `kind` CHECK `model_turn/tool_exec` is the **internal** queue taxonomy, not a wire field; lease + heartbeat columns; poll + session indexes), `api_keys` / `environment_keys` (hash only, `revoked_at` rotation). Wire-required plain strings (`sessions.title`, `environments.description`) are `NOT NULL DEFAULT ''`. Every top-level table reserves `org_id`/`workspace_id`/`project_id` (default `'default'`). |
| `migrations/0002`–`0006` | Follow-on DDL, one concern each: session `archived_at` (0002), the per-(session, kind) live-item partial unique index behind `Enqueue`'s idempotency (0003), the four work-item lifecycle-timestamp columns (0004), the `trace_context` jsonb column that carries `traceparent` through work items (0005), the `worker_polls` table behind `workers_polling` (0006). Migrations are immutable once merged; new DDL goes in a new numbered file. |
| `migrate.go` | `Migrate`: embedded-FS migrations in filename order, one transaction for the whole run (all-or-nothing), `pg_advisory_xact_lock` so concurrently starting binaries don't race, versions recorded in `schema_migrations`. |
| `store.go` | `Open(ctx, dsn)`: pool + ping + migrate; the single startup entry point for every database-backed binary (the BYOC worker is deliberately database-free). |

### Test support and cmd/

`internal/pgtest` starts one Dockerized Postgres per test binary and hands out
freshly-migrated databases (a missing Docker daemon is a hard failure, never a skip).
`internal/modeltest` owns the live-tier opt-in contract: `.env` supplies configuration,
`RUN_LIVE_MODEL_TESTS`/`RUN_EVALS` supply consent, and opted-in-but-misconfigured fails
rather than skips (`TierEnabled` serves `TestMain` callers). Both are deliberately
outside the coverage-gate denominator, as is `cmd/`.

The four `cmd/` binaries are env-config plus wiring: `controlplane` (serves the REST API;
`CONTROLPLANE_ADDR` / `DATABASE_URL` / `CONTROLPLANE_API_KEY` + OTel), `brain`
(`DATABASE_URL` + `MODEL_PROVIDERS_PATH` + lease/poll tunables + OTel), `executor`
(`DATABASE_URL` + `EXECUTOR_IMAGE`/`EXECUTOR_WORKDIR` + `SANDBOX_BACKEND` selection —
`docker` default, `k8s` — via `internal/sandbox/backend` + OTel), and `worker`
(`ANTHROPIC_BASE_URL` / `ANTHROPIC_ENVIRONMENT_ID` / `ANTHROPIC_ENVIRONMENT_KEY`
required; no `DATABASE_URL` by design).

`deploy/helm/managed-agent-platform` is the chart (controlplane + brain + executor with
the k8s sandbox backend, optional inline Postgres, OTLP values);
`deploy/compose/docker-compose.yml` is the local stack (one multi-stage image for all
four binaries, loopback-bound control plane, optional Jaeger profile).

## Security invariants

- **Credentials never enter the sandbox.** Tool credentials are a reserved egress-time
  seam (vaults, deferred); model keys live in the brain's provider config; the sandbox
  sees none of them. Provider errors and config printouts redact the key under every
  `fmt` verb that can be intercepted.
- **A session is not a context window.** The harness may replay, slice, or rewind the
  event log before feeding the model; context strategy is never baked into an
  irreversible compaction.
- **Auth is scoped.** Management calls carry `x-api-key` (hashed at rest,
  rotation-by-restart); workers carry an environment key scoped to exactly one
  environment's work queue — a worker can neither read nor write another environment's
  sessions.
- **Sessions are not bound to an end-user.** Scoping keys are org/workspace/project
  (reserved, single-tenant defaults in v1); end-user ownership is an application-layer
  concern hooked on session `metadata` and the audit-only `created_by`.
- **The container is the boundary.** Tools run inside the per-session sandbox with no
  host filesystem access; the toolset does no lexical path confinement that a `bash`
  call could walk around, because the container itself is the wall.

## Observability

OpenTelemetry is built in, not bolted on. Trace context propagates as W3C `traceparent`
across every process hop that continues a trace — HTTP headers between processes, and a
`trace_context` column that carries a turn's trace through `tool_exec` work items to
executors and BYOC workers (a `model_turn` item deliberately stores none: nothing reads
it back) — so one session's turn is one trace across process boundaries. Anthropic
`span.*` wire events and OTel spans are emitted from the **same instrumentation point**
(they cannot drift); the metrics ride the same points (model-request duration and
token-usage counters in `internal/events/metrics.go`, tool-run duration in
`internal/toolset/telemetry.go`; queue depth is not a metric — it is an on-demand
derived view served by the work-stats endpoint), and a configured OTLP endpoint bridges
`slog` records — trace-correlated where a span is in reach — to the collector.
`internal/telemetry` owns init and propagation; an empty endpoint is a fully-offline
no-op.

## Testing architecture

Four tiers (see README's table for the opt-in contract): unit/contract tests and
dependency-integration tests run on every PR and call no model — the store, API, queue,
sandbox, and toolset suites run against real Postgres and real Docker/Kubernetes, and a
missing daemon or cluster is a hard failure, not a skip. The live tiers are consented by
environment variable (`RUN_LIVE_MODEL_TESTS`, `RUN_EVALS`), configured by the gitignored
`.env`, and fail rather than skip when misconfigured (`internal/modeltest` owns the
contract).

Backend variability lives behind interfaces, and where more than one backend exists the
contract is a **shared suite**: every sandbox provider passes
`internal/sandbox/sandboxtest` — a new backend inherits the whole battery. The queue and
the model providers each have one production implementation contract-tested in their own
packages today (Postgres; the two protocol adapters) — a shared provider contract suite
is tracked as [#48](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/48),
and a second queue backend would owe the same extraction. The merge gate is `make verify`
(build, linux/arm cross-compile, vet, gofmt, `go test -count=1`, and **≥90% total
statement coverage** over the logic packages of `./internal/...`). On top sits the eval
system (`make eval`, [plan 02](./plan/02_evals-system.md)): ten deterministic regression
tasks driving whole sessions through the public API against a real model, graded
code-only with per-trial nonces and Platform/Model/Either failure classing.
