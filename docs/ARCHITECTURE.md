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
                                 model_turn work  │               │ append-only    │ tool_exec + web_exec
                                                  ▼               ▼ event log      ▼  (work queue)
                                          ┌──────────────┐  ┌──────────┐   ┌──────────────────────────┐
                                          │ brain pool   │  │ Postgres │   │ executor                 │
                                          │ (stateless:  │◀▶│ events   │◀─▶│  Docker / K8s sandbox    │
                                          │ replay log,  │  │ sessions │   │  providers; runs the     │
                                          │ call model,  │  │ agents…  │   │  built-in toolset in a   │
                                          │ emit events) │  └──────────┘   │  per-session container;  │
                                          └──────────────┘                 │  web_fetch/web_search    │
                                                                           │  in-process (web_exec,   │
                                                                           │  both env kinds)         │
                                                                           └──────────────────────────┘
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
| `executor` | The built-in sandbox worker for platform-managed (`cloud`) environments. Claims `tool_exec` work, runs the tool inside the session's sandbox container, posts `agent.tool_result`. Also claims `web_exec` work — web_fetch/web_search, run in its own process with no sandbox, for **both** environment kinds — `outputs_harvest` work, the deliverables snapshot of `/mnt/session/outputs/` a cloud session's outcome-grading cycle begins with, and `mcp_exec` work — both halves of the MCP path, likewise in its own process with no sandbox and for **both** environment kinds: the discovery that fills `mcp_catalogs`, enqueued when a turn suspends for a declared server with no row, and the tool call itself, enqueued when the brain routes an `mcp__{server}__{tool}` the model asked for. |
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
3. A sandbox tool call becomes an `agent.tool_use` event plus a `tool_exec` work item;
   the brain suspends (it holds nothing in memory a crash could lose). Not every call
   does: an `always_ask` policy suspends the turn with no item at all until a
   confirmation releases it, an MCP call takes `mcp_exec`, and a delegation call is
   answered in the settlement that commits the turn. A turn carrying a
   web tool (web_fetch/web_search) enqueues a `web_exec` item instead — the platform
   executor answers the web calls in its own process (both environment kinds, no
   sandbox) and chains the `tool_exec` for any sandbox tools on the same turn, so a
   BYOC worker never observes an unanswered web call.
4. For a platform-managed (`cloud`) environment the executor claims the item straight
   off the Postgres queue (`FOR UPDATE SKIP LOCKED`, lease + reclaim); for a
   `self_hosted` environment a BYOC worker claims the same kind of item over the wire
   work API (`poll`/`ack`/`heartbeat`/`stop`, lease expiry, dead-worker reclaim) — the
   same pull semantics at two deployment points. Either materializes the agent's
   skills into the freshly provisioned sandbox (`{workdir}/skills/<name>/`, versions
   resolved at use time, per-skill failure tolerated) along with the session's mounted
   resources (below), runs the tool, and posts the result event (`agent.tool_result`
   platform-managed, `user.tool_result` self-hosted). On the platform-managed side the
   run's end also reconciles the session's memory stores with their directories
   (below), inside the transaction that commits the results.
5. The commit that appends the result also enqueues the next `model_turn` — only once
   every tool use in the turn is answered. A brain claims it (brains wake by polling the
   queue; Postgres LISTEN/NOTIFY serves the SSE fan-out, not the brain), replays, and
   continues until the model stops calling tools, then writes `session.status_idle`
   with `stop_reason.end_turn`.

**Outcome grading** (plan 21). A session with an active outcome (`user.define_outcome`)
does not idle on `end_turn`: the settlement commits `span.outcome_evaluation_start`,
flips the outcome entry to `evaluating`, and schedules a grading cycle — on a cloud
environment by enqueueing an `outputs_harvest` item, whose executor-side settlement
snapshots `/mnt/session/outputs/` into the files registry (session-scoped rows,
downloadable through `GET /v1/files`) and chains the grading `model_turn`; on
self_hosted by requeueing the turn item directly (the platform cannot reach a BYOC
sandbox — grading is transcript-only there). The grading claim runs one grader call —
rubric + deliverables + transcript, no tools — and settles the verdict under the
session lock: satisfied/failed/max_iterations idle the session, needs_revision feeds
the grader's findings back and runs another agent cycle.

