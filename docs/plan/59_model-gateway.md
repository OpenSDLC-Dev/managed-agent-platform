---
status: draft
---

# Model gateway: a standalone, Anthropic-native model service (plan 59)

Today the brain is the only thing that reaches a model. `cmd/brain` reads a static JSON
routes file once at start (`internal/provider/config.go` `LoadRoutes`) into a registry
that is immutable after construction, so changing a route means restarting every brain;
the upstream keys live in the brain; nothing records who used which model; nothing
outside the brain can use the routes; and the console's model field is free text because
no catalogue exists. After this plan, `cmd/modelgateway` is a separately deployable,
horizontally scalable server that speaks Anthropic Messages (the default) and the
OpenAI-compatible APIs to two kinds of caller — the platform's brain and an enterprise's
internal applications — routes each request to a configured upstream, and is configured
from managed-agent-console. Its first upstreams are the official cloud APIs of DeepSeek,
MiniMax, Zhipu (BigModel / Z.ai) and Moonshot (Kimi).

Scope decisions settled with the user on 2026-10-04:

1. **Build it, borrowing ideas from bifrost; neither deploy nor embed bifrost.** Three
   approaches were weighed: deploying bifrost as-is, embedding `bifrost/core` behind a
   server of our own, and building our own. The first two put an OpenAI-shaped schema
   between an Anthropic caller and an Anthropic-speaking upstream, which design
   principle 1 rules out for the default path; open-source bifrost also cannot run
   several replicas on one Postgres and keeps SSO/RBAC in its closed enterprise edition,
   where the platform already has both. Evidence under Ground truth.
2. **This repository, a new binary in the shared `server` image**, run as
   `command: /modelgateway` the way the four server binaries are, and released with the
   platform. A separate image waits for someone who deploys the gateway alone and needs
   it smaller.
3. **v1 upstreams are the four vendors' official cloud APIs.** Gemini, Vertex (#236) and
   self-hosted engines (vLLM, SGLang, …) follow in later plans, with bifrost as the
   reference (Later upstreams says for what).
4. **Governance serves internal applications:** API keys, per-key usage and cost,
   per-key RPM/TPM limits. No budgets, billing, teams or tenants.
5. **Inbound surfaces:** Anthropic Messages in full — `POST /v1/messages` (streamed and
   not), `POST /v1/messages/count_tokens`, `GET /v1/models` and `GET /v1/models/{id}` —
   and, on the OpenAI side, Chat Completions, Embeddings, Models and Responses, the last
   **stateless**: no response is stored and `previous_response_id` is refused.
6. **All agent model traffic goes through the gateway.** The brain keeps one route,
   `*` → the gateway; the static routes file stays the configuration of a deployment
   that runs no gateway.
7. **Models are configured in managed-agent-console**, against the gateway's own admin
   API.
