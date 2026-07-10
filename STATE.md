# STATE.md — Development Progress

Running record of where this project actually stands, so work can resume cleanly across sessions.

**Keep it honest.** A slice is only "done" when its code builds, its tests pass, and its behavior has been verified by the independent **`verifier` subagent** (`.claude/agents/verifier.md`; protocol in CLAUDE.md → "Independent verification"). Update this file whenever a slice changes status.

---

## Snapshot

- **Last updated:** 2026-07-10
- **Phase:** The platform converses — the brain orchestration loop is live: a `user.message` sent by the real `ant` CLI flips the session to running, a brain claims the turn, replays the log against the configured model endpoint, streams previews over SSE, and idles the session with `end_turn`. Custom tools round-trip as `agent.custom_tool_use` / `user.custom_tool_result`. Tool EXECUTION (sandbox), permissions, and the wire work API are still ahead.
- **Current slice:** 5 complete; next up is slice 6 (tool-exec executor + Docker sandbox).
- **Build status:** `go build ./...`, `go vet ./...`, `go test ./...` all green.

## Reference documents

- **Approved design plan:** `~/.claude/plans/agent-managed-agent-encapsulated-moonbeam.md` — read before any large change.
- **[CLAUDE.md](./CLAUDE.md)** — architecture, non-negotiable design principles, working conventions.
- **Local reference checkouts** (paths + authority order in CLAUDE.md → "Reference source checkouts"): `anthropic-sdk-go` (typed wire schema — `betasessionevent.go` covers the full event taxonomy), `anthropic-cli` (real `ant` client behavior), `claude-code-source` (harness design reference only).

## v1 goal

Ship a platform an enterprise can `helm install` into its own Kubernetes, exposing a REST API wire-compatible with Anthropic Claude Managed Agents, that completes this loop:

> create agent → create environment → create session → send `user.message` → brain calls the model → brain emits `agent.tool_use` → an executor runs the tool in a sandbox → `agent.tool_result` → events stream back over SSE → an `always_ask` tool takes one human approval → `session.status_idle`

The model backend must be pointable at either an Anthropic-protocol endpoint or an internal OpenAI-compatible gateway.

---

## Delivery slices

| # | Slice | Status |
|---|---|---|
| 0 | `internal/domain` (Anthropic-native types) + `internal/telemetry` (OTel/OTLP, context propagation) | ✅ Done |
| 1 | Postgres schema + migrations (`internal/store`), reserved multi-tenant columns | ✅ Done |
| 2 | Control plane CRUD (agents / environments / sessions) + optimistic versioning + ID prefixes + `x-api-key` auth | ✅ Done |
| 3 | Append-only event log (seq allocation) + `POST /events` + SSE stream (`event_start` / `event_delta` reconciliation) + `span.*` emitted from the same point as OTel spans | ✅ Done |
| 4 | `ModelProvider` (config-driven: protocol / model / base_url / api_key) + `model_providers` routing; first provider passing a single model turn; verify a custom `base_url` works | ✅ Done |
| 5 | Brain orchestration loop (replay → assemble provider request → write Anthropic-native events). No adk runtime. | ✅ Done |
| 6 | tool-exec queue (Postgres `FOR UPDATE SKIP LOCKED`) + executor + Docker sandbox provider + built-in toolset really executing inside the sandbox | ⬜ Not started |
| 7 | Permission policies + `requires_action` / `user.tool_confirmation` approval round-trip | ⬜ Not started |
| 8 | Wire-compatible work API (`/work/poll`, `/ack`, `/heartbeat`, `/stop`) + distributable BYOC worker + `traceparent` propagated through work items | ⬜ Not started |
| 9 | Kubernetes sandbox provider + Helm chart (with OTLP endpoint values) | ⬜ Not started |

---

## Completed

### Repository & tooling
- `git init` on branch `main`; initial commit `9a1ca75`.
- Apache-2.0 `LICENSE` (canonical text fetched from apache.org, not hand-written).
- `.gitignore` for Go (build output, coverage, `go.work`, `.env`/secrets, editor/OS files, `.impeccable/` tool cache).
- `README.md` — public-facing, states "early development" honestly.
- `CLAUDE.md` — architecture, 5 non-negotiable design principles, wire-compat rules, working conventions.
- `.claude/agents/verifier.md` — independent verifier subagent; every slice must pass it before being marked done. Local reference checkouts (SDK / `ant` CLI / Claude Code source) documented in CLAUDE.md as wire-schema ground truth.
- CI (`.github/workflows/ci.yml`: `ci` job = build / 32-bit cross-compile / vet / gofmt; `coverage` job = `test -count=1` + total statement coverage ≥90% over `./internal/...` as its own named GitHub check, with a per-package job summary and the profile as an artifact) + branch→review→PR→CI→squash-merge iteration workflow with dual code review (`/codex:review` + `/code-review`) (CLAUDE.md → "Iteration workflow").
- Automated PR reviewers: `.coderabbit.yaml` (CodeRabbit config — wire-compat, migration-immutability, and test-quality instructions) and `AGENTS.md` (ground rules for Codex and other AI agents, pointing at CLAUDE.md). The CodeRabbit and Codex GitHub Apps themselves are installed at the GitHub-account level, not in-repo.
- Docs-consistency rule: STATE.md, README.md, and CHANGELOG.md move with code in the same PR; the verifier checks them as rung 6 of its ladder. CHANGELOG.md follows Keep-a-Changelog, everything under Unreleased until a first release.