**Permissions / human-in-the-loop.** A tool whose resolved `permission_policy` is
`always_ask` suspends the session *before* execution: the brain writes
`session.status_idle` with `stop_reason:{type:"requires_action", event_ids:[…]}` naming
the blocked `agent.tool_use` events (stamped `evaluated_permission:"ask"`). A client
answers each with `user.tool_confirmation{tool_use_id, result:"allow"|"deny",
deny_message?}`; allow releases the tool to the queue, deny synthesizes an
`is_error:true` `agent.tool_result` carrying the deny message, and the turn resumes
either way. Every driver re-derives that gate rather than trusting the item it claimed:
the platform's exec drivers and the BYOC worker alike run only a call stamped `allow`,
or one stamped `ask` and released by a confirmation — a `deny` runs on neither — so a
turn suspended on two asks and released one at a time never runs the one no human
answered, on every session, single-agent and coordinator alike.

**Interrupting.** Not every stall has an owner to wait for: a `self_hosted` worker fleet
that never comes back, a custom tool the client never answers, a confirmation nobody
sends. `user.interrupt` is the one call that ends a turn from outside it — the control
plane answers whatever the turn left outstanding, settles the session `idle` on
`stop_reason.end_turn`, and cancels the session's work items so the claimant still running
it commits nothing. Sent together with a `user.message`, it is also how a client redirects
a working agent in one request.

