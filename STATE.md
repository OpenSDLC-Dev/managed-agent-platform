# STATE.md — Development Progress

Running record of where this project actually stands, so work can resume cleanly across sessions.

**Keep it honest.** A slice is only "done" when its code builds, its tests pass, and its behavior has been verified by the independent **`verifier` subagent** (`.claude/agents/verifier.md`; protocol in CLAUDE.md → "Independent verification"). Update this file whenever a slice changes status.

---

## Snapshot

- **Last updated:** 2026-07-09
- **Phase:** Foundation — the domain layer exists; nothing runs end-to-end yet.
- **Current slice:** 0 (cross-cutting foundations) — half done.
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
| 0 | `internal/domain` (Anthropic-native types) + `internal/telemetry` (OTel/OTLP, context propagation) | 🟡 **In progress** — domain done, telemetry not started |
| 1 | Postgres schema + migrations (`internal/store`), reserved multi-tenant columns | ⬜ Not started |
| 2 | Control plane CRUD (agents / environments / sessions) + optimistic versioning + ID prefixes + `x-api-key` auth | ⬜ Not started |
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
- CI (`.github/workflows/ci.yml`: build / vet / gofmt / `test -count=1` / total statement coverage ≥90% over `./internal/...`) + branch→review→PR→CI→squash-merge iteration workflow with dual code review (`/codex:review` + `/code-review`) (CLAUDE.md → "Iteration workflow").

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

---

## Next up

1. **Finish slice 0:** `internal/telemetry` — OTel tracer/meter init, OTLP exporter, W3C `traceparent` propagation helpers. Must be in place before the event log so `span.*` events and OTel spans can be emitted from a single instrumentation point.
2. **Slice 1:** Postgres schema + migrations. Tables: `agents` + `agent_versions`, `environments`, `sessions`, `events` (append-only, unique `(session_id, seq)`), `work_items`, `api_keys` / `environment_keys`. Every core table carries reserved `org_id` / `workspace_id` / `project_id`.

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
- **Docker** available. **`psql` is not installed** — use the Postgres container for database work.
- **Repository:** <https://github.com/OpenSDLC-Dev/managed-agent-platform> (public).
- **Module path:** `github.com/OpenSDLC-Dev/managed-agent-platform` — note the owner's mixed case is intentional and must match the GitHub owner exactly; Go escapes the uppercase letters in the module cache.

## Open questions / blockers

- None right now.
