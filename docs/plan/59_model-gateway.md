---
status: in-progress
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
MiniMax, Zhipu (BigModel / Z.ai) and Moonshot (Kimi), and Gitee AI for embeddings and
rerank.

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
3. **v1 upstreams are the four vendors' official cloud APIs, plus Gitee AI** for
   embeddings and rerank — dikw-core's default vendor for both (decision 9). Gemini,
   Vertex (#236) and self-hosted engines (vLLM, SGLang, …) follow in later plans, with
   bifrost as the reference (Later upstreams says for what).
4. **Governance serves internal applications, on the platform's own API keys.** A caller
   authenticates with a key the console already issues (`sk-map-api01-`, #378), as one
   Anthropic key serves both the Messages API and Managed Agents; the gateway adds
   per-key usage and cost and per-key RPM/TPM limits. Usage holds metadata only — never
   prompt or response content — kept 90 days by default (configurable), with daily
   rollups kept indefinitely. No budgets, billing, teams or tenants. A platform key
   keeps the full control-plane authority it has today (`internal/api/identitylane.go`
   lets a machine key past every role check), including issuing keys
   (`internal/api/consoleapikeys.go`); the gateway grants none of it model access. A
   key reaches inference only through a key policy an administrator wrote — the
   bootstrap key and the brain's, known by their configured values, excepted — and the
   admin API refuses application keys, so an application can neither lift its limits
   nor mint a key that escapes them (Configuration and Auth below).
5. **Inbound surfaces:** Anthropic Messages in full — `POST /v1/messages` (streamed and
   not), `POST /v1/messages/count_tokens`, `GET /v1/models` and `GET /v1/models/{id}` —
   and, on the OpenAI side, Chat Completions, Embeddings, Models and Responses, the last
   **stateless**: no response is stored and `previous_response_id` is refused. Beside
   them, `POST /v1/rerank` in the Jina/Cohere shape, which has no OpenAI counterpart.
6. **All agent model traffic goes through the gateway.** The brain keeps one route,
   `*` → the gateway; the static routes file stays the configuration of a deployment
   that runs no gateway.
7. **Models are configured in managed-agent-console**, against the gateway's own admin
   API.
8. **Thinking persistence and replay is a separate plan, done first (#67).** Models
   the brain will reach think with no `thinking` field sent: every Claude
   5-generation model, DeepSeek's ("Thinking mode is enabled by default"), MiniMax's
   M2.x and M3.1-Flash-Preview (the latter answers `thinking: disabled` with a 400) —
   though not `MiniMax-M3`, off until a request asks. Anthropic, DeepSeek, Kimi and
   MiniMax document that thinking goes back within a tool-use turn (Ground truth;
   Zhipu's docs leave it open), but the brain stores no thinking content, and
   Anthropic's API checks a returned block against the `system`, `tools` and messages
   before it. That plan gates slice 5 (the brain cutover), not the gateway.
9. **Embeddings and rerank are shaped by dikw-core** (OpenDIKW's knowledge-base engine):
   the gateway serves its three calls unchanged, so dikw-core moves onto the gateway by
   configuration alone — `embedding_base_url`, `assets.multimodal.base_url` and
   `rerank_base_url` at the gateway, a platform API key behind its key variables.

Out of scope: Claude Code as a client (Anthropic does not support routing it to
non-Claude models), logging request or response content, semantic caching, an MCP
gateway, guardrails, budgets and billing, teams and tenants, stored Responses state,
audio/image/video generation, batch and files APIs on the gateway, injecting
`cache_control` breakpoints, and a `GET /v1/models` on the
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
([Anthropic API](https://platform.minimax.io/docs/api-reference/text-anthropic-api));
its per-model defaults — M2.x always thinks, M3.1-Flash-Preview refuses `disabled`
with a 400, `MiniMax-M3` thinks only when sent `{"type": "adaptive"}` — are on that page;
its [text generation guide](https://platform.minimax.cn/docs/guides/text-generation)
details M3.1-Flash-Preview's forced thinking and effort levels.
Probed live on 2026-10-04, in tool loops of up to four requests: `deepseek-flash` and
`deepseek-v4-pro` (both endpoints), and on MiniMax CN `MiniMax-M2.7`,
`MiniMax-M3.1-Flash-Preview` and `MiniMax-M3` with adaptive thinking, all return signed
thinking. DeepSeek's documented 400 holds where a request's tool ids are not its own:
on its Anthropic endpoint, both models refuse a request ending in tool results whose
`tool_use` turn carries no thinking and an id DeepSeek did not issue — `` The
`content[].thinking` in the thinking mode must be passed back to the API. `` — and
answer the same request with DeepSeek's own ids (`call_00_…`) 200, the reasoning
restored server-side; the first probes kept those ids, and so missed the rule. Neither
a made-up signature nor none is refused, and a loop followed by a later user message
is not checked. The brain sends event ids as tool ids, so without replay every DeepSeek
tool loop it drives is refused at its second request (#67). MiniMax answered a tool
continuation 200 without its thinking, with it, and with its text edited under its own
ids, and without its thinking under foreign ones (`MiniMax-M2.7` directly,
`MiniMax-M3.1-Flash-Preview` through the brain's event ids). Zhipu and Moonshot are untested; the gateway and the
brain follow the documentation regardless.

**Vendors.** Each publishes an Anthropic-compatible endpoint beside its
OpenAI-compatible one. The CN and international sites are separate consoles issuing
their own keys — the user's MiniMax account is on the CN site — so a profile carries
both regions and a provider names one; whether MiniMax's CN key also works on the
international host is the live tier's to show. OpenAI hosts are listed where documented.

| Vendor | Anthropic endpoint, CN · international | OpenAI endpoint | Field-support table |
|---|---|---|---|
| DeepSeek | `https://api.deepseek.com/anthropic` (one host) | `https://api.deepseek.com` | [published](https://api-docs.deepseek.com/guides/anthropic_api) |
| MiniMax | `https://api.minimax.cn/anthropic` · `https://api.minimax.io/anthropic` | `https://api.minimax.cn/v1` · `https://api.minimax.io/v1` | [CN](https://platform.minimax.cn/docs/api-reference/text-anthropic-api), [international](https://platform.minimax.io/docs/api-reference/text-anthropic-api); two pages disagree on `tool_choice` |
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

**Embeddings and rerank.** Of the four, only Zhipu's BigModel site serves either:
`/api/paas/v4/embeddings`, OpenAI-shaped (`embedding-3` takes `dimensions` of 256, 512,
1024 or 2048 and at most 64 inputs), and `/api/paas/v4/rerank`, returning
`results[{index, relevance_score, document}]`
([embeddings](https://docs.bigmodel.cn/api-reference/模型-api/文本嵌入),
[rerank](https://docs.bigmodel.cn/api-reference/模型-api/文本重排序)). Z.ai, DeepSeek and
Moonshot document neither; MiniMax keeps only a legacy embeddings page in a shape of its
own (`texts` and `type` in, `vectors` out, on `api.minimax.chat`), which no profile
adopts. Gitee AI serves both under `https://ai.gitee.com/v1`, per the OpenAPI spec it
publishes at `/v1/yaml`: `/embeddings` OpenAI-shaped, with `dimensions` and the
multimodal objects dikw-core sends (below), and `/rerank`, whose `top_n` the spec
defaults to 3 — though Gitee scores every document when it is left out (probed
2026-10-08). Neither vendor says whether it accepts `encoding_format: "base64"` —
Gitee's spec lists the field with a `float` default and no values, Zhipu's has no such
field — but dikw-core's working Gitee setup sends it on every request, so Gitee at least
accepts it; asked for it, Gitee answers floats (probed 2026-10-08).

**dikw-core** ([OpenDIKW/dikw-core](https://github.com/OpenDIKW/dikw-core) at `ef219da`,
v0.6.5), the embedding and rerank client of record:

- Text embeddings go through the `openai` Python SDK (2.33.0 in its lock):
  `embeddings.create(model, input=[…strings], dimensions=…)`, with `dimensions` on every
  request (`src/dikw_core/providers/openai_compat.py:267`). The SDK adds
  `encoding_format: "base64"` whenever the caller names none, and decodes a string
  vector but keeps a float array as it is (`openai/resources/embeddings.py:111-133`),
  so an upstream answering either way works. dikw-core reorders the result by
  `data[].index` (`:327`) and treats `usage.prompt_tokens` as optional.
- Multimodal embeddings post Gitee's own shape to the same `/embeddings` path:
  `input` is a list of `{"text": …}` or `{"image": "data:<mime>;base64,…"}` objects,
  and dikw-core expects no `usage` in the response
  (`src/dikw_core/providers/gitee_multimodal.py`), though Gitee reports `prompt_tokens`
  there too (probed 2026-10-08).
- Rerank posts `{model, query, documents, top_n}` to `/rerank` and reads
  `results[{index, relevance_score}]` (`src/dikw_core/providers/rerank.py`).
- It sizes its own batches. It observed Gitee refusing more than 25 inputs on both calls
  with a 400 in Gitee's own words (`docs/providers.md`, gotcha 2), where Gitee's spec
  allows 1000 embedding inputs and sets no rerank cap (above) — so the cap is the live
  tier's to measure, and nothing in the gateway depends on it. It measured 1000
  embedding inputs, multimodal ones included, and 25 rerank documents (2026-10-08).
- Gitee drops idle keep-alive connections in the middle of a batch, so dikw-core opens a
  fresh connection per request (`src/dikw_core/providers/_http.py`).
- An index's version is its dimension, normalization, distance and a revision its
  operator bumps "when a vendor silently refreshes weights behind a stable model name"
  (`src/dikw_core/config.py:108-117`), and a changed dimension means wiping the index
  and ingesting again (`docs/providers.md`, gotcha 1). Vectors that change under an
  unchanged name corrupt an index without an error.
- Its LLM leg is `anthropic_compat`: the Anthropic Python SDK's `messages.stream`, with
  `cache_control` on the system block, which the passthrough path serves as it is.

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
- Session affinity (`core/sessionaffinity.go`) binds a session id, per virtual key, user
  and requested model, to the route and the key that last served it, in a key-value
  store with a one-hour default TTL (`core/schemas/kvstore.go:16`): the first served
  request binds, a fallback that serves rebinds, a binding that fails is dropped. It
  exists so "a conversation keeps hitting the same provider prompt cache"
  (`docs/providers/request-options.mdx`), at two levels, provider and key; it does not
  track which model produced a thinking block.
- A replayed signature that an upstream refuses is healed after the fact
  (`core/encryptedreasoning.go`): a 400 whose message names a reasoning token — not
  Anthropic's "cannot be modified", and not one naming only a reasoning configuration
  parameter (`shouldStripReasoningAfterClientError`) — earns one retry with every signed
  thinking and `redacted_thinking` block removed from every turn
  (`stripRawAnthropicChatThinking`), and only if that removal changed the request
  (`core/bifrost.go:7043`). Its comments record the measurement behind removing whole
  blocks — against the live API on claude-sonnet-4-5, claude-haiku-4-5 and
  claude-opus-5 a history replays 200 untouched, 400 with its signatures blanked, and
  200 with the blocks removed, on every turn including the latest with a tool call in
  it — and it leaves untouched any assistant message that removal would empty, so a
  history whose only signed content sits in such a message earns no retry at all.
- Ideas taken here: weighted key selection, retry with jittered backoff before the first
  byte, ordered fallbacks, per-provider request paths, a model catalogue with prices,
  virtual keys with limits, session stickiness for cache locality, the fail-soft
  thinking strip as a backstop, and later its Gemini/Vertex converters. No code is
  copied; if a later converter ever is, NOTICE and THIRD_PARTY_LICENSES carry it.

**Anthropic's gateway docs.** Claude Code is out of scope, but its
[gateway compatibility guide](https://code.claude.com/docs/en/llm-gateway-protocol) is
the most precise public statement of what an Anthropic-format gateway owes a client, and
this gateway keeps its client-neutral rules: forward `anthropic-version` and
`anthropic-beta` "verbatim; don't allowlist individual values", and pass `anthropic-*`
headers and body fields through as open lists; stream without buffering, keep `ping`
events and deliver each event sequence whole; forward error bodies unmodified, since a
client's recovery can match on the upstream's wording; answer `retry-after` in integer
seconds. Token counting is optional there — "when they're absent, Claude Code falls back
to a character-based estimate".
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
relay per protocol), `convert`, `store` (queries over the `modelgateway` schema), `admin` — with
the HTTP surfaces at the root.

It imports `internal/provider` for `StallGuard`, `Redactor` and `SearchResultText`
only: the brain's `Provider` interface is too narrow for a gateway (a single system
string; no sampling parameters, `tool_choice`, signatures or streamed tool input). The
Anthropic ↔ Chat Completions conversion in `internal/provider/openai` moves to
`convert`, which both then use. It also imports `internal/secrets`,
`internal/identity`, `internal/telemetry`, and the platform's key check — `authenticate`
moves out of `internal/api` into a package both servers use, so a revoked or expired key
stops at the same instant on both — and never `internal/domain`, `internal/events` or
`internal/api`: beyond the `api_keys` table and an opaque session-id header, the gateway
knows nothing of the platform.

### Two request paths

Anthropic Messages is the **pivot**: every conversion goes through it, and nothing
converts when protocols match.

- **Passthrough** — the inbound and upstream protocols match. The body is decoded only
  as far as its top-level keys and the content blocks a profile or thinking provenance
  (Routing below) touches; provenance filtering runs first, then `model` becomes the
  deployment's upstream id and the profile's edits apply, and everything else, unknown
  fields included, goes out equal as JSON; `anthropic-*` headers go with it verbatim.
  `count_tokens` is filtered as its `/v1/messages` twin would be. The response relays
  event by event as it arrives — no buffering, `ping` events and keep-alive comments
  kept, each sequence whole — rewriting only `message.model` (back to the alias the
  caller sent), the usage fields the profile normalizes, and the thinking wrappers. On
  Chat Completions `model` is rewritten in the answer and in every chunk, and a stream is
  asked for its usage (`stream_options.include_usage`, the caller's other options kept),
  which the ledger and the TPM limit count and MiniMax reports only when asked (probed
  2026-10-08); the chunk carrying it — no choices, usage set — is withheld from a caller
  that did not ask. On both protocols `stream` must be a boolean or null, spelled
  exactly: the gateway relays and counts the answer by it, so a value or a key's case
  that an upstream might read as asking for a stream is refused (`count_tokens`, which
  never streams, excepted), as is, on Chat Completions, `stream_options` the gateway
  could not ask through: not an object or null, or a key differing from it or
  `include_usage` only in case. Such a stream has ended at `[DONE]`, at an upstream's
  error, relayed with its credentials removed, or when the upstream closes it after
  every choice's finish, as MiniMax-M3 does, sending no `[DONE]`; closed before any of
  these, it ends with an error chunk in OpenAI's envelope. A conversion path whose
  upstream sends no pings emits its own during silent gaps, which do not hold off the
  upstream's stall budget. A provider configures both
  of its vendor's endpoints
  and selection prefers the one matching the inbound protocol, so Anthropic and Chat
  Completions callers both pass through to all four chat vendors.
- **Conversion** — the protocols differ. v1 needs two directions: Responses (inbound) ↔
  Anthropic (upstream), since no v1 vendor serves Responses — reasoning items'
  `encrypted_content` and summary map to the thinking block's `signature` and text, so a
  stateless caller's tool loop keeps its thinking — and Anthropic (inbound) ↔ Chat
  Completions (upstream), for a credential usable on the OpenAI protocol only. Chat
  Completions inbound to an Anthropic-only upstream has no v1 case and waits for one.
  Slice 4 built the second (`convert`, whose doc gives each field's disposition): a
  credential is tried on the caller's protocol wherever it and its provider allow it, and
  converts only otherwise; a request the conversion cannot carry — structured output, a
  server tool, a document, a prefill — goes to the alias's passthrough attempts, answered 400 naming
  the field when none is left, and `count_tokens`, which Chat Completions lacks, is never
  converted, answering 404 `not_found_error` where only conversion attempts exist. The
  answer comes back whole or as the Messages event flow, `reasoning_content` as a thinking
  block (Routing below), a tool call as `tool_use`, the finish as the stop reason, and an
  upstream error in Anthropic's envelope; a whole answer it cannot carry is the gateway's
  502, and a stream's chunk an `error` event, the upstream read on for the usage it
  reports, neither retried once the upstream has answered. What is dropped, refused or
  lossy is in docs/DIVERGENCES.md.
- **Embeddings and rerank** have no Anthropic counterpart: passthrough to deployments
  of kind `embedding` (`/v1/embeddings`) and `rerank` (`/v1/rerank`). The body is read
  for `model` and `stream` alone — `input` may hold strings, token arrays or a vendor's
  multimodal objects, and `encoding_format` and `dimensions` go upstream as sent, while
  a stream asked for is refused, as is a `stream` the chat routes would refuse: neither
  API streams, and an upstream that did would answer in a shape whose usage the ledger
  cannot read — and the response is relayed as it arrives, unchanged but for its
  top-level `model` where it has one, its vectors never held or decoded, base64 or float,
  so it is bounded only by the stall guard and 512 MiB in all. The gateway splits no
  batch: an upstream's cap answers with the upstream's own 400, and a caller sizes its
  batches as it does today, within the 32 MiB the gateway takes in any request body.

### Vendor profiles

A profile is declarative data plus at most a few Go hooks, compiled in: `deepseek`,
`minimax`, `zhipu`, `moonshot`, `gitee`, and `anthropic-generic` / `openai-generic` for
any conformant endpoint (the later engine profiles join these). It names the request
path per protocol and per kind (every v1 profile's are the same: an OpenAI host is the
base of `/chat/completions`, `/embeddings` and `/rerank`), the CN and international
hosts, the auth header, content-block edits, field strips, the usage mapping, whether
connections are reused (not for `gitee`, which drops idle ones mid-batch), and whether
`count_tokens` exists — where it does not, that alias's `count_tokens` answers `404 not_found_error`
and a client falls back to estimating, as the compatibility guide describes. No v1
profile records its absence: DeepSeek and MiniMax answer it (probed 2026-10-07), and
Zhipu and Moonshot, which do not document it, answer for themselves.

The edit policy, which keeps a profile from quietly changing what a caller asked for:

- **Every edit is deterministic and leaves `system` alone,** so an upstream sees one
  stable prefix across a conversation's requests — what preserved thinking checks.
- **Pass through** by default; the upstream's own error reaches the caller, redacted.
  What the docs leave uncertain (every Zhipu field; MiniMax's two `tool_choice` pages,
  until slice 2g's live tier settled them) passes through until evidence says
  otherwise — the live tier, for a vendor it has a key for.
- **Edit** only what (a) the platform's own traffic needs — `search_result` flattened to
  text through `provider.SearchResultText` where a vendor refuses it, which is the
  brain's `flatten_search_results` moved behind the gateway — or (b) the vendor
  documents as ignored, where dropping it changes nothing.
- **Refuse** at the gateway only where a vendor documents, or the live tier shows, that
  it silently ignores a field whose absence changes the result (DeepSeek ignores
  `disable_parallel_tool_use`), decided per field in the slice, citing the evidence in
  the profile. Nothing is
  downgraded silently, as plan 53 decided for effort. Slice 2 reads "changes the
  result" as a bound on the answer a caller's code may rely on — DeepSeek's
  `disable_parallel_tool_use`, MiniMax's `stop_sequences`, and thinking disabled on
  MiniMax's M2.x models, which think regardless; and, on the live tier's evidence,
  `tool_choice` `any` on DeepSeek and `any` and `tool` on MiniMax, which answer without
  the tool call they force, and MiniMax's `disable_parallel_tool_use`. Slice 4 holds
  Chat Completions to the same rule on its own probes: `parallel_tool_calls: false` on
  both vendors, and MiniMax's `stop` and a `tool_choice` forcing a call (`required`, naming
  a function, or `allowed_tools` in `required` mode; DeepSeek honors the first two, refusing a forced call with thinking on itself),
  and thinking disabled on MiniMax's M2.x models as on Messages; a request the gateway
  converts is judged as the Chat Completions request it becomes — and passes through what
  a vendor ignores among sampling knobs, context shaping and server-side features;
  a request such a field bars from a deployment goes to the alias's others, and is
  refused only when none is left — but never a count, which the field leaves
  unchanged.

### Configuration model

Two layers of credentials, never mixed: a caller presents a platform API key and names a
model — an alias — and the gateway resolves the alias to a deployment and calls that
deployment's provider with the provider's own credential, which no caller ever sees.

A Postgres schema, `modelgateway`, in the platform's database (`DATABASE_URL`, whose
`api_keys` the gateway reads), created by ordinary numbered migrations under
`internal/store/migrations/`: every binary runs the platform's migrations when it opens
the database, the gateway with them, so there is one ledger and CLAUDE.md's migration
rules hold unchanged. Ids carry gateway-local prefixes (`gwprov_`, `gwcred_`, `gwdep_`),
deliberately outside `internal/domain`'s wire list. The top-level tables (provider,
deployment, alias) reserve `org_id`, `workspace_id` and `project_id` with single-tenant
defaults, as the platform's top-level resource tables do
(`internal/store/migrations/0001_init.sql`, design principle 5).

- **provider** — one vendor account behind fixed endpoints: profile and an endpoint per
  protocol (a profile host or a custom one), all fixed at creation; name, extra
  headers, stall timeout, whether it is sent W3C trace context (`propagate_trace`, off
  by default), enabled. Its credentials are keys of that one account — a key of another
  account is another provider, which the console says where a key is added, since no
  vendor exposes which account a key belongs to. No header or endpoint carries a
  secret: a
  header name the redactor treats as a credential (`internal/provider/redact.go`), an
  endpoint with userinfo, and one with a query string are refused; a secret is a
  credential.
- **credential** — a provider's key, as ciphertext and key id from `internal/secrets`
  (the vault credentials' backend selection); `kind` (`api_key`, the only v1 value);
  last four characters for display; the protocols it may be used on; weight; enabled.
  Write-only through the API.
- **deployment** — a provider's upstream model id; kind (`chat` | `embedding` |
  `rerank`); capabilities (tools, thinking, vision, `max_input_tokens`, `max_tokens` —
  the `ModelInfo` fields `/v1/models` answers from); prices per million tokens for
  input, output, cache write and cache read, entered by the operator — no remote price
  sync, so an air-gapped install works — each 0 or between 1e-12 and 1e15, so every
  cost the ledger computes from one reads back as a double. A deployment's provider and upstream model id
  never change, so a deployment id names one model on one account behind its provider's
  fixed endpoints —
  what thinking provenance (Routing below) and an embedding index both key on; moving
  to another endpoint, account or model is a new provider or deployment. Credentials
  rotate freely within their account.
- **alias** — the model name a caller sends: a display name and targets
  `[{deployment, priority, weight}]`. Exact match first, then an optional `*` alias. A
  `claude-*` alias is only a name, which is how clients that hard-code Claude names
  reach a vendor model. An `embedding` alias has exactly one deployment, fixed when the
  alias is created: an index built through it would mix two vector spaces without an
  error (Ground truth, dikw-core), so a new embedding model is a new alias. Its
  credentials still balance and rotate.
- **key_policy** — per platform API key (`api_keys.id`), the grant that lets it call
  models: an optional alias allow-list, optional RPM and TPM limits. A key with no row
  cannot call models, so a key an application issues itself reaches nothing until an
  administrator grants it. Two keys need no row, being known by their configured value
  rather than by any row's name (issued names are not unique,
  `internal/store/migrations/0024_api_keys_lifecycle.sql`): the bootstrap key and the
  brain's (Auth and Brain integration below); a row written for either still applies. Issuance, status and
  expiry stay the platform's.
- **usage** — one row per request a key's limits admitted: request id, key id, the
  model name sent and the alias it matched, the deployment and credential that answered
  (or were tried last), session id, inbound protocol and route, the status and error
  type the caller was given, the token counts the upstream reported (input, output,
  cache write, cache read), cost at the prices in force, latency, time to first token.
  No count is estimated: a response without `usage`, or with counts under keys an
  OpenAI usage does not use, as Gitee's rerank reports its zeros, records no tokens and
  no cost, and a `count_tokens` answer is a count, not usage. Metadata only. Kept 90 days by default (`MODELGATEWAY_USAGE_RETENTION`) and
  deleted by a sweep any replica may run under an advisory lock.
- **usage_daily** — rollups per UTC day, key, matched alias (Telemetry below) and
  deployment, written by the statement that writes each usage row, so no job builds
  them, and kept indefinitely; the console's cost reports read these.
- **rate_windows** — one-minute windows per limited key: requests admitted and tokens
  completed.

Every admin write commits a `NOTIFY modelgateway_config`; each replica reloads its
snapshot on the notification and on a periodic tick, so a missed notification heals. The
request path reads its configuration only from the snapshot; what it counts and records —
a limited key's admission and the ledger row, costed at the deployment's prices as the
database holds them — goes to the database directly.

### Routing, retries, limits

- Resolve the alias; in the highest-priority group with a healthy target, choose a
  deployment by weight, then a credential by weight. A request carrying
  `X-MAP-Session-ID` chooses its credential, and its deployment when there is no
  thinking provenance to follow (below), by weighted rendezvous hashing on the session
  id instead, so a conversation's requests keep reaching one upstream prompt cache —
  bifrost's two levels of session stickiness, without its state. It is an optimization
  only: nothing correct depends on it.
- **Thinking provenance.** Every thinking block's `signature` and every
  `redacted_thinking` block's `data` the gateway returns is wrapped as
  `mapgw1.<deployment id>.<the upstream's value>`, exactly once per block: a whole
  response's field is prefixed, and in a stream the first non-empty fragment of each
  block index — on its start or its first `signature_delta` — is prefixed and the rest
  pass unchanged, so for blocks streamed one after another, as Anthropic streams
  them, a client that concatenates the fragments, as the SDK does, assembles the
  same value either way. A block whose value is empty — an unsigned
  thinking block — has nothing to wrap: it names no producer and goes back to no
  upstream, and no thinking block after it in the same response is wrapped either,
  since its signature covers the block that will not go back with it — as the brain
  ends its kept run at the first unsigned block (plan 60). In a stream, where a
  signature arrives after its block starts, a thinking block that starts while the
  one before it is still unsigned ends the wrapping the same way, as do an empty
  `redacted_thinking` block and an empty start that names no index. Both fields are
  opaque to a client, which
  stores and returns them verbatim, so the wrapper rides along unseen. On a request the
  gateway reads the wrappers in the history: among the alias's healthy targets, the
  deployment that produced the newest block is chosen first, and whichever deployment
  answers receives exactly its own blocks, unwrapped — every other thinking block
  (another deployment's, an unwrapped one, one naming a deployment that no longer
  exists) is removed, and an assistant message left empty by the removal goes with
  it. That message held only thinking, so no tool call loses its result; the two user
  turns it separated are joined into one, as the Messages API itself combines
  consecutive same-role turns.
  What this guarantees is that the gateway never makes a valid history invalid: for a
  caller that returns its history append-only and unedited, with the wrappers it was
  given, each deployment receives every one of its blocks under the prefix it produced
  it under — each was produced under a request carrying only that deployment's blocks,
  after the same deterministic removals — so a fallback costs the earlier model's
  reasoning, never a 400. A caller that edits its history, or forges a wrapper, gets the
  upstream's own answer to what it sent; the wrapper is a routing hint, not a
  credential, and a forged one can only name a deployment the key may already reach.
  The scheme needs no state, no session id and no sweep, holds per conversation (each
  of a session's threads carries its own history) and across replicas and restarts, and
  leaves no session stranded when a deployment is retired. A per-session record of the
  last answering deployment — bifrost's session affinity (Ground truth) — was
  considered and rejected as provenance: a session's threads share one id, a TTL forgets
  provenance a history still holds, and replicas race to write it. So was rendezvous
  hashing, which after a fallback would send a recovered first choice the fallback
  model's thinking. **Backstop, best effort:** a 400 whose message names a thinking
  block or its signature — other than one naming only a reasoning configuration
  parameter, and DeepSeek's "must be passed back" or Anthropic's "must start with a
  thinking block", which ask for thinking the request lacks and a removal cannot
  supply — puts the inbound request in strip mode: every
  thinking block is removed, an emptied assistant message going as above, and the
  attempt is made again, as a retry like any other — at the same deployment within
  its attempt budget, at the alias's next once that is spent — and after the
  backoff, so never for a caller that has left. Removing all thinking is valid
  (Ground truth, Thinking). That
  is bifrost's fail-soft strip, with four differences: Anthropic's "cannot be
  modified" enters strip mode here, where bifrost excludes it as a refusal the removal
  repeats — the Claude 5-generation rule is that removing every block stays valid, and
  when the removal does not cure it the cost is the one attempt strip mode allows;
  DeepSeek's "must be passed back" and Anthropic's "must start with a thinking block"
  do not, where bifrost's matcher, which only excludes the other two, would strip and
  retry;
  strip mode is entered once per inbound request and holds for every attempt after it,
  the fallbacks included, so no attempt can re-send what was stripped or strip twice;
  and whichever attempt answers in strip mode wraps the first thinking block of its
  response, when signed, `mapgw1r.` rather than `mapgw1.` — a reset mark. Every later request
  removes each thinking block that precedes the newest reset-marked block before the
  provenance rule applies, so the response that block opened keeps all its blocks and
  nothing older returns ahead of it: it was produced with no thinking before it, and
  sending older blocks back in front of it would earn the same 400 on every turn
  after. The recovery holds only while the caller returns that block; a strip-mode
  answer with no thinking, or whose first thinking block is unsigned, has nothing to
  carry the mark, and then each later request
  on that history pays the refusal and one retry again — bounded, never a loop within
  a request. An OpenAI-shaped caller's
  `reasoning_content` carries no signature to wrap and has no provenance; it goes
  upstream as sent. Reasoning a converted answer returns does have one: its thinking
  block's signature is the wrapper around an empty value, and goes back only to the
  deployment that produced it and only on a conversion attempt, as `reasoning_content` —
  never to that deployment's Anthropic endpoint, which cannot verify it, as a signed
  block never goes to an OpenAI one.
- **Retry and fallback happen before the first byte only.** A connect error, 408, 409,
  429, 5xx or overload — or whatever the upstream's own `x-should-retry` says, as
  Anthropic's SDKs read it — moves to the
  next credential, then the next group, within a bounded attempt count per deployment
  and jittered exponential backoff; so does a vendor refusing the credential itself
  (401, 402, 403), answered 502 when nothing serves. A stall does not: the quiet
  upstream may still be generating, and charging for, the answer. A stream's answer
  begins at its first event that is not a keep-alive, or once 64 KiB of keep-alives have
  passed, so one that opens with an error event is retried like an error response. After the first byte a failure is the
  caller's (`event: error` on a stream). A fallback to another deployment carries only
  that deployment's thinking, by the rule above — so a fallback taken in the middle of
  a tool loop sends the loop without its thinking, which DeepSeek refuses under
  foreign ids (Ground truth, Thinking), and that attempt fails like any other. An embedding alias, with its one
  deployment, retries across credentials only.
- **Stall.** `provider.StallGuard` per provider, for #121's reasons unchanged.
- **Limits.** Admission increments the key's request count for the current minute in one
  upsert and refuses over the limit with 429 and `retry-after` in integer seconds; a
  refused request is not counted, nor is one the gateway refuses on its own before
  admission (an unknown model, a field every upstream ignores), and a `count_tokens`
  request counts like any other. TPM is
  a soft limit on completed usage: a response's tokens count in the minute it ends —
  written apart from its ledger row, so a row the ledger refuses still counts — and a
  request is admitted while the current minute's count is under the limit, so requests
  already in flight can overshoot it by their own size — RPM is what bounds that. It
  counts input, cache-write and output tokens but not cache reads, as Anthropic's own
  limits count them (platform.claude.com/docs/en/api/rate-limits, "Cache-aware ITPM"). A
  caller that disconnects does not end the upstream response: the gateway reads it to
  the end under the stall guard and records its usage, so abandoned streams cannot
  spend tokens TPM and the ledger never see, and no count has to be estimated.
  Per-replica in-memory buckets were rejected: under an autoscaler the effective limit
  would be a function of the replica count.

### Auth, admin API and `/v1/models`

- **Inference** takes a platform API key in `x-api-key` or `Authorization: Bearer`
  (Anthropic SDKs send the first, OpenAI SDKs the second), checked by the control plane's
  own rule: one indexed lookup per request, `status = 'active'` and unexpired against
  the database clock (`internal/apikey` `Authenticate`). Both headers, and every
  `X-MAP-*` header, are stripped before the upstream call.
- **The admin API** lives under `/admin/v1/`: providers, credentials, deployments,
  aliases, key policies, usage queries, and the profiles (read-only). No reference
  surface corresponds to it, so it is ours to shape. It takes the operator's own token,
  with identity configured from the same `IDENTITY_*` settings the control plane reads
  (`viewer` and `developer` read, `admin` writes), or the bootstrap key — the gateway
  reads `CONTROLPLANE_API_KEY` from the Secret the control plane reads it from and
  compares in constant time, so a key an application issues under the name `bootstrap`
  is nothing to it. A deployment without identity configures its console with that
  key. Every other platform key — the keys applications hold, the brain's — is refused
  there: a key that could edit key policies could lift its own limits.
- **`/v1/models`** is one path with two shapes. The root answers in Anthropic's shape
  when the request carries `anthropic-version` — which every Anthropic SDK sends and no
  OpenAI SDK does — and in OpenAI's otherwise; `/anthropic/v1/…` and `/openai/v1/…`
  prefixes give every inbound route an explicit choice. Anthropic's shape lists `chat`
  aliases only, since an Anthropic client can call nothing else; OpenAI's lists every
  kind. A key with an alias allow-list sees only those aliases.

### Console

managed-agent-console gains a Models section — providers with profile presets and
write-only vendor keys, deployments and aliases, usage and cost — and, on its existing
API keys page, a model grant per key: granting writes the key's policy (alias
allow-list, RPM and TPM limits), revoking deletes it, and the page shows which active
keys may call models, since an active key without a grant may not. It reaches
`/admin/v1/` through its server-side proxy (`MODEL_GATEWAY_BASE_URL`), attaching the
credential it already sends the control plane.
The agent and dream editors' model field becomes a choice of aliases. That work is
planned in the console repository; this plan owns the admin API contract it consumes,
frozen by slice 2.

### Brain integration

- One route: `{"model": "*", "protocol": "anthropic", "base_url": <gateway>,
  "api_key": <platform API key>}`, no `upstream_model`, so the agent's model string
  reaches the gateway as the alias.
- `internal/provider` injects `traceparent` (`telemetry.Inject`) on both adapters'
  requests — it sends none today — and the brain sends `X-MAP-Session-ID` for
  per-session cost and cache locality. Both headers are each call's own, so a route may
  not configure them (`provider.NewRegistry` refuses it).
- compose and Helm run the gateway — Helm with two replicas and a PodDisruptionBudget by
  default, since every agent turn now depends on it. Until the cutover nothing does, so
  slice 2 ships the chart's gateway behind `modelgateway.enabled`, off: the chart still
  installs without a credential cipher, which the gateway refuses to start without, and
  turning it on by default makes a cipher part of every default install. The brain's platform API key comes
  from one Secret that the brain, the control plane and the gateway read: the control plane
  registers it in `api_keys` under the name `brain` exactly as it registers
  `CONTROLPLANE_API_KEY` as `bootstrap` (`api.EnsureAPIKey`, `cmd/controlplane/main.go`),
  the gateway knows it by that value (Configuration, `key_policy`), and the brain sends
  it. It is `BRAIN_API_KEY`; a control plane booting without it archives the
  env-var-managed `brain` key, so a removed variable leaves no live credential behind.

### Telemetry, errors, security

- A server span per request continues the caller's `traceparent`; a client span per
  upstream attempt. `gen_ai.request.model` is the name the caller sent and
  `gen_ai.response.model` the upstream id — span attributes, where cardinality costs
  nothing. `traceparent` goes upstream only where a provider opts in
  (`propagate_trace`): the attempt's span context, or, with no tracer installed and so
  no span recorded, the caller's as it came. A provider's headers cannot send W3C trace
  context: the admin API refuses them.
- Metrics: requests, latency, time to first token, tokens and cost, by matched alias,
  deployment and key — `modelgateway.requests`, `.request.duration`,
  `.time_to_first_token`, `.tokens` by kind and `.cost` (the ledger row's), recorded from
  the usage the ledger row is written from. An error type no Anthropic status maps is
  `_OTHER` there; the span and the ledger keep the type itself.
  The matched alias is the configured one, `*` for a wildcard match, so a caller
  choosing names cannot grow a metric (#88's concern).
- Errors answer in the inbound protocol's envelope (Anthropic
  `{"type":"error","error":{…}}`, OpenAI `{"error":{…}}`). A passthrough upstream error
  keeps its status and body after `provider.Redactor` has removed the credential the
  call used.
- Credentials are encrypted at rest, write-only, and decrypted per request into the
  outbound header alone. Provider hosts are admin-configured, so — like the brain's
  providers and web backends today — they dial with an ordinary client, not
  `internal/dialguard`: admin rights are the vouching. That client follows no redirect,
  as the web backends' do (`internal/webtool/tavily/tavily.go`): a 3xx fails the
  attempt, so a credential header reaches only the configured host. An endpoint may be
  `http`, as the vault's credential endpoints may (`internal/api/vaultcredauth.go`
  validateEndpointURL): an in-cluster model server often serves no TLS, and refusing
  it would refuse the deployment this platform is for. The console marks such a
  provider, whose key crosses the network in clear.

## Slices

0. **Dependency, a separate plan (#67):** thinking persisted internally — never on the
   wire, where `agent.thinking` stays `{id, processed_at, type}` (checked against
   anthropic-sdk-go v1.70.1 — betasessionevent.go BetaManagedAgentsAgentThinkingEvent)
   — and sent back only to the model that produced it, while everything before it in the
   request is what it was produced under. Gates slice 5.
1. **Store and catalogue:** schema and migrations, the platform key check moved to a
   shared package, the admin API (providers, credentials, deployments, aliases, key
   policies) under both auth modes, the snapshot with notify-driven reload, the four
   chat vendors' profile data.
2. **Anthropic inference:** `/v1/messages` streamed and not, `count_tokens`,
   `/v1/models`, the passthrough relay, profile edits, routing with retry, fallback and
   thinking provenance, the stall guard, usage rows with their retention sweep and
   daily rollups, limits, telemetry; compose and Helm; the live
   tier on MiniMax (CN) and DeepSeek through the official Anthropic SDK. Freezes the
   admin API for the console.
3. **Console** (the console repository's own plan): the Models section, model grants on
   the API keys page, and the editors' alias choice.
4. **OpenAI surfaces:** Chat Completions passthrough, Embeddings (text and multimodal)
   and Rerank passthrough with the `gitee` profile, Models in OpenAI's shape,
   Anthropic ↔ Chat Completions conversion for OpenAI-only credentials (the conversion
   moving out of `internal/provider/openai`), `openai-go` into
   docs/REFERENCE_PROJECTS.md; the live tier on Gitee AI. Acceptance: dikw-core through
   the gateway.
5. **Brain cutover:** the `traceparent` and session-id headers, the one-route default in
   compose and Helm with the seeded key, and the chart's `modelgateway.enabled` on by
   default; `flatten_search_results` stays accepted for the
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
  vendor's shape, a batch-cap 400 in the vendor's own words, a connection closed
  between requests, a redirect the attempt fails on without the credential reaching
  its target — all run through one shared suite, as `providertest` does for the
  brain's adapters. Thinking provenance runs against two fakes behind one alias that
  enforce what Anthropic's API does: each signs a block over the request ahead of it
  and refuses a block whose prefix or signer differs. A tool loop whose first
  deployment fails mid-loop reaches the second with none of the first's blocks and
  stays there after the first recovers (the second produced the newest block); when the
  second fails in turn, the first gets back only its own blocks, each under its own
  prefix; a thinking-only reply stripped on fallback leaves no empty assistant message;
  a stream whose signature arrives in several fragments, after an empty start,
  assembles the same wrapped value as the whole response; an unwrapped block, or one
  whose wrapper names a deployment outside the alias or no longer configured, never
  reaches an upstream; an edited block the fake refuses earns one retry without
  thinking, which answers with its first thinking block reset-marked — a response of
  two thinking blocks and two parallel tool calls keeps all four on the next request —
  after which the conversation continues with no further refusal; a strip-mode attempt
  that fails before its first byte falls back still in strip mode, and a second
  refusal earns no second retry; a strip-mode answer without thinking pays the retry
  again on the next request and no more; a refusal naming no thinking earns no retry,
  and neither does DeepSeek's "must be passed back" nor Anthropic's "must start with
  a thinking block", while Anthropic's "cannot be modified" does;
  and requests of one session id with no thinking keep one deployment and credential
  while the eligible candidates, weights and priorities are unchanged, move when health
  changes them, and yield to a newer block's producer.
- **Store:** `pgtest`; reload under concurrent writes; limits under concurrent requests,
  and a stream whose caller disconnects still read to its end, its usage recorded and
  counted against TPM;
  the retention sweep against rows either side of the cutoff, its rollups intact; a key
  archived or expired mid-run refused on its next request, by both servers alike; a key
  without a grant refused inference, including one an application issued itself under
  the name `bootstrap`, and refused the admin API.
- **Clients:** the official Anthropic and OpenAI Go SDKs drive the gateway in-process —
  streaming, tool loops with thinking, errors. dikw-core's request bodies are replayed
  as fixtures, not left to a Go SDK's defaults: `encoding_format: "base64"` (its
  SDK's default) and `dimensions` on every embeddings request, Gitee's multimodal
  objects, a rerank batch — the embeddings answered once with float vectors and once
  with base64.
- **Live tier — MiniMax (CN), DeepSeek and Gitee AI, the keys the user has.** Chat goes
  through the official Anthropic SDK (anthropic-sdk-go at the `go.mod` pin, pointed at
  a running gateway with `option.WithBaseURL` and a platform API key, as any SDK caller
  would be). `RUN_LIVE_MODELGATEWAY` names the vendors consented to
  (`deepseek,minimax,gitee`), so the fail-rather-than-skip contract holds per vendor: a
  named vendor with missing configuration fails, an unnamed one never runs. `.env`
  supplies `DEEPSEEK_API_KEY`, `MINIMAX_API_KEY` beside
  `MINIMAX_BASE_URL=https://api.minimax.cn/anthropic`, and `GITEE_API_KEY`.
  - **Model list:** `Models.List` and `Models.ListAutoPaging` over more aliases than one
    page return every `chat` alias the key may use and no other, and `Models.Get`
    answers each of them and refuses an alias the key may not use or that is not
    `chat` — every returned entry with every `ModelInfo` field the SDK marks required
    present (`respjson.Field.Valid`).
  - **Provenance through the real vendors:** each model's provider points at a proxy
    that forwards to the vendor and records both directions, since neither vendor
    checks a signature and its 200 proves nothing. Every model expected to think
    returns signed thinking, within three asks, since adaptive thinking is the model's
    choice; each value the gateway wraps, in block order up to the
    first unsigned one, comes back around the vendor's own value and goes back, on
    the continuation the SDK's `ToParam` makes, as exactly that value — a model that
    returns no thinking, as MiniMax-M3.1-Flash-Preview did on 2026-10-07, has its
    round trip checked without it; an alias whose first choice is down
    falls back, and its continuation goes straight to its thinking's producer; a
    conversation crossing from DeepSeek to MiniMax and back sends each only its own,
    MiniMax closing any tool loop it opens first, since DeepSeek refuses a loop that
    reaches it under MiniMax's ids without DeepSeek's thinking — a refusal the tier
    checks is relayed as DeepSeek's own 400.
  - **Model calls:** `Messages.New` and `Messages.NewStreaming` (assembled with
    `Message.Accumulate`) on an alias routed to each vendor: text, a tool-use round trip
    that sends the thinking blocks back unchanged, reported usage, and an upstream
    refusal surfacing as an `*anthropic.Error` carrying the upstream's status.
  - **Vendor behavior:** a `search_result` replay, `count_tokens`, cache usage fields,
    each vendor's `tool_choice` values; for MiniMax, whether the CN key works on the
    international host. Results land in docs/HISTORY.md.
  - **Chat Completions** (slice 4), through openai-go to each vendor's OpenAI endpoint:
    a text answer, whole and streamed with and without the usage chunk asked for,
    equal to what the vendor sent and counted in the ledger; a tool round trip made
    with `ToParam`; and each field the gateway refuses for the vendor, asked of the
    vendor directly too, which must still ignore it.
  - **Embeddings and rerank on Gitee** (slice 4), through openai-go for embeddings and
    a plain HTTP client for rerank, which no SDK covers: text embeddings with
    `dimensions`, asked as float and as base64, decoding to vectors of that length that
    agree (and recording which encoding Gitee answers a base64 request in); a
    multimodal text-and-image request; rerank scores mapped back by `index`; each
    call's batch cap measured, the 400 past it relayed in Gitee's own words.
  - Zhipu and Moonshot join when keys exist. Until then their profiles are checked
    against fake upstreams only, and Zhipu's whole support matrix stays unconfirmed.
- **Acceptance:** slice 4's dikw-core run — its embedding, multimodal and rerank base
  URLs and its Anthropic leg's `llm_base_url` all at the gateway, a platform API key
  behind each key variable — passing `dikw client serve-and-run --base <base> -- check`,
  then an ingest whose sources include an image embedded through `assets.multimodal`,
  and a retrieve with rerank; and slice 5's `ant` sessions through the brain.
  Transcripts in docs/HISTORY.md.
- Every slice: `make verify` (the coverage gate takes in the new packages), the
  verifier, both reviews and green CI, per CLAUDE.md.

## Docs and registry, as slices land

- CLAUDE.md, AGENTS.md, docs/ARCHITECTURE.md: a fifth server binary, its package
  reference, its place in the execution flow (slices 2 and 5).
- README.md: the live tier and the compose service.
- docs/REFERENCE_PROJECTS.md: bifrost as a design reference — ideas only, never a wire
  source (slice 2); `openai-go` (slice 4).
- docs/DIVERGENCES.md: `/v1/messages` echoing the alias as `model`; `count_tokens`
  answering 404 where an upstream has none; stateless Responses; thinking signatures
  and redacted data returned wrapped in two forms (`mapgw1.`, and `mapgw1r.` on the
  first thinking block of a strip-mode answer, when signed), history thinking filtered by
  provenance, all of it removed for the attempts after a signature refusal, and every
  block ahead of the newest reset mark removed on later requests;
  each profile edit with its vendor evidence.

## Open questions, settled by evidence in the slice that meets them

1. Whether Zhipu accepts `encoding_format: "base64"`, the OpenAI Python SDK's default.
   If it refuses it, either the edit policy gains a lossless case — ask for floats,
   encode the answer as base64 — or the gateway refuses that value on Zhipu aliases with
   an error that says why. Settled when a Zhipu key exists; until then Zhipu's
   embeddings run against a fake upstream only.