**Session threads** ([plan 35](./plan/35_multiagent-threads.md)). Every session has a
primary `sthr_` thread, and a coordinator agent's roster spawns child threads — each with
its own agent, its own log and its own turn, all on the session's one sandbox, with at
most 25 threads live at once and the primary counted among them. The flow above then reads per thread: a turn is `(session, thread)`-keyed, the
`requires_action` suspension and the interrupt address one thread (an interrupt naming a
`session_thread_id` ends that thread alone and leaves the shared exec item; one without —
or one naming every live thread — cancels every thread's live work and re-idles the
session once), the exec drivers run only the runnable calls — every thread's sandbox
calls off the session's one `tool_exec`, the web and MCP lanes on their own items — and
wake each thread as its own calls are answered, outcome grading runs at
the session's quiescence, and the session's status is a fold over its threads' (running ≻
rescheduling ≻ idle; `requires_action ≻ retries_exhausted ≻ end_turn`, `event_ids` unioned).
Delegation is the settlement's own work rather than any driver's: a coordinator's primary
thread is offered `create_agent` / `send_to_agent` / `list_agents` / `wait_for_agents` and
every child `submit_result` / `send_to_parent`, and the transaction that commits the turn
calling one also does what it asked and answers it there — inserting a thread row, writing
the `agent.thread_message_sent`/`_received` pair the agents talk through, enqueueing
whatever turn follows. The session's skills are the roster's union, the coordinator's
references first and each member's after, deduplicated by skill id. On a `self_hosted`
session the session-level list and stream additionally carry every child thread's
`agent.tool_use` and the results answering it, which is how the thread-unaware BYOC worker
sees calls it must run on a thread it knows nothing about. A single-agent session is the
one-thread case of the same machinery and its wire is unchanged. What is inferred versus
documented is in [DIVERGENCES.md](./DIVERGENCES.md) ("Session status as a fold over
threads", "Coordinator delegation", "The `self_hosted` session view").

**Crash recovery is replay.** Sessions are never bound to a brain: any brain can pick up
any session's next turn from the log. A sandbox container dying surfaces as one
tool-call error; a worker dying strands its lease, which `poll` reclaims after expiry.

**Session resources** (plans 08 and 25). A session mounts things besides its agent's
skills: uploaded files, and `github_repository` entries cloned in-process by the executor
(go-git, so no `git` binary is in any image). Each is a `sesrsc_` row under
`/v1/sessions/{id}/resources` with the mount path it lands on; a private repository's
token is sealed through the same credential cipher the vaults use
(`session_resource_credentials`). Both halves work the same way at both ends — the
executor materializes them into the sandbox beside the skills, and the brain renders a
"Mounted files" / "Mounted repositories" block into the request so the model knows what
is there. A resource that has gone missing costs its own mount and a log line, never the
turn.

**Memory stores** (plan 36) ride the same array as an id-less element that snapshots
the store's name and its `/mnt/memory/<slug>` mount. On a `cloud` session the executor
lands the store's memories there before the tools run — `0666` files beside a marker
naming the store and a baseline recording what the directory and the store agreed on —
and the brain renders a "Memory stores" block after the repositories block. The
directory is reconciled with the store when a tool run ends, in three phases the shared
rules in `internal/memsync` decide: the tree is hashed in the sandbox, the plan settles
inside the transaction that commits the run (a push is a compare-and-set on the head's
digest and appends a `session_actor` version; the store wins a both-sides change; an
emptied directory against a baseline of several files is re-downloaded, never read as
deletions), and the settlement is written back. A `read_only` or archived store, or a
directory whose marker was altered, is pulled from and never pushed to, and the file
tools refuse to write there; the reaper syncs an idle sandbox before it is destroyed. A
`self_hosted` session cannot attach a store until the plan's slice 6.

**Sandboxes have a lifecycle** (plan 24). Provision is idempotent per session — it
returns, heals, or re-creates — and the **reaper in the executor is the single owner of
destruction**, on four tiers: a session `deleted`, `archived` or `terminated`, plus an
`idle` tier that reclaims a session idle past `EXECUTOR_SANDBOX_IDLE_TTL` with no work
owed. It needs no cross-replica coordination, because each executor sees only its own
daemon or namespace and reaping is idempotent; teardown is one interval behind its
trigger, which no wire surface can observe. Before the idle tier destroys a sandbox the
checkpoint engine captures the session's durable state — workdir, the persistent shell's
cwd/env, the published deliverables — as one gzipped tar in object storage, and the next
provision restores it into a fresh sandbox, so an idle-reaped session resumes where it
left off. Both halves run under the session advisory lock. Without an object store
configured the idle tier is disarmed rather than lossy.

## Wire-compatibility model

The public REST API mirrors Anthropic's Claude Managed Agents resource model — paths,
JSON fields, ID prefixes (`agent_` `env_` `sesn_` `sevt_` `work_` …),
pagination and error envelopes, and the `{domain}.{action}` event taxonomy (SSE deltas
use `content_delta`, not the Messages API's `content_block_delta`). The typed schema in
the pinned `anthropic-sdk-go` checkout is the ground truth; client behavior comes from
the `ant` CLI source (see [REFERENCE_PROJECTS.md](./REFERENCE_PROJECTS.md)). Where we
deliberately diverge — or infer behavior the references don't pin down —
[DIVERGENCES.md](./DIVERGENCES.md) is the single registry; the verifier resolves
wire-compat findings against it.

Not everything an operator needs is on that wire. The reference mints environment
keys from its **console**, on a private backend no SDK or CLI route touches, so a
self-hostable platform has to own that surface — and it lives under `/api/`,
deliberately outside `/v1`, where nothing can be mistaken for or collide with what
the real `ant` CLI speaks. The convention for such a surface is **mirror, don't
invent**: its paths copy the reference console's own segment-for-segment
(`/api/oauth/organizations/{organization_id}/environments/{environment_id}/tokens…`,
observed live 2026-08-10), the observation is dated in the plan, and every
departure from what was seen is registered in DIVERGENCES.md like any other. Auth
is the management `x-api-key`, now through `dispatchAuth`'s own `/api/` arm —
plan 30 reached the same lane by fallthrough (no other predicate can match an
`/api/` path, since each is either a `/v1/` prefix test or the gate's exact
equality against the fixed `/internal/v1/gate/config`) and asked that a future
off-`/v1` lane re-check that rather than inherit it; plan 31 slice 2 was that
lane and made the arm explicit. The namespace is off-wire and console-shaped,
not a separate permission tier for a key-authenticated caller — with SSO
configured it does become the first **role**-gated surface, at `admin`, which
constrains humans and changes nothing about what a management key may do.
Future console-facing endpoints follow the same rule rather than opening a
naming argument — and must re-check that auth reasoning rather than assume a
blanket `/v1/` prefix rule covers every lane.

Model access is **config-driven**: a provider is constructed from `protocol`
(`anthropic` | `openai`) + `model` + `base_url` + `api_key` (+ optional headers), and a
`model_providers` routing table maps an agent's model string to a provider instance.
The Anthropic-protocol adapter is near-zero-conversion and works against any endpoint
speaking Anthropic Messages; the OpenAI-compatible adapter is the platform's one lossy
conversion seam, confined to `internal/provider/openai` and tested hard. Providers are
built with `WithoutEnvironmentDefaults`, so ambient `ANTHROPIC_*` credentials can never
leak to a configured third-party endpoint.

## Package reference

A map, not a description — **the code is the reference.** Nearly every package
below carries more comment than this section ever spent on it, most by three to
ten times: `internal/api` 192 KB of comment against the 50 KB that was here,
`internal/executor` 169 KB against 43 KB, `internal/identity` 45 KB against 5 KB.
Two qualify that. `internal/version` is one variable and falls just under. And
`internal/store` only clears it by counting the SQL comments under
`migrations/`, which is right — the schema *is* those files — but they are the
one substantial body of comment `go doc` will not print for you. Everything else
sits beside the code it describes, so it cannot drift from it; what stood here
was a second, staler copy that did.

Read a package with `go doc ./internal/X`. That prints the package doc and the
exported surface, which is the whole story for the seam packages — but not for
`api`, `brain` and `executor`, whose substance is unexported handlers, loops and
consumers; read those files directly. Each package doc states its own invariants
and names its neighbours.

Layout order is by layer, as the repo is.

### Domain and wire surface

| Package | What it owns |
|---|---|
| `domain/` | The Anthropic-native types every other package speaks — ids and their wire prefixes, the event taxonomy, session, agent, environment, outcome. Stdlib only: no adk-go, no genai, no provider SDK, because the wire schema is authoritative here. |
| `api/` | The control plane's whole HTTP surface, and the dispatcher deciding which of four credentials reaches which route. Five surfaces share one `ServeMux`; auth runs before the router, never inside it. |
| `events/` | The append-only session event log — the single source of truth — plus per-session `seq` allocation, list queries, the Postgres LISTEN/NOTIFY broker behind SSE, the ephemeral preview frames, and the `span.*` events emitted from the same instrumentation point as the OTel spans. |

### Execution chain

| Package | What it owns |
|---|---|
| `brain/` | The stateless orchestration loop: claim a `model_turn`, replay the log into a provider request, stream the turn back as events, drive the session state machine at turn end. It runs no *agent* tool in-process — such a call is an emitted intent, and the turn resumes when the result event lands. The exception is the delegation six, which are the platform's own and are answered inside the settlement that emits them (plan 35 decision 6). |
| `provider/` | Model backends behind Anthropic Messages semantics, constructed purely from configuration. `anthropic/` works against **any** endpoint speaking Anthropic Messages; `openai/` is the platform's one lossy seam and confines every conversion; `providertest/` is the contract suite both must pass. |
| `queue/` | The work queue over Postgres (`FOR UPDATE SKIP LOCKED`). Five kinds share `work_items`, and two ways take one: `Claim` is the in-process lane, `Poll` the wire lane that serves exactly a `tool_exec` item on a `self_hosted` environment. That split is what makes the executor and the BYOC worker one protocol at two deployment points. |
| `executor/` | The platform-managed half of that protocol: pull work, run the built-in toolset in the session's sandbox, append the results the brain resumes on. Also the web and MCP work, which run in its own process for **both** environment kinds, and the outputs harvest, which only a `cloud` session enqueues — a `self_hosted` sandbox has no file lane to snapshot, so its grading stays transcript-only (`brain/grader.go`). |
| `worker/` | The customer-hosted twin. It holds no database handle and reaches everything over the wire with its environment key — which is what makes "customer compute, zero inbound network access" the same code path as the executor's. |
| `toolset/` | What the model is offered and how one call runs: the built-in `agent_toolset_20260401` (bash, read, write, edit, glob, grep), the `web_fetch`/`web_search` definitions and the `IsWebTool` predicate that routes them, the six delegation tools a coordinator session's threads are offered by role with the `IsDelegationTool` twin, and the `mcp_toolset` arm — validation, and resolving an entry's `default_config`/`configs[]` against a server's listing. Nothing here knows what a call means for the session; that is the executor's. |
| `sandbox/` | The "hands" boundary: a disposable per-session container, `docker/` and `k8s/` behind one interface with one contract suite (`sandboxtest/`), plus `shell/`, the persistent per-session bash built on the stateless primitives. |
| `webtool/` | The `web_fetch` / `web_search` seam (`tavily/`, `jina/`). These run in the executor's process on both deployment modes — never in the sandbox, never on the worker, never through the egress gate. |
| `mcp/` | The MCP client: a thin wrapper over the official go-sdk, whose types never reach the domain layer. Connections are per-work-item, so a crashed executor loses nothing a fresh one cannot rebuild. |

### Egress, credentials and identity

| Package | What it owns |
|---|---|
| `egress/` | The substitution engine that rewrites vault placeholders into secrets, and the host matcher both it and the gate use. It holds no I/O, and that is the invariant to keep. |
| `gate/` | The per-session forward proxy the sandbox reaches through `HTTP_PROXY`: the enforcement point where the networking policy admits a host and substitution rewrites placeholders on plain HTTP. HTTPS rides through as an opaque CONNECT tunnel. |
| `gaterun/` | That gate's runtime — firewall, privilege drop, the config fetch-and-swap loop. The OS-touching adapters live in `cmd/gate` behind seams declared here, so everything with logic stays testable off a container. |
| `gateconfig/` · `gatetoken/` | The internal gate-config endpoint's client and shared wire contract, and the per-session `gtk_` tokens that authenticate to it. Off the public wire, and registered as a divergence. |
| `vaultresolve/` | Credential resolution at read time: a session's vault ids become the sandbox's placeholder bindings and the gate's decrypted secrets, under one selection rule so the two halves can never disagree, plus a third lane resolving an MCP endpoint's bearer by URL. Current rows every time, so rotation needs no session restart. It also performs the **one write** in this area — the OAuth refresh grant reseals the rotated token onto the credential row under a compare-and-set. |
| `secrets/` | The credential-cipher seam — `openbao/` in production, `gcpkms/` for GCP, `local/` (AES-256-GCM, one configured master key) for tests **and bao-less minimal deployments** — with the ciphertext persisted by the caller, never here. |
| `dialguard/` | The SSRF floor under the dials whose URL a **customer** supplied — an agent's `mcp_servers` entry, a vault credential's MCP server or token endpoint. **Not a universal egress check**, and reading it as one misplaces three neighbours: a configured provider or web backend dials with its own ordinary client, the `github_repository` clone's host is pinned to the literal `github.com` by the create-time grammar rather than by an address check, and the per-session gate admits hosts by the environment's network policy. The check runs on the resolved IP at connect time, so DNS rebinding cannot slip past a name that resolved innocently. RFC 1918 is deliberately allowed — this platform runs in the operator's own network. |
| `identity/` | The human-authentication boundary: verify the credential a human presents and reduce it to a principal holding one of three roles — or `RoleNone`, the fourth value and a live denial, which is what an authenticated human whose claims mapped to nothing receives and what no route minimum accepts. Machine credentials never come here. `identitytest/` is its fake OpenID Provider. |
| `oauthrefresh/` | The RFC 6749 refresh grant, spelled once for the two places that perform it. |

### Storage and shared infrastructure

| Package | What it owns |
|---|---|
| `store/` | The Postgres schema. Every table lives in `migrations/`, embedded in the binaries so a deployment needs no migration step. Three properties of `Migrate` are contract rather than detail — one all-or-nothing transaction, an advisory lock, and the filename as the immutable version record — and the schema reserves the multi-tenant columns it does not yet enforce. |
| `blob/` | The object-storage seam: `s3/` for anything speaking S3, `gcs/` native on Application Default Credentials, one contract suite for both. |
| `skills/` | Skill-upload validation and canonical-zip normalization, funnelled through one place so the rules cannot drift between entry points. |
| `memsync/` | What every writer of a memory store agrees on, so the halves of plan 36 cannot drift — `internal/api` today, the executor and the BYOC worker from slice 4 on: the path and content rules every memory write obeys and the mount-path slug (slice 2); the marker, baseline and tree-hash conventions and the pure `Plan(local, baseline, remote)` decision table arrive with the sync (slice 4). |
| `telemetry/` | OTel tracing and metrics init, and W3C trace-context propagation. The `span.*` domain events come from the spans started here, so the two views never drift. |
| `mimetab/` | The pinned extension → MIME table both writers of the files registry consult, so the serving host never decides a wire-visible value. |
| `sandbox/backend/` · `blob/backend/` · `secrets/backend/` | Where a deployment's backend is *chosen*, one selector per seam, so every binary constructs it from the same config point. Each is a sibling rather than part of its seam because the seam package holds the interface **and** the sentinel errors backends wrap, so it must not import them. |
| `version/` | The build-time version stamp. A bare variable on purpose: there is no version endpoint, which would be net-new wire surface. |

### Test support, and `cmd/`

Test-support packages are excluded from the coverage denominator and production
code must never import them: `pgtest/`, `dockertest/`, `modeltest/`, and the
per-seam pairs — `sandbox/sandboxtest/`, `blob/blobtest/`, `blob/gcs/gcstest/`,
`provider/providertest/`, `secrets/secretstest/`, `secrets/gcpkms/gcpkmstest/`,
`webtool/webtooltest/`, `identity/identitytest/`, `mcp/mcptest/`. Two rules run
through them, each over its own half of the list. The ones that start a
container — `pgtest`, `dockertest`, `sandboxtest`, `blobtest`, `gcstest`,
`secretstest` — treat a missing Docker daemon as a hard failure rather than a
skip, because a skipped contract test hollows out the coverage gate silently;
the rest serve an in-process fake (an httptest server, a fake gRPC endpoint, a
fake OpenID provider) and need no daemon. And the ones gating a paid tier take
consent from an environment variable, never from the presence of a configured
`.env`; once given, missing configuration fails rather than skips.

The four server binaries under `cmd/` — `controlplane`, `brain`, `executor`,
`worker` — are thin glue: they map the environment to a config and call into the
packages above. `cmd/gate` is not a server but the per-session egress sidecar,
and holds the two OS-touching adapters `gaterun/` declares.

## Security invariants

- **Credentials never enter the sandbox.** Tool credentials (vaults) reach the wire only
  at egress time: the sandbox sees opaque `vltph_` placeholders, and the per-session gate
  substitutes the real value on admitted plain-HTTP egress alone (both backends when the
  executor opts in; in-sandbox HTTPS keeps its placeholders until #166). Model
  keys live in the brain's provider config; the sandbox sees none of them. Provider adapters redact the credentials they were configured with
  — the api key, a `base_url` userinfo password, an auth header — out of the errors that
  quote an endpoint (`internal/provider/redact.go`), so an endpoint echoing the request's
  auth header back cannot land the key in a `session.error` event, which is append-only
  and re-served to clients. Matching is verbatim against the configured value in each
  form it is known to reach an error in — decoded, percent-encoded, base64 in a derived
  `Authorization: Basic`, and as written — and by design does not chase a credential an
  endpoint re-encodes into some form of its own. A model's *successful* output is a
  trusted boundary and is never redacted: it is the content the session exists to record.
  `provider.Config` has no redacting `String`, so it must not be formatted whole.
- **A session is not a context window.** The harness may replay, slice, or rewind the
  event log before feeding the model; context strategy is never baked into an
  irreversible compaction.
- **A thread is a concurrency boundary, not a security boundary.** A session's threads
  share the session, and that is the whole of what they share: the one sandbox its
  sandbox-executed tools run in, the vault bindings fixed when it was created (an update
  cannot add one), and the environment whose egress policy applies. The lanes that never
  enter a sandbox are the session's too — web and MCP work runs in the executor process,
  a delegation call is answered in the settlement itself — so a child inherits the blast
  radius of the session that spawned it, and delegation confers no authority that session
  did not already hold. What a member *may* do is set by the roster snapshot taken at
  session create, which pins each member's tools, MCP servers and the `permission_policy`
  declared inside them; the one exception is the coordinator's own `self` copy, which a
  session update patching `agent.tools` or `agent.mcp_servers` rewrites. `create_agent` is
  honored for a roster name and a task and nothing else, so a coordinator chooses *which*
  member runs and *what* it is asked to do, never what it is allowed to do: the unit of
  trust is the session, and a roster should hold only agents you would have run in it
  directly.
- **Auth is scoped.** Management calls carry `x-api-key` (hashed at rest,
  rotation-by-restart); workers carry an environment key scoped to exactly one
  environment's work queue — a worker can neither read nor write another environment's
  sessions. Environment keys are hashed at rest too, issued one per host so a
  compromised host is revoked alone, and expire a year after issue; revoked,
  expired and unknown are one indistinguishable 401. Issuing and revoking them is
  a **management** operation on the off-wire console API, so an environment key
  can never mint or retire another — but equally, that surface delegates no
  authority the management key did not already hold, and is not a separate
  permission tier.
- **Humans authenticate through the deployment's own IdP, and the platform stores no
  authority** (plan 31, #56). Off by default: with `IDENTITY_MODE` unset or `disabled`
  no JWT is accepted anywhere and the surface is the machine-credential platform
  above — byte-for-byte on every request shape but one, a repeated `x-api-key`
  field, which is refused as ambiguous in every mode rather than resolved by header
  order (`server.go`). Configured, a human's token is verified against its issuer's JWKS —
  algorithm allowlist, exact `iss`, `aud` containment plus `azp`, required `sub`/`exp`,
  keys only from the key set and only while its bounded lifetime holds — and never
  minted here; every **credential** rejection is one indistinguishable 401, which is
  what makes the 500 from a failed principal provisioning meaningful rather than
  noise: it says the credential was good and the database was not, so an operator is
  not sent hunting the IdP for an outage. The machine lanes always
  resolve first, so a second credential can never change which lane, and so which role
  check, a request takes. The request's role is computed from the IdP's claim on **that
  request** through the operator's map: `principals` records who signed in, never what
  they may do, so revoking a group at the IdP takes effect on the next request rather
  than after someone remembers to mirror it. Enforcement is one check at the control
  plane, stated per route as a minimum role, and fails closed — an unannotated route
  denies every human, as does a human whose claims mapped to nothing.
- **Sessions are not bound to an end-user.** Scoping keys are org/workspace/project
  (reserved, single-tenant defaults in v1); end-user ownership is an application-layer
  concern hooked on session `metadata` and the audit-only `created_by`.
- **The container is the boundary.** Tools run inside the per-session sandbox with no
  host filesystem access; the toolset does no lexical path confinement that a `bash`
  call could walk around, because the container itself is the wall. That wall is
  hardened when it is built, not left to the operator: every sandbox is created with
  cgroup limits and capability drops (`sandbox.Hardening`, defaults on), which is also
  the only containment for a process that escaped the exec deadline's process-group
  kill by calling `setsid`.
- **The platform runs nothing in a sandbox as anyone but the sandbox's own user.** No
  privileged exec, on either backend — the operator's `SANDBOX_RUN_AS_USER` (or the
  image's own user) is who every command runs as, cleanup included. One place had to be
  designed around it: docker's daemon lands a write's temporary as root, so a refused
  write leaves one where the sandbox uid cannot unlink, and the obvious `exec -u 0`
  cleanup was measured to be unboundable — `docker exec -u 0` starts with `AT_SECURE=0`
  and runs the image's own binary and libraries, so an image's `BASH_ENV`, `LD_PRELOAD`
  or `LD_DEBUG_OUTPUT`, or an `/etc/ld.so.preload` no environment setting closes, all
  reach it. The daemon empties what it landed instead, executing nothing (#310) — and
  the same for a **batch**, whose two sheds now name on stdout what their own `rm` could
  not take so the daemon can empty what is left in one archive (#316) — best effort, and
  with the exec-error branch's carve-out, exactly as the single write's. What
  pins the invariant is structural — docker's `execConfig` carries no `User` field to
  set, so no exec can name one — with a real-image row
  (`TestTheRootShedRunsNoAgentCodeOnANonRootImage`) covering the shell-hook channel
  behind it; what an operator must configure around it is docs/self-hosted-security.md.
- **A skill archive is verified before it is extracted.** The registry records the
  archive's sha256 at upload (Postgres) while the bytes live in object storage; both
  materialization halves check the object they read back against that digest before
  extracting it into a sandbox — the executor from the version row, the BYOC worker from
  the `x-skill-archive-sha256` download header, since it never touches the database.
  Bit-rot, truncation, and whole-object substitution are refused (`corrupt` outcome,
  skipped like any other per-skill miss rather than faulting the run). Two limits are
  deliberate: a version predating the digest column records none and is read unverified
  (logged), and a digest served by the same control plane that serves the bytes proves
  storage integrity, not provenance.

How these invariants divide between what the platform enforces and what a self-hosting
operator must configure — sandbox image hardening, capability drops, non-root execution,
read-only rootfs, egress policy, environment-key rotation — is
[docs/self-hosted-security.md](./self-hosted-security.md).

## Observability

OpenTelemetry is built in, not bolted on. Trace context propagates as W3C `traceparent`
across every process hop that continues a trace — HTTP headers between processes, and a
`trace_context` column that carries a turn's trace through `tool_exec` work items to
executors and BYOC workers (a `model_turn` item deliberately stores none: nothing reads
it back; the brain's `model_turn` span roots the turn's own trace instead) — so one
session's turn is one trace across process boundaries. All three claimants of the work
queue wrap a claimed item in a consumer span standing for its handling end to end — the
brain's `model_turn`, the executor's and the BYOC worker's `tool_exec` — and each reports
its item-handling faults from inside that span, so those logs are reachable from the red
span they describe. (The worker's heartbeat path is the exception and stays uncorrelated:
its lease-loss warnings are logged outside the run's span, and a cancellation-caused
reclaim deliberately leaves the span unset.) Anthropic
`span.*` wire events and OTel spans are emitted from the **same instrumentation point**
(they cannot drift); the business metrics ride the same points — model-request duration,
token-usage counters and the cache-token breakdown in `internal/events/metrics.go`,
session-status transition counts and approval (HITL) wait in
`internal/events/statusmetric.go`/`approvalmetric.go` (both recorded **after** the
transaction commits, so a rolled-back transition counts nothing), time-to-first-token
measured from the work claim in `internal/brain/telemetry.go`, tool-run duration in
`internal/toolset/telemetry.go`, skills-materialization outcomes/duration in
`internal/executor/telemetry.go` and `internal/worker/telemetry.go` (same instrument
names, two scopes), and the executor's memory-store instruments beside them
(`memory.materialized`, `memory.sync.actions` by pulled/pushed/deleted/conflict/refused,
and the two durations). Queue `depth`/`pending`/`workers_polling` are OTLP
observable gauges (`internal/queue/metrics.go`) sampling the same work-stats view the API
serves — registered once by the control plane, reported per self_hosted environment. A
configured OTLP endpoint bridges
`slog` records — trace-correlated where a span is in reach — to the collector.
`internal/telemetry` owns init, propagation, and the shared process startup/exit sequence
that keeps a binary's fatal-exit log ahead of the flush that would drop it; an empty
endpoint is a fully-offline no-op.

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
`internal/sandbox/sandboxtest` (a backend that declares the gate seam also runs the gated
egress rows against the real gate image), and both model-provider protocol adapters pass
`internal/provider/providertest` (the cross-provider invariants — stream termination,
`tool_use` stop, usage-nil-only-when-the-endpoint-reported-none, cancellation, `Close`;
the wire request shape, redaction, and the OpenAI lossy conversions stay per-package) — a
new backend inherits the whole battery. The queue still has one production implementation
(Postgres) contract-tested in its own package; a second queue backend would owe the same
extraction. The merge gate is `make verify`
(build, linux/arm cross-compile, vet, gofmt, `go test -count=1`, and **≥90% total
statement coverage** over the logic packages of `./internal/...`). On top sits the eval
system (`make eval`, [plan 02](./plan/02_evals-system.md)): nineteen deterministic regression
tasks driving whole sessions through the public API against a real model, graded
code-only with per-trial nonces and Platform/Model/Either failure classing. `repo-answer`
is the one trial whose expected answer is not nonce-derived — it lives in a fixed private
GitHub fixture repository, whose privacy stands in for the nonce a fixed remote cannot
carry (#358). The registered set is pinned by an offline test, so adding or dropping a trial
fails `make verify` until that pinned list moves with it, and the failure names the two
documents — this one and README.md — whose spelled-out counts must move too. The tier stays out
of the merge gate — it spends money and minutes — but a scheduled workflow
(`.github/workflows/evals.yml`, daily plus manual dispatch) runs it against the `MODEL_*`
and `EVAL_GITHUB_REPO_*` secrets of the `evals` deployment environment — which admits only the default branch, so a
dispatch cannot borrow the credential onto an unreviewed ref — and a break in the
whole-session path therefore surfaces on the next scheduled run instead of at whoever's next
manual one. That job carries the same fail-not-skip rule: with the secrets unset it is red,
never green-and-silent — a green job means the suite ran and passed, never that it was skipped.

Beside the evals sits the top-level `acceptance/` suite (plan 21): the define-outcomes
doc example driven end-to-end through the latest Go SDK. Its deterministic rehearsal —
a scripted model standing in for the real one — runs in the merge gate like any other
suite; the live variant (`TestLiveDefineOutcomesAcceptance`) points the same harness at
a running compose stack and a real model, consented by its own tier variable
`RUN_LIVE_ACCEPTANCE_TESTS` (whole sessions cost an order of magnitude more than the
`RUN_LIVE_MODEL_TESTS` single-turn smoke, which therefore must not buy them) with the
`ACCEPTANCE_*` variables naming the stack's key, model, and base URL.

One more check sits on that same in-gate / out-of-gate seam, over documentation rather
than code. `tools/registrycheck` holds [DIVERGENCES.md](./DIVERGENCES.md)'s pointers to the
grammar its own Format legend promises. Most of that grammar is readable from the file alone —
a clause parses at all, an INFERRED entry has a live tracker, a tracker several entries share
says what *this* one leaves open, and no cross-reference is a bare line number (one had drifted
77 lines before anyone noticed) — so those rules run in the gate as that package's own test,
the shape `internal/modeltest`'s README-tier test already uses. What is not readable from the file is
the part that rots: whether the issue a live `Tracked: #N` names is still open, and whether
GitHub still knows the issues its provenance cites — events no file in this repository can
see. That half is
`make registry-check` and [`registry.yml`](../.github/workflows/registry.yml), daily and on
every pull request that touches the registry.
