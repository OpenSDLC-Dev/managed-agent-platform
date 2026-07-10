# STATE.md — Development Progress

Running record of where this project actually stands, so work can resume cleanly across sessions.

**Keep it honest.** A slice is only "done" when its code builds, its tests pass, and its behavior has been verified by the independent **`verifier` subagent** (`.claude/agents/verifier.md`; protocol in CLAUDE.md → "Independent verification"). Update this file whenever a slice changes status.

---

## Snapshot

- **Last updated:** 2026-07-10
- **Phase:** Control plane — the wire-compatible CRUD surface is live (the real `ant` CLI drives it); the event log, brain, and executor do not exist yet, so nothing runs end-to-end.
- **Current slice:** 2 complete; next up is slice 3 (event log + SSE).
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
| 3 | Append-only event log (seq allocation) + `POST /events` + SSE stream (`event_start` / `event_delta` reconciliation) + `span.*` emitted from the same point as OTel spans | ⬜ Not started |
| 4 | `ModelProvider` (config-driven: protocol / model / base_url / api_key) + `model_providers` routing; first provider passing a single model turn; verify a custom `base_url` works | ⬜ Not started |
| 5 | Brain orchestration loop (replay → assemble provider request → write Anthropic-native events). No adk runtime. | ⬜ Not started |
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
- CI (`.github/workflows/ci.yml`: build / 32-bit cross-compile / vet / gofmt / `test -count=1` / total statement coverage ≥90% over `./internal/...`) + branch→review→PR→CI→squash-merge iteration workflow with dual code review (`/codex:review` + `/code-review`) (CLAUDE.md → "Iteration workflow").
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
| `server.go` | Route table (Go 1.22 method patterns) + `NewHandler`. **Updates are `POST /v1/{resource}/{id}`, not PATCH** (SDK is authoritative; the plan doc predates this). Envelope-shaped 404/405 fallbacks. `?beta=true` and `anthropic-*` headers accepted and ignored. |
| `auth.go` | `x-api-key` middleware against `api_keys` (SHA-256 hash only), `EnsureAPIKey` bootstrap (used by `cmd/controlplane` at startup with `CONTROLPLANE_API_KEY`); authenticated key ID becomes the audit-only `sessions.created_by`. |
| `errors.go` | Wire error envelope `{"type":"error","request_id":…,"error":{type,message}}` + `request-id` header on every response. Version conflicts are `invalid_request_error` with HTTP 409 (the reference SDK has no dedicated conflict type). |
| `page.go` | Cursor pagination: `{"data":[…],"next_page":…}` (+ `prev_page` on sessions), opaque offset cursors via `?page=`, `limit` default 20 / max 100. |
| `wire.go` | Body parsing with omitted/null/value distinction (reference updates are patches), tools/skills/mcp_servers union validation (raw bodies preserved so configs round-trip byte-for-byte), UTC-normalized timestamps (`Z`, never a local offset). |
| `agents.go` | CRUD + optimistic `version` in the update body (mismatch → 409), immutable `agent_versions` snapshots, `GET ?version=N` pinned reads, versions list, archive (idempotent). No DELETE — the wire has none. |
| `environments.go` | CRUD incl. update (exists in the SDK though the plan omitted it) + delete (`environment_deleted`; refused while sessions reference it) + archive; config union normalized (cloud → full networking/packages surface, self_hosted → type only); `scope` rendered as the constant `"organization"`; metadata updates delete on empty string as well as null (an environments-only rule in the reference). |
| `sessions.go` | Create resolves the agent union (id string / `{type:"agent"}` / `agent_with_overrides`) into a full `resolved_agent` snapshot; `session_` accepted for `sesn_`; update limited to title/metadata/`agent.tools`+`agent.mcp_servers` (vault_ids update rejected, matching the reference); list filters (agent_id/agent_version/statuses[]/order/created_at ranges) + bidirectional cursors; archive/delete (`session_deleted`). |

**Deliberate v1 rejections (documented divergences, clear errors):** `multiagent` config, session `resources`, non-empty `vault_ids` on create, `scope:"account"`. `deployment_id`/`memory_store_id` list filters return empty sets (nothing can match). Reference-side validations not enforced yet: numeric caps (max 128 tools, metadata limits) and the mcp_servers cross-checks (max 20, unique names, each referenced by an `mcp_toolset`). A skill without a `version` is normalized to the literal `"latest"` (the reference resolves it to a concrete version; nothing resolves skill versions here yet).

**Test coverage:** contract tests over real HTTP + Dockerized Postgres: full-surface response-shape assertions (every `api:"required"` field, `[]`/`{}`/null defaults, UTC `Z` timestamps), optimistic-version 409 + no-op on conflict, patch semantics (null clears, metadata upsert/delete), snapshot pinning + overrides, pagination walks both directions, auth (missing/wrong/revoked key), error envelope on 404/405/500, corrupt-row and dropped-table defensive paths.

---

## Next up

1. **Slice 3:** Append-only event log (seq allocation) + `POST /events` + SSE stream + `span.*` events emitted from the same instrumentation point as OTel spans.
2. **Slice 4:** `ModelProvider` (config-driven) + `model_providers` routing + first provider passing a single model turn (pin the SDK dependency; re-check wire-drift then).

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