### `internal/domain` — Anthropic-native core types
Zero external dependencies (stdlib only), enforcing the rule that the domain layer never depends on adk-go, genai, or a provider SDK.

| File | Contents |
|---|---|
| `id.go` | `ID` with wire-compatible prefixes (`agent_`, `env_`, `sesn_`, `sevt_`, `work_`, `vlt_`, `sesrsc_`, `depl_`, `drun_`, `file_`, `skill_`); accepts the alternate `session_` form on input. CSPRNG + Crockford base32 generator. |
| `event.go` | Full `{domain}.{action}` event taxonomy (user / agent / session / span) plus stream-only `event_start` / `event_delta`. Helpers `Domain()`, `Inbound()`, `Persisted()`. `Event` envelope, `StopReason`, `ContentBlock`. |
| `session.go` | `SessionStatus` state machine (`idle` / `running` / `rescheduling` / `terminated`), `Usage`, `Scope` (org/workspace/project), `Session`, `SessionResource`. |
| `agent.go` | `Agent`, `ResolvedAgent`, `AgentSpec`, `Model` (accepts both bare-string and object wire forms), tools union, `MCPServer`, `Skill`, `PermissionPolicy`. |
| `environment.go` | `Environment`, `EnvironmentConfig`, `EnvironmentKind` (`cloud` / `self_hosted`), `Networking` (`unrestricted` / `limited`). |

**Test coverage so far:** ID prefixes / uniqueness / token format; event domain, direction, and persistence classification; `Model` dual-form JSON round-trip. `session.go` and `environment.go` are plain types with no dedicated tests yet — they will be covered by the store and API round-trip tests in slices 1–3.

### `internal/telemetry` — OTel init + W3C trace-context propagation

Uses `go.opentelemetry.io/otel` (+ OTLP/gRPC exporters) — the first external dependency in the module.

| File | Contents |
|---|---|
| `telemetry.go` | `Config` (`ServiceName` / `Endpoint` / `Insecure` / `SampleRatio`) + `Init`: installs the global W3C propagator; with an endpoint configured, installs OTLP/gRPC-exporting tracer + meter providers (`ParentBased(TraceIDRatioBased)` sampler, `service.name` resource). Empty endpoint = fully offline no-op. Returns a flush-at-exit shutdown func. |
| `propagation.go` | `Inject` / `Extract` — W3C `traceparent`/`tracestate` over any `map[string]string` carrier (HTTP headers and work-item metadata both flatten to this shape). Fixed propagator, works without `Init`. |

**Test coverage:** contract tests drive an in-process fake OTLP/gRPC collector and assert what actually leaves the process: exported span names, `service.name` resource attribute, exported metrics, and that `SampleRatio` is honored. Plus traceparent inject/extract round-trip (IDs, sampled flag, remote flag, tracestate) and config validation. `span.*` domain-event emission from these spans lands in slice 3.

### `internal/store` — Postgres schema + migrations

Uses `github.com/jackc/pgx/v5` (pool + wire protocol). No ORM, no migration library — migrations are embedded SQL applied by a ~60-line migrator.

| File | Contents |
|---|---|
| `migrations/0001_init.sql` | Core schema: `agents` + `agent_versions` (optimistic `version`, immutable snapshots), `environments` (kind CHECK `cloud/self_hosted`, config required with a CHECK forcing `config->>'type' = kind` — the wire keeps the discriminator inside the config union), `sessions` (resolved-agent jsonb snapshot, status CHECK, composite FK `(agent_id, agent_version) → agent_versions` so the audit trail can't dangle, `vault_ids` seam, audit-only `created_by`, **no user_id**), `events` (append-only log, `UNIQUE (session_id, seq)`), `work_items` (`state` CHECK matches the wire enum; `kind` CHECK `model_turn/tool_exec` is the plan's **internal** queue taxonomy, not a wire field; lease + heartbeat columns; poll + session indexes), `api_keys` / `environment_keys` (hash only, `revoked_at` rotation). Wire-required plain strings (`sessions.title`, `environments.description`) are `NOT NULL DEFAULT ''`. Every top-level table reserves `org_id`/`workspace_id`/`project_id` (default `'default'`). |
| `migrate.go` | `Migrate`: embedded-FS migrations in filename order, one transaction for the whole run (all-or-nothing), `pg_advisory_xact_lock` so concurrently starting binaries don't race, versions recorded in `schema_migrations`. |
| `store.go` | `Open(ctx, dsn)`: pool + ping + migrate; the single startup entry point for every binary. |

