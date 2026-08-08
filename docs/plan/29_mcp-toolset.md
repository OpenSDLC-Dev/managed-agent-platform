---
status: draft
issue: "#45"
---

# MCP client and `mcp_toolset` end to end (plan 29)

This plan closes #45: the MCP tool path wired end to end. Today `mcp_servers` +
`mcp_toolset` are accepted on the wire and stored, but the brain never expands the
toolset (`internal/brain/replay.go` — "mcp_toolset still waits for the MCP client"), no
MCP client exists, and an agent configured with MCP tools silently gets none. After this
plan, an agent with an `mcp_toolset` calls a real MCP server's tools with discovery,
permission gating, vault-credential injection, networking policy, and the reference's
failure semantics — the round-trip in the event log.

Scope decisions settled with the user on 2026-08-08:

1. **Full reference parity** — the core loop plus all four peripherals: OAuth
   refresh-on-expiry, materialized `mcp_toolset` config echo + bidirectional reference
   validation, the gate's `allow_mcp_servers` finally honored, and the >100k-character
   output spill into the sandbox.
2. **Discovery runs in the executor; the catalog lives in Postgres** (three
   architectures were weighed; brain-inline discovery and a standing control-plane
   reconciler were rejected — the executor already owns every other piece of platform
   tool egress, and the reference's lazy/retry-on-resume semantics fit a per-session
   catalog, not a background loop).
3. **Acceptance is three-fold**: a local-fixture live eval (`RUN_EVALS`), a live tier
   against a real public MCP server (`RUN_LIVE_MCP_TESTS`), and an `ant` CLI end-to-end
   acceptance transcript recorded in docs/HISTORY.md.

Out of scope: MCP Tunnels (`mcp-tunnels-2026-06-22`, a separate research-preview beta),
deployment-level `mcp_egress_blocked_error` (no `/v1/deployments` surface exists),
changes to the `mcp_oauth_validate` probe (already live, plan 12 D8), and stdio
transport (the wire admits only `type: "url"`).

## Ground truth (verified 2026-08-08)

Resolved per CLAUDE.md's order: public docs (platform.claude.com, fetched 2026-08-08:
managed-agents/mcp-connector, /vaults, /permission-policies, /environments, /reference)
→ `anthropic-sdk-go` (local checkout v1.62.0; the managed-agents MCP surface is
unchanged from the pinned v1.61.0) → the `ant` CLI source (Stainless-generated; adds no
semantics of its own). The SDK carries **two separate MCP schemas** — the Messages API
connector (`betamessage.go`: `authorization_token`, map-keyed configs, `defer_loading`)
and managed agents (`BetaManagedAgents*`) — never conflate them; everything below is the
managed-agents surface.

### Wire shapes