8. **Thinking persistence and replay is a separate plan, done first (#67).** DeepSeek's
   models think by default ("Thinking mode is enabled by default"), MiniMax M2.x and
   GLM-5.3 cannot turn thinking off, and Claude 5-generation models think by default
   too. A thinking model needs its thinking blocks back within a
   tool-use turn — DeepSeek answers 400 without them — but the brain stores no thinking
   content and does not build its requests append-only (`system.message` text is folded
   into the system prompt; the `web_search` description carries the date). That plan
   gates slice 5 (the brain cutover), not the gateway.

Out of scope: semantic caching, an MCP gateway, guardrails, budgets and billing, teams
and tenants, stored Responses state, audio/image/video generation, batch and files APIs
on the gateway, injecting `cache_control` breakpoints, and a `GET /v1/models` on the
control plane — create-time model validation stays as docs/DIVERGENCES.md's
`model`-at-create entry describes.

## Ground truth (verified 2026-10-04)

**Wire shapes.** The Anthropic surfaces follow CLAUDE.md's resolution order: public
docs, then anthropic-sdk-go at the `go.mod` pin, v1.70.1 (`message.go`; `model.go`,
whose `ModelInfo` carries `capabilities`, `max_input_tokens` and `max_tokens` beside
`id`, `display_name` and `created_at`). The OpenAI surfaces follow OpenAI's public API
reference, the source `internal/provider/openai` already cites; slice 4 adds the
official `openai-go` SDK to docs/REFERENCE_PROJECTS.md as their typed schema.

**Thinking.** Anthropic's [thinking docs](https://platform.claude.com/docs/en/build-with-claude/thinking#preserving-thinking-blocks):
"Required: within a tool-use turn, pass thinking blocks back", complete and unmodified,
a modified block being a 400; and from Claude Fable 5.1 a block stays valid only while
the `system`, `tools` and earlier messages are unchanged
([preserved thinking](https://platform.claude.com/docs/en/build-with-claude/preserved-thinking)).
DeepSeek: "For requests carrying the `tools` parameter, the `reasoning_content` must be
fully passed back to the API in all subsequent requests … the API will return a 400
error" ([thinking mode](https://api-docs.deepseek.com/guides/thinking_mode)). Kimi:
"pass the thinking block from the response (including `signature`) back unchanged"
([messages](https://platform.kimi.ai/docs/api/messages.md)). MiniMax: thinking blocks
must be preserved unchanged
([Anthropic API](https://platform.minimax.io/docs/api-reference/text-anthropic-api)).

**Vendors.** Each publishes an Anthropic-compatible endpoint beside its
OpenAI-compatible one. The CN and international sites are separate consoles issuing
their own keys — the user's MiniMax account is on the CN site — so a profile carries
both regions and a provider names one; whether MiniMax's CN key also works on the
international host is the live tier's to show. OpenAI hosts are listed where documented.

| Vendor | Anthropic endpoint, CN · international | OpenAI endpoint | Field-support table |
|---|---|---|---|
| DeepSeek | `https://api.deepseek.com/anthropic` (one host) | `https://api.deepseek.com` | [published](https://api-docs.deepseek.com/guides/anthropic_api) |
| MiniMax | `https://api.minimax.cn/anthropic` · `https://api.minimax.io/anthropic` | `https://api.minimax.io/v1` | [CN](https://platform.minimaxi.com/docs/api-reference/text-anthropic-api), [international](https://platform.minimax.io/docs/api-reference/text-anthropic-api); two pages disagree on `tool_choice` |
| Zhipu (BigModel · Z.ai) | `https://open.bigmodel.cn/api/anthropic` · `https://api.z.ai/api/anthropic` | `https://open.bigmodel.cn/api/paas/v4` · `https://api.z.ai/api/paas/v4` | **none** |
| Moonshot | `https://api.moonshot.cn/anthropic` · `https://api.moonshot.ai/anthropic` | `https://api.moonshot.ai/v1` | [published](https://platform.kimi.ai/docs/api/messages.md) |

The differences that shape the profiles: `tool_choice` coverage (Kimi has no `tool`
type); sampling parameters (Kimi's schema has none, MiniMax errors outside [0,2]);
thinking that cannot be disabled; `cache_control` honoured three ways (ignored by
DeepSeek, top level only on Kimi, up to four breakpoints on MiniMax); refused content
blocks (DeepSeek: `search_result`, `document`, `redacted_thinking`; MiniMax:
`search_result`, #565); usage fields needing normalization (DeepSeek
`prompt_cache_hit_tokens`, GLM `cached_tokens`); request paths off the `/v1` convention
(Zhipu's OpenAI side is `/api/paas/v4/chat/completions`); and Zhipu Coding Plan keys,
which work on the OpenAI protocol only. The self-hosted engines serve `/v1/messages`
too (vLLM from 0.11.1, SGLang from 0.5.9, Ollama from 0.14, LMDeploy from 0.13), so the
later engine profiles ride the same passthrough path.

**bifrost** ([maximhq/bifrost](https://github.com/maximhq/bifrost) at `3b31be003`,
Apache-2.0):

- Its unified schema is OpenAI-shaped. `/anthropic/v1/messages` always converts to a
  Responses-shaped request first (`core/providers/anthropic/responses.go:4034`); raw
  passthrough needs a Claude Code client calling a Claude model
  (`transports/bifrost-http/integrations/anthropic.go:899`). The converted path keeps
  only `user_id` of `metadata` (`core/providers/anthropic/types.go:729`).
- MiniMax, Zhipu and Moonshot are not providers there.
- "Running multiple OSS Bifrost nodes with a Postgres backend is not supported"
  (`docs/deployment-guides/how-to/multinode.mdx`); governance state lives in process
  memory (`plugins/governance/store.go`).
- It reads an inbound `traceparent` but `InjectTraceContext`
  (`framework/tracing/propagation.go:187`) has no caller, so none goes upstream.
- Ideas taken here: weighted key selection, retry with jittered backoff before the first
  byte, ordered fallbacks, per-provider request paths, a model catalogue with prices,
  virtual keys with limits, and later its Gemini/Vertex converters. No code is copied;
  if a later converter ever is, NOTICE and THIRD_PARTY_LICENSES carry it.

**Anthropic's gateway docs.** The
[gateway compatibility guide](https://code.claude.com/docs/en/llm-gateway-protocol) is
the contract a gateway owes Claude Code, and this gateway's Anthropic surface follows it:
forward `anthropic-version` and `anthropic-beta` "verbatim; don't allowlist individual
values", and pass `anthropic-*` headers and body fields through as open lists; stream
without buffering, keep `ping` events (Claude Code aborts a stream silent for five
minutes by default) and deliver each event sequence whole; forward the `system` array
unchanged, since its first block is a positional attribution block, and error bodies
unmodified, since Claude Code's recovery "matches on the upstream's error wording";
answer `retry-after` in integer seconds. Token counting is optional — "when they're
absent, Claude Code falls back to a character-based estimate". Model discovery calls
`GET /v1/models?limit=1000` with a 3-second timeout, follows no redirect, and keeps only
ids containing `claude` or `anthropic`. Requests carry `x-claude-code-session-id`, which
a gateway may consume for attribution.
[Claude apps gateway](https://code.claude.com/docs/en/claude-apps-gateway), Anthropic's
own self-hosted gateway inside the `claude` binary, translates Anthropic Messages for
Claude upstreams only (Amazon Bedrock, Claude Platform on AWS, Google Cloud, Microsoft
Foundry, the Anthropic API), so it cannot front the four vendors and is not a fourth
approach. It does confirm two choices below: its rate-limit counters live in Postgres,
and it runs its migrations at boot under a lock and refuses to start on a bad
configuration or an unreachable dependency.

## Architecture

### Process and packages

`cmd/modelgateway` is stateless; any number of replicas sit behind one Service. (Not
`cmd/gateway`: `cmd/gate` is the egress sidecar, and the two must not be confusable.)
Packages live under `internal/modelgateway/` — `catalog` (configuration snapshot, alias
resolution, target selection, reload), `profile`, `upstream` (passthrough and streaming
relay per protocol), `convert`, `store` (schema, migrations, queries), `admin` — with
the HTTP surfaces at the root.

It imports `internal/provider` for `StallGuard`, `Redactor` and `SearchResultText`
only: the brain's `Provider` interface is too narrow for a gateway (a single system
string; no sampling parameters, `tool_choice`, signatures or streamed tool input). The
Anthropic ↔ Chat Completions conversion in `internal/provider/openai` moves to
`convert`, which both then use. It also imports `internal/secrets`,
`internal/identity` and `internal/telemetry`, and never `internal/domain`,
`internal/events` or `internal/api`: the gateway knows nothing of agents or sessions
beyond an opaque session-id header, which is what keeps it usable without the platform.

### Two request paths

Anthropic Messages is the **pivot**: every conversion goes through it, and nothing
converts when protocols match.

- **Passthrough** — the inbound and upstream protocols match. The body is decoded only
  as far as its top-level keys and the content blocks a profile touches; `model` becomes
  the deployment's upstream id, the profile's edits apply, and everything else, unknown
  fields included, goes out equal as JSON; `anthropic-*` headers go with it verbatim.
  The response relays event by event as it arrives — no buffering, `ping` events and
  keep-alive comments kept, each sequence whole — rewriting only `message.model` (back
  to the alias the caller sent) and the usage fields the profile normalizes. A
  conversion path whose upstream sends no pings emits its own during silent gaps. A
  provider configures both of its vendor's endpoints
  and selection prefers the one matching the inbound protocol, so Anthropic and Chat
  Completions callers both pass through to all four v1 vendors.
- **Conversion** — the protocols differ. v1 needs two directions: Responses (inbound) ↔
  Anthropic (upstream), since no v1 vendor serves Responses — reasoning items'
  `encrypted_content` and summary map to the thinking block's `signature` and text, so a
  stateless caller's tool loop keeps its thinking — and Anthropic (inbound) ↔ Chat
  Completions (upstream), for a credential usable on the OpenAI protocol only. Chat
  Completions inbound to an Anthropic-only upstream has no v1 case and waits for one.
- **Embeddings** have no Anthropic counterpart: OpenAI passthrough to deployments of
  kind `embedding`.

### Vendor profiles

A profile is declarative data plus at most a few Go hooks, compiled in: `deepseek`,
`minimax`, `zhipu`, `moonshot`, and `anthropic-generic` / `openai-generic` for any
conformant endpoint (the later engine profiles join these). It names the request path
per protocol, the CN and international hosts, the auth header, content-block edits,
field strips, the usage mapping, and whether `count_tokens` exists — where it does not,
that alias's `count_tokens` answers `404 not_found_error` and the client estimates, as
the compatibility guide says Claude Code does.

The edit policy, which keeps a profile from quietly changing what a caller asked for:

- **Every edit is deterministic and leaves `system` alone,** so an upstream sees one
  stable prefix across a conversation's requests — what preserved thinking checks, and
  what keeps Claude Code's attribution block first.
- **Pass through** by default; the upstream's own error reaches the caller, redacted.
  What the docs leave uncertain (MiniMax's two `tool_choice` pages; every Zhipu field)
  passes through until evidence says otherwise — the live tier, for a vendor it has a
  key for.
- **Edit** only what (a) the platform's own traffic needs — `search_result` flattened to
  text through `provider.SearchResultText` where a vendor refuses it, which is the
  brain's `flatten_search_results` moved behind the gateway — or (b) the vendor
  documents as ignored, where dropping it changes nothing.
- **Refuse** at the gateway only where a vendor documents that it silently ignores a
  field whose absence changes the result (DeepSeek ignores `disable_parallel_tool_use`),
  decided per field in the slice, citing the evidence in the profile. Nothing is
  downgraded silently, as plan 53 decided for effort.

### Configuration model

Postgres schema `modelgateway`, with its own embedded migrations and its own
`schema_migrations` (CLAUDE.md's immutability rule applies); `DATABASE_URL` may name the
platform's database or another. Ids carry gateway-local prefixes (`gwprov_`, `gwcred_`,
`gwdep_`, `gwkey_`), deliberately outside `internal/domain`'s wire list. Every table
reserves `org_id`, `workspace_id` and `project_id` with single-tenant defaults, as the
platform's own do (`internal/store/migrations/0001_init.sql`, design principle 5).

- **provider** — profile, name, endpoint per protocol (a profile host or a custom one),
  extra headers, stall timeout, enabled.
- **credential** — a provider's key, as ciphertext and key id from `internal/secrets`
  (the vault credentials' backend selection); `kind` (`api_key`, the only v1 value);
  last four characters for display; the protocols it may be used on; weight; enabled.
  Write-only through the API.
- **deployment** — a provider's upstream model id; kind (`chat` | `embedding`);
  capabilities (tools, thinking, vision, `max_input_tokens`, `max_tokens` — the
  `ModelInfo` fields `/v1/models` answers from); prices per million tokens for input,
  output, cache write and cache read, entered by the operator — no remote price sync,
  so an air-gapped install works.
- **alias** — the model name a caller sends: a display name and targets
  `[{deployment, priority, weight}]`. Exact match first, then an optional `*` alias. A
  `claude-*` alias is only a name, which is how clients that hard-code Claude names
  reach a vendor model.
- **api_key** — name, SHA-256 of the secret (shown once), prefix `sk-map-gw01-`
  (siblings: `sk-map-api01-`, `sk-map-env01-`), optional alias allow-list, RPM and TPM
  limits, expiry, revocation.
- **usage** — one row per request: key, alias, deployment, credential, session id,
  inbound protocol, status, the four token counts, cost at the prices in force, latency,
  time to first token.
- **rate_window** — one-minute windows per key: requests and tokens.

Every admin write commits a `NOTIFY modelgateway_config`; each replica reloads its
snapshot on the notification and on a periodic tick, so a missed notification heals. The
request path reads only the snapshot.

### Routing, retries, limits

- Resolve the alias; in the highest-priority group with a healthy target, choose a
  deployment by weight, then a credential by weight.
- **Affinity.** A request carrying a session id — `X-MAP-Session-ID` from the brain,
  `x-claude-code-session-id` from Claude Code, also the usage row's — chooses by
  rendezvous hashing over
  the group, so a session stays on one deployment while it is healthy: thinking blocks
  are bound to the backend that produced them.
- **Retry and fallback happen before the first byte only.** A connect error, 429, 5xx
  or overload moves to the next credential, then the next group, within a bounded
  attempt count and jittered exponential backoff. After the first byte a failure is the
  caller's (`event: error` on a stream). A request whose history carries thinking blocks
  falls back only to a deployment of the same upstream model; otherwise the error
  surfaces rather than one model's thinking being replayed to another.
- **Stall.** `provider.StallGuard` per provider, for #121's reasons unchanged.
- **Limits.** Admission increments the key's request count for the current minute in one
  upsert and refuses over the limit with 429 and `retry-after` in integer seconds; tokens
  are added when the
  response ends, so TPM admits against tokens already spent, never against an output not
  yet known. Per-replica in-memory buckets were rejected: under an autoscaler the
  effective limit would be a function of the replica count.

### Auth, admin API and `/v1/models`

- **Inference** takes a gateway key in `x-api-key` or `Authorization: Bearer` (Anthropic
  SDKs send the first; OpenAI SDKs and `ANTHROPIC_AUTH_TOKEN` the second). Both, and
  every `X-MAP-*` header, are stripped before the upstream call.
- **The admin API** lives under `/admin/v1/`: providers, credentials, deployments,
  aliases, keys, usage queries, and the profiles (read-only). No reference surface
  corresponds to it, so it is ours to shape. It accepts an admin key
  (`MODELGATEWAY_ADMIN_KEY`) or, with identity configured from the same `IDENTITY_*`
  settings the control plane reads, the operator's own token: `viewer` and `developer`
  read, `admin` writes.
- **`/v1/models`** is one path with two shapes. The root answers in Anthropic's shape
  when the request carries `anthropic-version` — which every Anthropic SDK sends and no
  OpenAI SDK does — and in OpenAI's otherwise; `/anthropic/v1/…` and `/openai/v1/…`
  prefixes give every inbound route an explicit choice. Anthropic-shape entries add the
  alias's optional `description`, which Claude Code's picker shows. Its discovery keeps
  only ids containing `claude` or `anthropic`, so an alias meant for the picker needs one
  in its name; any other alias is reached by naming it in Claude Code's model settings.

### Console

managed-agent-console gains a Models section — providers with profile presets and
write-only keys, deployments and aliases, gateway keys (the secret shown once), usage —
through its server-side proxy to `/admin/v1/`, the way it reaches the control plane
today (`MODEL_GATEWAY_BASE_URL`; the admin key only where no identity is configured).
The agent and dream editors' model field becomes a choice of aliases. That work is
planned in the console repository; this plan owns the admin API contract it consumes,
frozen by slice 2.

### Brain integration

- One route: `{"model": "*", "protocol": "anthropic", "base_url": <gateway>,
  "api_key": <gateway key>}`, no `upstream_model`, so the agent's model string reaches
  the gateway as the alias.
- `internal/provider` injects `traceparent` (`telemetry.Inject`) on both adapters'
  requests — it sends none today — and the brain sends `X-MAP-Session-ID` for affinity
  and per-session cost.
- compose and Helm run the gateway — Helm with two replicas and a PodDisruptionBudget by
  default, since every agent turn now depends on it — and seed the brain's gateway key
  from a Secret.

### Telemetry, errors, security

- A server span per request continues the caller's `traceparent`; a client span per
  upstream attempt. `gen_ai.request.model` is the alias — operator-defined, so the
  cardinality #88 worried about stays bounded here — and `gen_ai.response.model` the
  upstream id. `traceparent` goes upstream only where a provider opts in.
- Metrics: requests, latency, time to first token, tokens and cost, by alias,
  deployment and key.
- Errors answer in the inbound protocol's envelope (Anthropic
  `{"type":"error","error":{…}}`, OpenAI `{"error":{…}}`). A passthrough upstream error
  keeps its status and body after `provider.Redactor` has removed the credential the
  call used.
- Credentials are encrypted at rest, write-only, and decrypted per request into the
  outbound header alone. Provider hosts are admin-configured, so — like the brain's
  providers and web backends today — they dial with an ordinary client, not
  `internal/dialguard`: admin rights are the vouching.

## Slices

0. **Dependency, a separate plan (#67):** thinking persisted internally — never on the
   wire, where `agent.thinking` stays `{id, processed_at, type}` (checked against
   anthropic-sdk-go v1.70.1 — betasessionevent.go BetaManagedAgentsAgentThinkingEvent)
   — replayed within the tool-use turn, and requests built append-only. Gates slice 5.
1. **Store and catalogue:** schema and migrations, the admin API (providers,
   credentials, deployments, aliases, keys) under both auth modes, the snapshot with
   notify-driven reload, the four profiles' data.
2. **Anthropic inference:** `/v1/messages` streamed and not, `count_tokens`,
   `/v1/models`, the passthrough relay, profile edits, routing with retry, fallback and
   affinity, the stall guard, usage rows, limits, telemetry; compose and Helm; the live
   tier on MiniMax (CN) and DeepSeek through the official Anthropic SDK. Freezes the
   admin API for the console.
3. **Console** (the console repository's own plan): the Models section and the editors'
   alias choice.
4. **OpenAI surfaces:** Chat Completions and Embeddings passthrough, Models in OpenAI's
   shape, Anthropic ↔ Chat Completions conversion for OpenAI-only credentials (the
   conversion moving out of `internal/provider/openai`), `openai-go` into
   docs/REFERENCE_PROJECTS.md.
5. **Brain cutover:** the `traceparent` and session-id headers, the one-route default in
   compose and Helm with the seeded key; `flatten_search_results` stays accepted for the
   no-gateway mode. Acceptance: an `ant` session on MiniMax (CN) and one on DeepSeek
   through brain → gateway, transcripts in docs/HISTORY.md. Waits on slice 0.
6. **Responses, stateless:** `/v1/responses` streamed and not, converted to Anthropic;
   `previous_response_id`, `conversation` and the retrieve and delete routes answer with
   a refusal that says why.

## Later upstreams, with bifrost as the reference

Each follows in its own plan. What bifrost (at `3b31be003`) is the reference for, and
what v1 reserves so they arrive without reshaping it:

- **vLLM, then SGLang, Ollama and LMDeploy.** Each serves `/v1/messages`, so each is a
  profile on the passthrough path, not a new protocol — what bifrost's vLLM provider
  does when `use_anthropic_endpoints` is set (`core/providers/vllm/vllm.go:183`). The
  profile also records the engine's minimum version and the tool-call and reasoning
  parsers each model needs, since an engine started without them returns neither tool
  calls nor thinking.
- **Gemini.** A third upstream protocol (`generateContent` / `streamGenerateContent`)
  and an Anthropic ↔ Gemini direction in `convert`. `core/providers/gemini` is the
  reference: its converters, `count_tokens`, embeddings, and its tests for reasoning
  replay and thinking levels.
- **Vertex (#236).** One Google credential in front of three shapes
  (`core/providers/vertex/utils.go:417-440`): Gemini models in Gemini's shape; Claude
  under `publishers/anthropic` through `:rawPredict` / `:streamRawPredict`, in
  Anthropic's shape, so passthrough again; and partner MaaS models under
  `/endpoints/openapi/*` in OpenAI's shape — bifrost's test configuration reaches Kimi
  and MiniMax this way.
- **Test cases.** bifrost's `tests/e2e/api/HARNESS_COVERAGE_BACKLOG.md`, a per-provider
  feature inventory drawn from each vendor's docs, seeds each later upstream's
  fake-upstream fixtures and live-tier rows.
- **Not from bifrost: Zhipu and Moonshot.** bifrost has no provider for either; its only
  Kimi and MiniMax traces are Vertex- and Bedrock-hosted model ids in its tests. Their
  profiles rest on vendor docs until keys exist.
- **What v1 reserves:** the upstream protocol is an open set (`anthropic`, `openai`,
  later `gemini`), and a credential's `kind` holds only `api_key` until Vertex's
  service-account credential arrives.

Also later, each in its own plan: stored Responses state, and `cache_control` injection
where a vendor bills cache writes.

## Verification

- **Profiles and conversions:** table-driven golden tests per edit and per direction,
  decoded through anthropic-sdk-go's types (and openai-go's from slice 4) so a shape the
  SDK cannot read fails; each edit's test is first run against the code without it.
- **Upstream contract suite:** one fake server per profile reproducing its documented
  behavior — a refused `search_result`, `: keep-alive` comments, cache usage in the
  vendor's shape — all run through one shared suite, as `providertest` does for the
  brain's adapters.
- **Store:** `pgtest`; reload under concurrent writes; limits under concurrent requests.
- **Clients:** the official Anthropic and OpenAI Go SDKs drive the gateway in-process —
  streaming, tool loops with thinking, errors.
- **Live tier — MiniMax (CN) and DeepSeek, the keys the user has, driven through the
  official Anthropic SDK** (anthropic-sdk-go at the `go.mod` pin, pointed at a running
  gateway with `option.WithBaseURL` and a gateway key, as any SDK caller would be).
  `RUN_LIVE_MODELGATEWAY` names the vendors consented to (`deepseek,minimax`), so the
  fail-rather-than-skip contract holds per vendor: a named vendor with missing
  configuration fails, an unnamed one never runs. `.env` supplies `DEEPSEEK_API_KEY`, and
  `MINIMAX_API_KEY` beside `MINIMAX_BASE_URL=https://api.minimax.cn/anthropic`.
  - **Model list:** `Models.List`, `Models.ListAutoPaging` over more aliases than one
    page, and `Models.Get` return every configured alias, each with every `ModelInfo`
    field the SDK marks required present (`respjson.Field.Valid`).
  - **Model calls:** `Messages.New` and `Messages.NewStreaming` (assembled with
    `Message.Accumulate`) on an alias routed to each vendor: text, a tool-use round trip
    that sends the thinking blocks back unchanged, reported usage, and an upstream
    refusal surfacing as an `*anthropic.Error` carrying the upstream's status.
  - **Vendor behavior:** a `search_result` replay, `count_tokens`, cache usage fields;
    for MiniMax its `tool_choice` values and whether the CN key works on the
    international host. Results land in docs/HISTORY.md.
  - Zhipu and Moonshot join when keys exist. Until then their profiles are checked
    against fake upstreams only, and Zhipu's whole support matrix stays unconfirmed.
- **Acceptance:** Claude Code with `ANTHROPIC_BASE_URL` at the gateway, checked against
  the compatibility guide — streaming without stalls, `anthropic-beta` round trips,
  model discovery, recovery from a rejected capability. Claude Code sends any alias it
  does not recognize adaptive thinking, effort and context management; whatever a
  vendor rejects is recorded with the client setting that avoids it
  (`CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` covers context management). Then slice 5's
  `ant` sessions through the brain.
- Every slice: `make verify` (the coverage gate takes in the new packages), the
  verifier, both reviews and green CI, per CLAUDE.md.

## Docs and registry, as slices land

- CLAUDE.md, AGENTS.md, docs/ARCHITECTURE.md: a fifth server binary, its package
  reference, its place in the execution flow (slices 2 and 5).
- README.md: the live tier and the compose service.
- docs/REFERENCE_PROJECTS.md: bifrost as a design reference — ideas only, never a wire
  source (slice 2); `openai-go` (slice 4).
- docs/DIVERGENCES.md: `/v1/messages` echoing the alias as `model`; `count_tokens`
  answering 404 where an upstream has none; the `description` on `/v1/models` entries;
  stateless Responses; each profile edit with its vendor evidence.

## Open questions, settled by evidence in the slice that meets them

1. Which v1 vendors serve OpenAI-shaped embeddings — none of the four is confirmed yet.
   Slice 4, before the route is built against them.