**Test coverage:** contract tests run against a real Postgres started in Docker by `TestMain` (`postgres:16-alpine`, random port, fresh database per test): fresh-migrate creates every table, idempotent re-run, 4 concurrent `Open`s don't conflict, `(session_id, seq)` uniqueness (and same seq OK across sessions), enum CHECKs reject invalid values and accept **every** valid value (all 4 session statuses, all 5 work states, both kinds/environment kinds), kind/config disagreement rejected, config required, dangling `agent_version` rejected, `title`/`description` scan into plain strings, `work_items(session_id)` index present, tenancy defaults, migration failures roll back atomically (conflicting object, broken/variant `schema_migrations`), unreachable/malformed DSN.

**Wire-drift note (recorded 2026-07-10, resolved by slice 2):** the SDK checkout's `BetaEnvironment` has no `state` field — the API layer never renders the `state` column (it stays internal). Session `archived_at` is real wire surface → added by migration `0002_session_archive.sql`. Session `stats` / `outcome_evaluations` / `deployment_id` are rendered as their empty/null wire shapes (no storage yet). Environment `scope` is accepted only as `"organization"`; `"account"` is rejected (single-tenant v1).

### `internal/api` + `cmd/controlplane` — wire-compatible control-plane CRUD

Slice 2. The real `ant` CLI (built from the local checkout, v1.16.0) drives every endpoint against `cmd/controlplane` unchanged — verified live: agents create/update/optimistic-409/versions, environment defaults, session snapshot resolution.