- **`mcp_servers[]` entry** (betaagent.go:2628-2653): exactly `{type: "url", name, url}`.
  Not a union — no other variant exists, no `authorization_token`, no
  `tool_configuration`. `name` 1–255 chars unique within the array, ≤20 servers, `url`
  ≤2048 chars. **Both reference directions are rejected**: unreferenced servers *and*
  dangling toolsets ("The API rejects agent definitions with unreferenced servers or
  dangling toolsets" — mcp-connector guide). Update/override/mid-session patch are all
  full-replacement (omit = preserve, `[]`/`null` = clear); `agent.mcp_servers` is one of
  only two mid-session-mutable agent fields (betasession.go:1580-1590), both already
  implemented here.
- **`mcp_toolset`** (betaagent.go:1950-1977): `{type, mcp_server_name, default_config?,
  configs?}`. `configs` is an **array** of `{name (1–128 chars), enabled?,
  permission_policy?}` — not the Messages connector's map. `default_config` =
  `{enabled?, permission_policy?}`; `enabled` **defaults to true**; policy union is
  exactly `always_allow` | `always_ask` (no `always_deny`). **The response shape is
  resolved**: `configs[]`/`default_config` come back with `enabled` and
  `permission_policy` required and materialized (betaagent.go:1773-1820) — the server
  fills defaults, it does not round-trip the sparse request.
- **`agent.mcp_tool_use`** (betasessionevent.go:325-381): `{id, name, mcp_server_name,
  input (object), processed_at, type}` required; `evaluated_permission`
  (`allow`|`ask`|`deny`) optional; `session_thread_id` nullable. `deny` has no
  configurable policy source — it stays exactly what `internal/domain/agent.go` already
  documents: reserved, produced by nothing.
- **`agent.mcp_tool_result`** (betasessionevent.go:152-188): `{id, mcp_tool_use_id,
  processed_at, type}` required; `content` optional array of **four** block types —
  `text` | `image` | `document` | `search_result`; `is_error` nullable. No
  `mcp_server_name` on the result.
- **Confirmation**: the same `user.tool_confirmation`, whose `tool_use_id` carries the
  `agent.mcp_tool_use` event id — the id is named `tool_use_id` by the confirmation and
  `mcp_tool_use_id` by the result. `requires_action` gating, partial-resolution
  re-emission, and `deny_message` are the existing machinery, unchanged.
- **Failure events**: `session.error` variants `mcp_connection_failed_error` and
  `mcp_authentication_failed_error`, both `{mcp_server_name, message, retry_status}`
  with `retry_status` = `retrying` | `exhausted` | `terminal`
  (betasessionevent.go:2323-2490). `session.error` and `stop_reason.retries_exhausted`
  already exist in `internal/domain/event.go`.
- **Credentials** (vaults guide + betavaultcredential.go): `mcp_oauth` and
  `static_bearer` each carry an immutable `mcp_server_url`. Matching is by URL after
  normalization — "scheme and host lowercased, default ports and trailing slashes
  stripped; a different path, subdomain, or non-default port does [not match]". First
  matching vault wins; no match → the connection is attempted **unauthenticated**.
  Refresh is platform-performed (RFC 6749; `token_endpoint_auth` `none` |
  `client_secret_basic` | `client_secret_post`; RFC 8707 `resource`). Rotation
  propagates to running sessions. `session.vault_ids` is create-time only.
- **Networking** (environments guide + betaenvironment.go:490-533): `limited.
  allow_mcp_servers` (default false) additively admits "MCP server endpoints configured
  on the agent" beyond `allowed_hosts`. Cloud-only — `self_hosted` has no networking
  block at all.
- **Behavioral pins**: session creation does not validate MCP connectivity or
  credentials; a failed connection "is retried on the next `session.status_idle` to
  `session.status_running` transition". MCP tool output >100,000 characters "is
  automatically written to a file in the sandbox. The model receives a truncated
  preview with the file path". Streamable HTTP with automatic fallback for
  deprecated-SSE-only servers.
- **Execution locus**: MCP is platform-side, always — the SDK says it three times
  (betasessiontoolrunner.go:209-212, :694-697, :1297-1300: "MCP tools are
  server-side"), the work API has zero MCP surface, and the BYOC worker's contract is
  `agent.tool_use` + `agent.custom_tool_use` only. Mirrors our `web_exec` precedent
  exactly (webwork.go: "for cloud AND self_hosted sessions alike").

### Protocol note (MCP spec 2026-07-28)

The 2026-07-28 revision makes MCP stateless (no `initialize` handshake, no
`Mcp-Session-Id`; per-request `_meta` versioning; `server/discover`), adds `ttlMs`
caching hints on `tools/list`, standardizes OTel `traceparent` propagation in `_meta`,
and deprecates roots/sampling/logging and the HTTP+SSE transport. The official
`github.com/modelcontextprotocol/go-sdk` (v1.7.0+) negotiates every version from
2024-11-05 up — the reason to build on it rather than hand-roll. Statelessness removes
any connection-affinity concern between discovery and execution.

## Architecture

### `internal/mcp` (new package)

A thin wrapper over the official go-sdk exposing what the platform needs and nothing
else: `Connect(ctx, Config) (*Conn, error)` (Config: URL, optional bearer token,
`*http.Client`), `Conn.ListTools`, `Conn.CallTool`, `Conn.Close`. Streamable HTTP
first, SSE fallback on the documented signal. Connections are **per-work-item**: the
executor connects, works, closes — no pooling, no shared state, nothing for a crashed
process to lose. Bearer injection via the supplied `http.Client`'s RoundTripper (the
SDK's experimental OAuth machinery is not used). The dial path reuses
`vaultvalidate.go`'s per-resolved-IP guard (loopback/link-local/multicast refused,
RFC 1918 allowed — the on-prem premise) and its refusal to follow redirects. OTel
`traceparent` rides `CallToolParams.Meta` per the 2026-07-28 convention, emitted from
the same instrumentation point as `span.*` events (design principle 3).

### Catalog (`mcp_catalogs` table)

Per-session snapshot: `(session_id, server_name, url, tools jsonb, status, error,
fetched_at)`. Written only by the executor's discovery pass; read by the brain at
request assembly. Invalidation: rows for a session's changed/removed servers are
deleted by the session-update handler in the same transaction as the patch; a failed
row is retried on the next turn (the reference's idle→running semantics); no TTL, no
reconciler.

### Discovery and execution flow

New `queue.Kind` **`mcp_exec`**, one driver in the executor (`mcpwork.go`), following
`webwork.go`'s shape (consumer span, dead-session drain, lease keeper, no sandbox
Provision — cloud and self_hosted alike):

1. Brain assembles a turn; the agent has an `mcp_toolset` whose catalog row is missing
   → enqueue `mcp_exec`, suspend (no model call).
2. The MCP driver finds no unanswered MCP calls + missing catalog rows → for each such
   server: resolve credentials, enforce networking policy, connect, `tools/list`, write
   the row (or the failure), close → chain `model_turn`.
3. Brain assembles with the catalog: each enabled MCP tool becomes a model tool
   definition named **`mcp__{server}__{tool}`** (internal to provider requests only —
   the session wire always carries the bare `name` + `mcp_server_name`; the brain maps
   back at emission). A collision with a custom tool's name skips the MCP tool and
   appends a `system.message` warning. `configs[]` names absent from the catalog warn,
   never error (documented dynamic-availability semantics). `classify()` learns the
   third arm: `mcp__` names → `agent.mcp_tool_use` + the resolved per-tool policy.
4. On a call: `evaluated_permission` stamped; `ask` (the default) parks the session
   through the existing `requires_action` gate — `confirmableToolUseTypes` gains
   `agent.mcp_tool_use`, and denial synthesis gains the MCP shape
   (`agent.mcp_tool_result{is_error:true}`, the "slice-8+ work" note in
   `internal/events/toolflow.go`).
5. Allowed calls: brain enqueues by precedence `mcp_exec` → `web_exec` → `tool_exec`
   (a turn with MCP calls enqueues `mcp_exec` first; each driver answers its own family
   and chains the next needed — webwork.go's settlement grows from three branches to
   four).
6. The MCP driver answers each unanswered call: connect (credentials + policy),
   `CallTool`, convert `Content` → the four block types (`EmbeddedResource` →
   `document`; text through `toolset.SanitizeText`; inline content capped by
   `toolset.MaxOutputBytes`, and beyond the documented 100,000-character threshold the
   full output spills per "Output spill" below with the capped preview inline),
   `IsError` → `is_error`. Tool failure is model-visible
   only (`is_error` result); transport failure is an `is_error` result **plus**
   `session.error` — the model retries with different arguments, the platform heals
   connections; the two are never collapsed (the agentscope lesson).

### Output spill (>100k characters)

Cloud sessions: the full output is written into the session's sandbox via the existing
sandbox handle; the model receives the truncated preview + path. `self_hosted` has no
platform-side sandbox handle: truncate without spill, recorded in DIVERGENCES.md (the
reference's own behavior there is unobserved).

### Credentials and OAuth refresh

Resolved at connection time on every connect (discovery and execution alike) — rotation
propagates naturally, no push machinery. Matching implements the documented
normalization exactly; first vault wins; unmatched connects anonymously.
`static_bearer.token` / `mcp_oauth.access_token` → `Authorization: Bearer`. An expired
`mcp_oauth` (clock-skewed `expires_at`) with a refresh block refreshes first — the
RFC 6749 exchange is extracted from `vaultvalidate.go` into a shared internal helper,
and rotated tokens persist (the validate endpoint's existing precedent). A 401/403 from
the server → `session.error{mcp_authentication_failed_error}`.

### Networking policy — two enforcement points

- **MCP client egress** (executor): at dial — `unrestricted` admits all (behind the IP
  guard); `limited` requires host ∈ `allowed_hosts` ∪ (agent's MCP server hosts when
  `allow_mcp_servers`); `self_hosted` has no networking block → unconstrained
  (inference, recorded). A policy block → `mcp_connection_failed_error` with
  `retry_status: terminal`.
- **Sandbox egress** (gate): `internal/gate`'s `newPolicy` finally honors
  `allow_mcp_servers` — the gate config gains the agent's MCP server host set, so
  in-sandbox processes reach those hosts under `limited`, closing that half of the
  plan 12 slice 4 fail-closed divergence (#50).

### Retry semantics (inference — DIVERGENCES entry)

Connection-failure attempts per server per session are derived from the log (count
prior `session.error` events for that server — no new state): attempts 1–2 emit
`retry_status: "retrying"` and retry on the next turn; attempt 3 emits `"exhausted"`
and the server stops participating in discovery/expansion for the session; a policy
block is `"terminal"` immediately. The exact reference thresholds are unobserved.

## Slices

Ordered so no landed slice leaves an incoherent state (tools are never offered to the
model before they can be gated and executed):

1. **Wire correctness** — materialized `mcp_toolset` echo (agent create/update/read +
   session resolve) and bidirectional reference validation (dangling toolsets now
   rejected — the public docs pin both directions, superseding #66's one-way reading;
   that DIVERGENCES entry is rewritten in this slice).
2. **`internal/mcp` + catalog** — the client wrapper, `mcp_catalogs` migration + store,
   the `mcp_exec` discovery driver **including the MCP-client egress policy check**
   (limited networking is never fail-open, even one slice long; before slice 5 the
   driver connects unauthenticated, which is the documented no-match behavior). Nothing
   offered to the model yet.
3. **Gate machinery** — `confirmableToolUseTypes` + MCP-shaped denial/interrupt
   synthesis (event-layer, independently testable).
4. **Activation** — brain expansion + `agent.mcp_tool_use` emission + the execution
   driver + four-way settlement chaining + output spill. **#45's acceptance criterion
   is met at the end of this slice.**
5. **Credentials** — matching, injection, OAuth refresh, `mcp_authentication_failed`.
6. **Networking polish** — the gate's `allow_mcp_servers` host set (sandbox egress),
   retry-status escalation, `mcp_connection_failed` semantics.
7. **Evals + acceptance** — the `mcp-answer` eval (in-process fixture server + real
   model, `RUN_EVALS`), the live public-server tier (`RUN_LIVE_MCP_TESTS`; `.env` gains
   `MCP_LIVE_SERVER_URL` / `MCP_LIVE_SERVER_TOKEN`, missing config fails rather than
   skips), the `ant` CLI end-to-end transcript into docs/HISTORY.md, and archiving.

## Verification

Every slice ships contract tests in CI against in-process MCP servers (the go-sdk's
server + `InMemoryTransport`, and an `httptest` streamable-HTTP server for
transport-level cases) — discovery, execution, gating, credential matching and refresh
(an `httptest` token endpoint), policy enforcement, both failure families, spill — no
money, no external network. The dedicated live tiers and the CLI acceptance land with
slice 7. Each slice runs the verifier before review per CLAUDE.md; wire claims diff
against the SDK checkout field by field.

## New DIVERGENCES entries (inferences to record as they land)

- Execution locus: platform executor for all environment kinds (slice 4).
- `mcp__{server}__{tool}` internal model-facing naming + custom-collision skip
  (slice 4).
- `self_hosted`: no spill (slice 4) and unconstrained MCP egress (slice 2).
- Retry thresholds and log-derived attempt counting (slice 6).
- Reject statuses/messages for the new validation rungs (slice 1).