| File | Contents |
|---|---|
| `server.go` | Route table (Go 1.22 method patterns) + `NewHandler`. **Updates are `POST /v1/{resource}/{id}`, not PATCH** (SDK is authoritative; the plan doc predates this). Envelope-shaped 404/405 fallbacks. `?beta=true` and `anthropic-*` headers accepted and ignored. Per-request OTel server span continuing the caller's `traceparent` (CLAUDE.md principle 3). |
| `auth.go` | `x-api-key` middleware against `api_keys` (SHA-256 hash only); `EnsureAPIKey` gives **rotation-by-restart** semantics: ensuring a new key under a name revokes the previous ones, so a leaked `CONTROLPLANE_API_KEY` dies on rotation. Authenticated key ID becomes the audit-only `sessions.created_by`. |
| `errors.go` | Wire error envelope `{"type":"error","request_id":…,"error":{type,message}}` + `request-id` header on every response. Version conflicts are `invalid_request_error` with HTTP 409 (the reference SDK has no dedicated conflict type); oversize bodies (>4 MiB) are 413 `request_too_large`. |
| `page.go` | Cursor pagination: `{"data":[…],"next_page":…}` (+ `prev_page` on sessions), opaque **keyset** cursors via `?page=` — positions on `(created_at, id)` (version number for agent versions), so concurrent writes never duplicate or skip rows — `limit` default 20 / max 100. |
| `wire.go` | Body parsing with omitted/null/value distinction (reference updates are patches), **strict unknown-field rejection** (typos error instead of silently vanishing, matching the reference's extra-inputs behavior), tools/mcp_servers union validation (raw bodies preserved so configs round-trip byte-for-byte; skills are re-normalized to `{type, skill_id, version}`), UTC-normalized timestamps (`Z`, never a local offset). |
| `agents.go` | CRUD + optimistic `version` in the update body (mismatch → 409), immutable `agent_versions` snapshots, `GET ?version=N` pinned reads, versions list, archive (idempotent; **archived resources are read-only** — updates 400). No DELETE — the wire has none. |
| `environments.go` | CRUD incl. update (exists in the SDK though the plan omitted it) + delete (`environment_deleted`; refused while sessions reference it) + archive; config union normalized strictly (cloud → full networking/packages surface, self_hosted → type only; unknown networking fields rejected); **config updates merge**: omitted cloud sub-fields preserve their stored values per the reference's update semantics — a packages-only patch cannot reset `limited` networking to `unrestricted`; `scope` rendered as the constant `"organization"`; metadata updates delete on empty string as well as null (an environments-only rule in the reference). |
| `sessions.go` | Create is one transaction (environment `FOR SHARE` + agent resolution + insert, FK-violation backstop) resolving the agent union (id string / `{type:"agent"}` / `agent_with_overrides`, `system:null` clears the prompt, explicit `version` must be ≥ 1) into a full `resolved_agent` snapshot; `session_` accepted for `sesn_`; update limited to title/metadata/`agent.tools`+`agent.mcp_servers` (vault_ids update rejected, matching the reference); list filters (agent_id/agent_version — ignored without agent_id per the reference — statuses[]/order/created_at ranges) + bidirectional keyset cursors; archive/delete (`session_deleted`). |

**Deliberate v1 rejections (documented divergences, clear errors):** `multiagent` config, session `resources`, non-empty `vault_ids` on create, `scope:"account"`. `deployment_id`/`memory_store_id` list filters return empty sets (nothing can match). Reference-side validations not enforced yet: numeric caps (max 128 tools, metadata limits) and the mcp_servers cross-checks (max 20, unique names, each referenced by an `mcp_toolset`). A skill without a `version` is normalized to the literal `"latest"` (the reference resolves it to a concrete version; nothing resolves skill versions here yet).

**Test coverage:** contract tests over real HTTP + Dockerized Postgres: full-surface response-shape assertions (every `api:"required"` field, `[]`/`{}`/null defaults, UTC `Z` timestamps), optimistic-version 409 + no-op on conflict, patch semantics (null clears, metadata upsert/delete), snapshot pinning + overrides, keyset pagination walks both directions incl. a concurrent-insert walk and prev-cursor round-trip, config-merge preservation, archived-immutability, bootstrap-key rotation, 413 oversize bodies, strict unknown-field rejection, OTel remote-parent span continuation, auth (missing/wrong/revoked key), error envelope on 404/405/500, corrupt-row and dropped-table defensive paths.

**Known debt (recorded slice 2, settled slice 5):** `internal/api` declares its own wire structs (`agentSpec`, `agentJSON`, `sessionAgentJSON`) instead of reusing `internal/domain`'s `AgentSpec`/`ResolvedAgent`, because `domain.Tool` keeps tool bodies in a non-serializable `Raw` field and the domain tags use `omitempty` where the wire requires always-present fields. Slice 3 honored the rule for its new surface — the event endpoints consume `domain.Event`/`domain.EventType`/`domain.ModelUsage` directly, no parallel event structs — and added no new drift. Settled in **slice 5**: `domain.AgentSpec`/`ResolvedAgent`/`Usage` became the wire shapes themselves (always-present fields, raw collection entries) and the api copies collapsed onto them — domain is the single source of truth per CLAUDE.md rule 1.

### `internal/events` + events API — append-only log, send/list, SSE stream (slice 3)

The event log is the single source of truth for session state. Verified end-to-end with the real `ant` CLI (v1.16.0): `beta:sessions:events send/list/stream` against `cmd/controlplane` — batch echo parsed by the typed SDK client, `--type` filter, `--limit 1` auto-pagination following our cursors, live stream frames, and a clean exit when `session.deleted` terminated the stream.

| File | Contents |
|---|---|
| `events/log.go` | `Log.Append` — per-session `seq` allocation under the session row lock (`SELECT … FOR UPDATE`; concurrent appends serialize per session, sessions don't contend), `sevt_` id assignment, `pg_notify` on commit only. `Log.List` — types / `created_at` ranges / seq-keyset / order / limit. Sentinels `ErrSessionNotFound` / `ErrSessionArchived`; stream-only types are unpersistable by construction. |
| `events/inbound.go` | `NormalizeInbound` — the POST contract: only the wire's 7 inbound types; field-exact validation (content-block unions per carrier, source unions, `deny_message` only with `result:"deny"`, `user.tool_result` only on `self_hosted` environments, `system.message` at most one / final / immediately after a user payload event); nullable fields normalized to explicit nulls; validated blocks kept as the client's raw bytes so they round-trip byte-for-byte. |
| `events/broker.go` | Postgres LISTEN/NOTIFY fan-out: one listening connection per process, held only while subscribers exist; wake signals are coalesced pointers ("re-read the log"), so a dropped notification can delay but never lose an event; reconnect re-wakes every subscriber; `Ready` lets the SSE handler snapshot its tail position only after LISTEN coverage is active (no subscribe-window gap). Frames (previews, `session.deleted`) are best-effort broadcast by contract. |
| `events/preview.go` | `event_start` / `event_delta` preview frames (delta type is literally `content_delta`, **not** the Messages API's `content_block_delta`); `agent.message` streams text fragments, `agent.thinking` is start-only; the preview pre-allocates the buffered event's id for reconciliation; long fragments auto-split at the same index to fit the 8000-byte NOTIFY cap (JSON-escape-aware chunking). Previews are never persisted and never replayed. |
| `events/span.go` | `StartModelRequest`/`End` — the `span.model_request_start`/`_end` wire events and the OTel client span come from one instrumentation point (CLAUDE.md principle 3), linked via `model_request_start_id` and carrying `model_usage`. |
| `api/events.go` | `POST /v1/sessions/{id}/events` (batch `{"events":[…]}` → `{"data":[…]}` echo with server-assigned ids, `processed_at` null until processed), `GET …/events` (PageCursor envelope `{"data","next_page"}` — no `prev_page` on events — opaque seq-keyset cursor, `types[]` in both spellings, `created_at[gt|gte|lt|lte]`, `order`), `GET …/events/stream` (SSE `event:`+`data:` framing — the reference decoder drops unnamed frames — `ping` keepalive, `?event_deltas[]` opt-in previews filtered per subscriber, live tail from connect time). `DELETE /v1/sessions/{id}` now broadcasts an ephemeral `session.deleted` event that terminates active streams. |

**Review hardening (same PR, from the dual-review pass):** `created_at` is taken under the session lock via `clock_timestamp()` — the column default `now()` freezes at BEGIN, which would let a lock-waiting transaction write a higher seq with an earlier timestamp and silently break `created_at[gt]` watermark polling; batches insert as one multi-row statement (one round trip under the lock); `\u0000` (valid JSON, unstorable in jsonb) is rejected with a clean 400 instead of surfacing as a 500; `{"type":"text","text":null}` no longer slips through validation; the events cursor binds its sort direction (a follow-up without `?order=` keeps walking the same way, a contradicting one is a 400); the SSE wake path drains preview frames first (an `event_start` can never trail its own buffered event), reads backlog in bounded batches, emits the protocol `error` frame on mid-stream failure, and backstops a lost `session.deleted` broadcast with a ping-time existence check so streams on deleted sessions always terminate; a dropped `event_delta` poisons the rest of its preview so partial text is a clean prefix, never an interior hole; the LISTEN retry loop backs off; span `End` appends the wire event before closing the OTel span and marks the span errored if the append fails; the per-stream preview tracker is capped.

**Slice-3 wire/scope decisions (documented divergences):** the stream is a live tail from connect time — no history replay, no `Last-Event-ID` (the reference client parses neither; reconnecting clients seed via list). `tool_use_id`/`custom_tool_use_id` references were initially not cross-checked on send; slice 5 tightened this — a tool result must reference an existing, kind-matching, not-yet-answered tool use or the send is a 400 (the log is append-only, so one bad reference would poison every future replay). `user.define_outcome` is rejected (outcome surface deferred; no `outc_` prefix in v1). A non-null `session_thread_id` is rejected (threads deferred). `seq` is internal only — never a wire field; cursors carry it opaquely.

**Test coverage:** events package contract tests against Dockerized Postgres — concurrent-append seq integrity (8×25 single session, gap/duplicate-free), cross-pool NOTIFY fan-out, listener kill via `pg_terminate_backend` → reconnect + heal-wake, garbage NOTIFY payloads survived, preview reconciliation (buffered event supersedes deltas under the preview id), JSON-escape-aware chunk reassembly, same-source span emission (one exported OTel span + linked start/end events per request). API contract tests over real HTTP — field-exact echo shapes per inbound type, ~30-case validation sweep (batch atomicity included), cursor walk, SSE framing parsed off the live socket, delta opt-in vs plain subscriber, ping keepalive, `session.deleted` stream termination, corrupt-row 500s.

### `internal/provider` — config-driven model access (slice 4)

The first provider: any endpoint speaking the Anthropic Messages protocol, constructed purely from configuration. Verified with a **real model turn against the self-hosted Anthropic-protocol endpoint in `.env`** (a non-Anthropic gateway model): text streamed, usage populated, `stop_reason` mapped — proving the custom `base_url` requirement end-to-end. `github.com/anthropics/anthropic-sdk-go` is now a pinned direct dependency at **v1.56.0** — the same version as the local reference checkout, so all wire-compat comparisons made in slices 2–3 hold against the pinned SDK verbatim.

| File | Contents |
|---|---|
| `provider/provider.go` | `Config` (`protocol` / `model` / `base_url` / `api_key` / optional headers — CLAUDE.md principle 4), `Request`/`Message` in Anthropic Messages semantics with content blocks and tool definitions as **raw wire JSON** (the Anthropic adapter is near-zero-conversion; lossy mapping stays confined to future non-Anthropic adapters), `Chunk` stream (`text_delta` / `thinking_delta` / complete `tool_use` after input accumulation / `done` with `stop_reason` + `domain.ModelUsage`), `Provider`/`Stream`/`Factory` interfaces, and the model→provider `Registry` (exact match + `"*"` default; a route without an upstream `model` passes the agent's model string through to the endpoint). |
| `provider/anthropic/anthropic.go` | The Anthropic-protocol adapter over the official SDK: `base_url` is **required** (no silent api.anthropic.com fallback), extra headers pass through for gateway routing, streaming events translate to chunks (tool_use inputs accumulate from `input_json_delta`, `message_delta` carries stop reason + output usage). |

**Test coverage:** contract tests against a fake Anthropic-protocol `httptest` server — request assertions (path, `x-api-key`, `anthropic-version`, extra headers, model/max_tokens/stream/system/messages/tools round-trip) and chunk-translation assertions (thinking → text ×2 → accumulated tool_use → done with full usage), verbatim passthrough (string-form content, plus fields and tool types unknown to the pinned SDK version, all reach the wire byte-preserved via `param.SetJSON` — the typed round-trip would silently drop them), empty tool input, upstream 401, config validation; registry routing/fallback/passthrough/validation. Plus the env-gated integration test (`TestIntegrationRealEndpoint`): reads `MODEL_*` from the environment or the gitignored `.env`, **skips cleanly when unconfigured** (verified — CI without credentials is unaffected), never logs credential values.

**Slice-4 scope notes:** the `openai` protocol adapter is deferred (the `Factory` seam exists; the registry rejects unknown protocols at construction). Registry config loading landed in slice 5 (`provider.LoadRoutes` + `MODEL_PROVIDERS_PATH`); a table-backed source can follow the same seam. Retry policy is the SDK default. Thinking **signatures** (`signature_delta`) and `redacted_thinking` blocks are not captured in the chunk vocabulary: on this platform's wire, `agent.thinking` events carry no content at all, so thinking is never replayed from the event log — how mid-turn tool-use continuations handle signed thinking must be settled against reference behavior in slice 5 (recorded here so it isn't forgotten). Providers are constructed with `option.WithoutEnvironmentDefaults()`: ambient `ANTHROPIC_*` credentials on the host are never mixed under config-driven options and can never leak to a third-party `base_url`. Malformed streams (overlapping/unclosed tool blocks, missing `message_delta`) fail loudly rather than emitting corrupted turns. The env-gated integration test honors `-short` for a no-network run.

### `internal/brain` + `internal/queue` + state machine — the orchestration loop (slice 5)

The platform converses end-to-end. Verified with the real `ant` CLI against the local stack (controlplane + brain + Dockerized Postgres) driving the **real Anthropic-protocol endpoint from `.env`**: `user.message` flips the session to running, a brain claims the turn, replays the log, and the CLI decodes the full turn from both the list and the live SSE stream — `session.status_running`, `span.model_request_start`, `event_start`/`event_delta` previews, buffered `agent.message`, `span.model_request_end` with usage, `session.status_idle` `end_turn` — with session `usage` folded on the resource.

| Piece | Contents |
|---|---|
| `queue/queue.go` | The internal work queue over the existing `work_items` table (`FOR UPDATE SKIP LOCKED`). `Enqueue` is idempotent per (session, kind) while a live item exists (partial unique index, migration `0003`); `Claim` leases the oldest queued item and reclaims expired-active ones (flagged so the brain surfaces recovery); `Extend`/`Complete`/`Requeue` carry the lease expiry as an ownership proof (the reference work API's `expected_last_heartbeat` shape) — a claimant that lost its lease gets `ErrLeaseLost`, never silently finishes a reassigned item. |
| `events` `AppendWith`/`AppendInTx` | Atomic session-state side effects under the append's session row lock: `SetStatus` (resource column and status event can never disagree), `AddUsage` (fold a turn into `sessions.usage`), `MarkProcessedThrough` (stamp consumed inbound events), `Then` (join work enqueue to the same commit). `AppendInTx` lets the API decide the batch under the lock. |
| `api` state machine | `POST /events` is one transaction (`FOR UPDATE OF s`): `user.message` on an idle session → running + `session.status_running` + model_turn enqueue; a tool result while running → next model_turn **only when it completes the set** (every tool use answered — the Messages API rejects a partial replay, so parallel tool calls wait for their last result), no status event (awaiting a tool is still `running`); everything else appends only. Tool results are validated against the log before anything commits: an unknown, kind-mismatched, or already-answered reference is a 400. The response echoes only the posted events. Session updates emit `session.updated` with only the changed fields (title / non-empty metadata / agent snapshot), compared semantically — stored jsonb never byte-matches a fresh marshal. |
| `brain/brain.go` | Claim → replay → generate → **commit the whole turn atomically**: the emitted events (`agent.message`, tool intents), `span.model_request_end`, the status change, the usage fold, the processed watermark, and the work item's fate are ONE transaction under the session row lock, with the queue's lease proof inside it. That single commit is both the liveness guarantee (API triggers serialize on the same lock — a tool result posted mid-settle either sees the live item, suppressed, or the completed one, enqueued; never the gap) and the integrity guarantee (a brain that lost its claim rolls the whole turn back — the log never carries a loser's half-turn, whose duplicate tool intents could never all be answered and would poison every future replay). `tool_use` suspends and completes the item (the intents commit in that same transaction, so nothing can have answered them yet — there is never anything to chain there); `end_turn` idles unless pending input (a mid-turn `user.message` **or** a suppressed tool result) chains the turn by **requeueing its own item**; failures append `session.error` (`model_request_failed_error`, `retry_status: exhausted`) and idle with `retries_exhausted` — unless input is pending, which chains instead of stranding it. Errors are classified: provider/model and deterministic input failures fail the turn visibly; brain-side infra failures (database, lost lease) abandon the turn to lease expiry with nothing committed and nothing on the wire. A lease keeper goroutine re-extends the lease at TTL/3 during streaming (long time-to-first-token can outlast any inter-chunk extension); each renewal is bounded by its own timeout, so a stalled database cannot hang the turn behind an unreturnable `Extend`. Reclaimed items surface `session.status_rescheduled` + `status_running` with the lease asserted in the same transaction; the staleness check (status/archived, under the session lock) runs first, so a reclaim of finished work never flips an idle session back to running. |
| `brain/replay.go` | The log IS the conversation: role-run merging, tool_result blocks sorted ahead of text within a user turn, `tool_use` blocks rebuilt under their **event ids** (the provider-side tool id is discarded at emission; result events reference the event id), string content normalized, `system.message` text appended to the system prompt. Custom tools become real tool definitions; `agent_toolset` expansion waits for the executor (slice 6), `mcp_toolset` for the MCP client. |
| `brain/stream.go` | Provider chunks → wire: `agent.message` preview opened at the first **non-empty** text delta (provider block index → content entry index), `agent.thinking` preview per block (start-only, buffered event under the preview id), tool_use collected, the buffered `agent.message` lands **before** `span.model_request_end` (the SDK accumulator closes previews at span end). Empty text deltas are skipped before anything is allocated, so a block that never produces text takes no content index — the preview's delta indices and the stored content array can never disagree, and no preview is opened for an event that will never land. A `tool_use` whose input is not a JSON object (truncated, or a bare string/array/number) fails the turn visibly rather than reaching the append-only log. Database failures mid-stream are marked infra — never reported as model failures. |
| `provider/config.go` + `cmd/brain` | `LoadRoutes` reads the `model_providers` JSON file (`model` / `protocol` / `base_url` / `upstream_model` / `api_key` or `api_key_env`; unknown keys rejected). `cmd/brain`: `DATABASE_URL` + `MODEL_PROVIDERS_PATH` + lease/poll tunables + OTel. |

**Slice-5 decisions (documented assumptions, revisit against reference behavior when observable):** the tool_use id the model sees on replay is the `sevt_` event id; a session awaiting a tool result stays `running` (`requires_action` is reserved for slice-7 confirmations); a turn with parallel tool calls resumes only on the completing result (full-set gating, both API-side and settle-side); `max_tokens`/`stop_sequence` settle as `end_turn`; tool-only turns emit no empty `agent.message`; the flat cache-creation counter folds into the `ephemeral_5m` bucket; no automatic retry — one failed model request ends the turn visibly (but pending mid-turn input chains a fresh turn rather than being stranded); `user.interrupt` is logged but not yet acted on; `session.status_running` is emitted once per idle→running flip, not per tool result; v1 never requests extended thinking, so continuations legally omit thinking blocks (the slice-4 signature note stands for when thinking becomes configurable). `agents`/`sessions` wire structs now live in `internal/domain` (`AgentSpec`/`ResolvedAgent`/`Usage` are the wire shapes; the slice-2 debt is settled). `internal/pgtest` is the shared Docker-Postgres harness for new packages; migrating the three older private copies stays a chore. **Known crash-window residue (cosmetic, accepted):** `agent.thinking` events and `span.model_request_start` commit mid-stream, so a brain that dies mid-turn can leave a duplicate thinking event or a dangling span start on the log — replay skips both, no request is affected. **Known limitation (slice 7 territory):** a session suspended on a tool whose result never arrives stays `running` indefinitely; `user.interrupt` semantics will provide the escape hatch.

**Test coverage:** queue contract tests (idempotent enqueue, kind isolation, FIFO, parallel claims share nothing, expired-lease reclaim + lost-lease `Complete`/`Extend`/`Requeue`/`Assert` failures); `AppendWith` atomicity (Then-error rollback, usage accumulation, watermark stamping); API state-machine tests (flip + enqueue exactly once, tool-result resume gated on the full set, tool-result validation 400s — unknown/kind-mismatch/duplicate/already-answered, no-op update emits nothing including jsonb-normalized retries, a change past 2^53 is still detected, `session.updated` carries only changed fields); brain turns against a scripted provider over real Postgres (full-turn event order, tool suspend/resume with event-id linkage, parallel tool calls resume only on the full set, mid-turn message chaining via requeue on both the end_turn and failure paths, end_turn settle chains an unconsumed tool result, a lease lost mid-turn commits nothing, long time-to-first-token survives via the lease keeper (a rival claim mid-stream finds nothing to take), a lost lease mid-stream abandons quietly with no wire error, empty text blocks are not stored and an empty leading block does not shift the stored content off the delta indices already streamed, malformed tool input (truncated JSON, or a bare string/array/number) fails the turn visibly instead of reaching the log, null tool input becomes `{}`, corrupt agent state fails visibly instead of reclaim-looping, tool_use stop without tool blocks fails visibly, provider error → `session.error` + `retries_exhausted`, unrouted model fails visibly, reclaim surfaces recovery, `Run` drains and stops); white-box replay tests (role merging, result-first ordering, null/absent tool input replays as `{}`, malformed events rejected).

---

## Next up

1. **Slice 6:** tool-exec executor + Docker sandbox provider + built-in `agent_toolset` expansion (the tool-exec queue kind and its enqueue seam already exist). Today the brain emits only `agent.custom_tool_use` (client-executed); `agent.tool_use` intents begin with the toolset expansion.
   **Trap to close when the executor lands:** the turn scheduler only ever sees *inbound* results. `pendingInputTypes` (`brain/brain.go`) lists `user.message` + the two `user.*_tool_result` types, and the append path stamps `processed_at` on every platform-emitted event at insert — so an executor appending `agent.tool_result` while a turn is live would be suppressed by the live work item, missed by the settle's pending check, and stranded on an idle session. The executor's result append must therefore either carry the enqueue itself (its own `Then`, mirroring the API trigger) or the pending/resume checks must learn the platform-side result types. `events.HasUnansweredToolUse` already counts `agent.tool_result`/`agent.mcp_tool_result` as answers, so only the *scheduling* half is missing.
2. **Slice 7:** permission policies + `requires_action` / `user.tool_confirmation` approval round-trip.

---

## Deferred past v1

Seams are reserved (a column or an interface boundary) but **not implemented**. Do not build these ahead of schedule:

- Secret vaults + egress credential injection (tokens never reach the sandbox)
- Scheduled deployments (cron)
- Memory stores
- Multi-agent threads / coordinator topology
- Skills distribution and execution
- git/repo mounting and the Files API
- Multi-tenant RBAC / SSO
- Redis-backed queue (Postgres queue is the v1 backend)

---

## Load-bearing decisions (quick recall)

Full rationale lives in the plan and `CLAUDE.md`; these are the ones most likely to be accidentally violated:

- **Anthropic's domain model is authoritative.** adk-go (`google.golang.org/adk/v2`) is a source of ideas only — never a dependency of the domain layer, and its genai-centric `Event`/`session.Service`, in-process `Runner`, and `server/adkrest` are explicitly not used.
- **Tool execution is fully async** through the event log + work queue. The brain never runs a tool in-process. Platform-managed `cloud` and customer `self_hosted` are the same pull protocol at two deployment points.
- **Model providers are config-driven** (`protocol` / `model` / `base_url` / `api_key`). Never hard-code `api.anthropic.com`.
- **Sessions carry no `user_id`.** Scoping is org/workspace/project. End-user ↔ session ownership is an application-layer concern; `metadata` and the audit-only `created_by` are the hooks.
- **v1's first-class scenario is a general task agent** (bash + file + web toolset). git/repo mounting is *not* a first-class v1 concern.
- Apache-2.0, pure open source — no open-core edition gating.

---

## Environment notes

- **Go 1.26.5** (installed via Homebrew).
- **Docker** available. **`psql` is not installed** — use the Postgres container for database work. The `internal/store` and `internal/api` tests start their own `postgres:16-alpine` container automatically (and fail loudly, not skip, without Docker — skipping would hollow out the coverage gate).
- **`ant` CLI:** no binary installed; build it from the read-only checkout for smoke tests: `cd ~/Projects/anthropic-cli && go build -o <scratch>/ant ./cmd/ant`. It ignores `ANTHROPIC_BASE_URL` — pass `--base-url http://127.0.0.1:<port>` explicitly.
- **Repository:** <https://github.com/OpenSDLC-Dev/managed-agent-platform> (public).
- **Module path:** `github.com/OpenSDLC-Dev/managed-agent-platform` — note the owner's mixed case is intentional and must match the GitHub owner exactly; Go escapes the uppercase letters in the module cache.

## Open questions / blockers

- None right now.
