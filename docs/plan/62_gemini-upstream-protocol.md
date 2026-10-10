---
status: archived
issue: "#900"
---

# A Gemini upstream protocol for the model gateway (plan 62)

The model gateway reaches two upstream protocols, Anthropic Messages (passthrough) and
OpenAI Chat Completions (the conversion of plan 59 slice 4c). Plan 59 left Gemini to a
plan of its own ("Later upstreams"): a third protocol, `generateContent` and
`streamGenerateContent`, and an Anthropic Messages ⇄ Gemini direction in
`internal/modelgateway/convert`. After this plan a Messages request, or a Responses
request served as one, can be answered by a Gemini model through the Gemini API with
an API key, streamed and not, tool loops included.

Scope settled with the user on 2026-10-10: the Gemini API (AI Studio) with an API
key — not Vertex, which stays #236's — and the code lands before a key that can
generate exists. The maintainer's key answers `countTokens` and the model list but
`402` on every billable call (its project's prepaid balance is empty), so generation
is verified against answers recorded on 2026-10-10 rather than live, and the request
side is checked live through `countTokens`, which validates a whole
`generateContentRequest` for free (Verification).

## Ground truth (measured 2026-10-10)

Recorded with `gemini-3.8-flash`. Generation was recorded on Vertex's
`publishers/google` route, which serves the same `GenerateContentResponse`; the Gemini
API answered the free calls and the errors.

- **Thought signatures ride on parts, not on thoughts.** With no thinking setting the
  answer's last text part carries `thoughtSignature`; in a parallel tool call only the
  **first** `functionCall` part does. A streamed answer puts a text signature on a final
  empty text part, in the last chunk, beside `finishReason` and the only complete
  `usageMetadata`; a streamed function call carries its own, and arrives whole, one part
  per chunk.
- **A tool turn replayed without its signature is refused**: `400` "Function call is
  missing a thought_signature in functionCall parts". With the signature, `200`; with
  the literal `skip_thought_signature_validator` in its place, also `200`, the value
  bifrost sends for history Gemini did not produce. Whether a text part's missing
  signature is refused was not measured, and Google's thinking guide, now written for
  its Interactions API, no longer says.
- **`functionCall` carries an `id`** (`call_38500`), and a tool-calling answer finishes
  `STOP`: there is no tool finish reason.
- **Thinking**: `includeThoughts: true` adds `{"text": <summary>, "thought": true}`
  parts, though a streamed tool call returned none while billing 59 thought tokens.
  `thoughtsTokenCount` is reported apart from `candidatesTokenCount`. `thinkingLevel`
  `low` and `high` are accepted and `minimal` refused on this model; `thinkingBudget: 0`
  still thought (93 tokens); a level and a budget together are refused.
- **`countTokens` takes a whole request** on the Gemini API, wrapped as
  `{"generateContentRequest": {…}}`, and its parser refuses an unknown field at any
  depth (`Invalid JSON payload received. Unknown name "bogus" at
  'generate_content_request.contents[0].parts[0]'`), so it checks a converted request's
  shape at no cost. It accepted a tool loop carrying `parametersJsonSchema` with
  `$defs`, a type union and `additionalProperties`, a `functionCall` and
  `functionResponse` under an id Gemini never issued, a real signature and the sentinel,
  an image in a user turn and in a function response's `parts`, `topK`,
  `stopSequences` and each thinking setting; the older `parameters` field refused that
  schema's `additionalProperties` and `$defs`.
- **The brain keeps only a response's leading run of signed thinking** — a block after
  text or a tool call is not stored (`internal/brain/stream.go`, plan 60 decision 3).
- **Errors** are `{"error": {"code", "message", "status"}}`: `404 NOT_FOUND` for an
  unknown model, `400 INVALID_ARGUMENT`, and `402 RESOURCE_EXHAUSTED` for an empty
  prepaid balance, which an attempt already reads as the vendor refusing the gateway's
  credential (`refusedCredential`), as it reads DeepSeek's `402`: the next attempt is
  tried, and the caller gets `502 api_error` when none is left.
- **The typed schema** is `google.golang.org/genai` (v1.73.0, read in the module cache,
  never a dependency): `Part.ThoughtSignature`, `ThinkingConfig{IncludeThoughts,
  ThinkingBudget, ThinkingLevel}`, `FunctionCall{ID, Name, Args}`; its converters
  (`models.go`) give the REST field names where the SDK's own types differ from them.
- **bifrost** (`core/providers/gemini` at `3b31be003`) carries a function call's
  signature inside the call id (`<id>_ts_<base64url>`), injects the sentinel when one is
  missing, sends tool schemas as `parametersJsonSchema` untouched, never sends Gemini 3
  a zero budget, and sends the Gemini API arbitrary call ids. It never reads
  `toolUsePromptTokenCount` or a prompt's `blockReason`.
- **The gateway is less open than plan 59 says.** Two columns name the protocols
  (`anthropic_base_url`, `openai_base_url`, migration 0049) and a CHECK limits a
  credential's protocols to those two; "converted" means Chat Completions in every
  branch of `messages.go`; thinking provenance tells a converted block from a
  passthrough one by an empty wrapped value (`thinking.go`, `keptFor`); and the upstream
  URL is a base plus a fixed route suffix, with the model in the body.

## Decisions

1. **Gemini API only, by API key.** A `gemini` profile with one host,
   `https://generativelanguage.googleapis.com/v1beta`, the protocol `gemini`, and the
   key sent as `x-goog-api-key` (already a credential name to the provider-header
   check). No Google OAuth credential: Vertex needs one, and it stays #236's.
2. **One migration widens the store.** `providers.gemini_base_url`, the
   at-least-one-endpoint CHECK, and the credential-protocols CHECK gain `gemini`; the
   admin API's two protocol checks with them. The usage ledger's protocol is free text
   already.
3. **The model goes in the path.** A Gemini attempt's URL is the provider's base, then
   `/models/<upstream model, path-escaped>:generateContent`, or
   `:streamGenerateContent?alt=sse`; the request body carries no model. A model named
   `models/…` or `tunedModels/…` goes under that collection, as genai reads a name, the
   rest still escaped.
4. **Messages inbound only.** A Messages request (and `/v1/responses`, served as one)
   converts to Gemini when the alias's credential speaks only `gemini`; passthrough is
   still preferred where it exists. Chat Completions, embeddings and rerank never
   convert to Gemini, so an alias reachable only on Gemini answers them `503` as it
   would on an Anthropic-only one. `count_tokens` stays passthrough-only (`404` on a
   Gemini-only alias), as on the OpenAI direction: the brain never calls it.
5. **The request conversion** (`convert/gemini_request.go`), each field given a
   disposition the way `convert.Request` gives one:
   - `system` → `systemInstruction`; user and assistant turns → `user` and `model`;
     text → `text`; a base64 image → `inlineData` (a URL image is refused, as is a
     `document`); a final assistant turn is refused, counted once empty turns are
     dropped, as Gemini refuses a request ending on `model`.
   - `tool_use` → `functionCall{id, name, args}`; `tool_result` →
     `functionResponse{id, name, response}`, its name read off the `tool_use` with that
     id in the history, its text as `{"output": …}` or, with `is_error`,
     `{"error": …}`; images inside it as the function response's `parts`; a
     `search_result` through `provider.SearchResultText`.
   - tools → `functionDeclarations` with the caller's JSON Schema as
     `parametersJsonSchema`, unchanged; server tools refused; `tool_choice` `auto`,
     `any`, `tool`, `none` → `AUTO`, `ANY`, `ANY` with `allowedFunctionNames`, `NONE`,
     sent only beside a declaration; `disable_parallel_tool_use: true` refused (Gemini
     has no such switch, and a vendor that would ignore it is sent nothing, as DeepSeek
     is).
   - `max_tokens`, `temperature`, `top_p`, `top_k`, `stop_sequences` →
     `generationConfig`; `cache_control`, `metadata`, `service_tier` dropped.
   - thinking: `enabled` with `budget_tokens` → `thinkingBudget`; `adaptive` → no
     budget; either sets `includeThoughts` unless `display` is `omitted`; `disabled` or
     none sends no setting of its own, since Gemini 3 cannot stop thinking (Ground truth);
     `output_config.effort` → `thinkingLevel` (`xhigh` and `max` as `high`) when no budget is set,
     a level the model lacks answered by Gemini's own `400`.
6. **A function call's signature travels in a leading thinking block.** An answer
   with thought summaries or a signed `functionCall` opens with one `thinking` block:
   the summaries, when there are any, as its text — empty text otherwise, the shape a
   Claude 5 model returns with `display: "omitted"` — and as its signature the
   deployment's provenance wrapper around `gemini:` and the signature of the answer's
   first `functionCall`, or `gemini:` alone when it has none. An answer with neither
   opens with no thinking block, which would carry nothing back. Leading, because the brain stores no other block
   (Ground truth), and it replays that block within the tool loop (plan 60). On the next
   request the block contributes only its signature, set on that turn's first
   `functionCall`; its summary text is not sent back. A `functionCall` that would leave
   without a signature — history from another deployment, strip mode, a block the brain
   dropped — carries `skip_thought_signature_validator`. Text signatures are not
   carried: only a function call's absence was measured to be refused. Rejected:
   bifrost's signature in the `tool_use` id, which puts a kilobyte into every id the
   session's events carry. One more loss on streams: a function call streamed after
   text can no longer reach the leading block, so its signature is dropped and the
   sentinel answers for it on replay.
7. **Provenance names the protocol.** A wrapped value starting `gemini:` is a Gemini
   block (base64 holds no colon, so no Anthropic signature starts that way); an empty
   one stays the OpenAI direction's. `keptFor` and `producer` compare the block's
   protocol with the attempt's instead of a converting flag, so a Gemini signature goes
   back only to the Gemini deployment that produced it.
8. **The answer conversion** (`convert/gemini_answer.go`): the first candidate's parts
   become blocks; a `functionCall` without an id gets `toolu_` and a deterministic
   suffix. `STOP` is `tool_use` when the answer holds a call and `end_turn` otherwise;
   `MAX_TOKENS` is `max_tokens`, and so is `CONTINUATION`, an answer the server's own
   limit cut short; the safety class (`SAFETY`, `RECITATION`, `BLOCKLIST`,
   `PROHIBITED_CONTENT`, `SPII`, `IMAGE_SAFETY`, `IMAGE_PROHIBITED_CONTENT`,
   `IMAGE_RECITATION`) is `refusal`; a
   prompt blocked with no candidate (`promptFeedback.blockReason`) is an empty
   `refusal`; the malformed class (`MALFORMED_FUNCTION_CALL`, `UNEXPECTED_TOOL_CALL`,
   `TOO_MANY_TOOL_CALLS`, `MISSING_THOUGHT_SIGNATURE`, `MALFORMED_RESPONSE`), `OTHER`,
   `NO_IMAGE`, `LANGUAGE` and `IMAGE_OTHER` (no policy check stops either) and any
   unknown reason are a `502 api_error` naming it, its tokens still counted. Usage:
   input is `promptTokenCount` plus `toolUsePromptTokenCount` minus
   `cachedContentTokenCount`, which is the cache read; output is `candidatesTokenCount`
   plus `thoughtsTokenCount`. Errors go through `convertedError`, but a `400` with the
   reason `API_KEY_INVALID`, Gemini's answer to a key that is not valid (measured), is a
   refused credential, as its `401`, `402` and `403` are.
9. **The stream conversion** (`convert/gemini_stream.go`) re-emits each chunk's parts as
   Messages events: thought parts as `thinking_delta`, text as `text_delta`, a function
   call as one `tool_use` block with a single `input_json_delta`. The leading thinking
   block stays open until the first text, function call or finish; that call's signature, when
   the block is still open, is its `signature_delta`, and a call that comes first opens
   an empty block to carry it (decision 6). The
   last chunk's `finishReason` and `usageMetadata` become `message_delta`, and the
   stream ends at EOF, as Gemini sends no `[DONE]`. An error object that opens a `200`
   stream answers as that error's response, as before a stream; a later one becomes an
   `error` event.
10. **Gemini answers are prefix-unchecked** (plan 61's `X-MAP-Thinking-Prefix`), as
    every converted answer is. Whether Gemini checks a signature against the history
    before it is not measured. `thinkingRefusal` reads a `400` naming a
    `thought_signature`, the word both measured signature refusals use, so if it does,
    strip mode resends without thinking, and the sentinel answers for the missing
    signature.
11. **The wire authority** for Gemini is its REST reference, `google.golang.org/genai`
    at the version docs/REFERENCE_PROJECTS.md names, and the answers recorded here,
    kept as test data. The SDK is read, not imported.

## Design

- `internal/store/migrations/0052_modelgateway_gemini.sql`; `internal/modelgateway/store`
  and `admin` accept `gemini`; `admin_test.go`'s unknown-protocol example changes.
- `internal/modelgateway/profile`: `Gemini` beside `Anthropic` and `OpenAI`, and the
  `gemini` profile.
- `internal/modelgateway/catalog`: `upstreamProtocol` converts a Messages request to
  Gemini when the credential speaks `gemini` alone.
- `internal/modelgateway`: a Gemini attempt is read before the Chat Completions
  direction wherever the two part — as built, a `case up == profile.Gemini` ahead of each
  `conv` case rather than a named conversion; it builds its URL and `x-goog-api-key`
  header, converts the request, and reads the answer through the Gemini whole-answer
  and stream readers, the stream relayed by the Chat Completions direction's
  `convStream`, which takes either converter; the planning pre-check and `honoring` gain the Gemini direction's
  refusals and drops; `thinking.go` reads the protocol off a wrapped value.
- `internal/modelgateway/convert`: the three Gemini files above and their tests; the
  package comment names three directions.
- Two PRs: whole answers end to end (store, routing, request and answer conversion,
  provenance, header); then streaming, the live tier and the plan's close.

## Verification

- Unit: every request disposition, each refusal naming its field; the answer and stream
  readers over the recorded answers (plain, thought summaries, parallel calls with one
  signature, a streamed call, a streamed text ending on its signature part, a blocked
  prompt), the signature always in the leading block; after replay the signature on the
  turn's first `functionCall`, the sentinel where none survives; provenance keeping a
  Gemini block from an Anthropic or OpenAI attempt and theirs from a Gemini one.
- Gateway (fake upstreams): a Messages request served by a Gemini-only alias, whole and
  streamed; a two-turn tool loop whose second request carries the first answer's
  signature on its `functionCall`; passthrough preferred over Gemini; `503` for Chat
  Completions and `404` for `count_tokens` on a Gemini-only alias; a Responses request
  through Gemini; the URL, the header, and a `402` read as a refused credential.
- Live (`RUN_LIVE_MODELGATEWAY=gemini`, `GEMINI_API_KEY` in `.env`): `countTokens`
  answers `200` for the converted form of a request carrying a system prompt, tools with
  `$defs`, type unions and `additionalProperties`, a tool loop's history with ids and a
  signature, an image in a user turn and one in a tool result, and each thinking
  setting — Gemini's parser checking the shape at no cost. Generation rows (a whole and a
  streamed answer, a tool loop, the brain end to end) are written and run once a key can
  generate; until then they join #903, the live rows waiting on credentials.
- Every new guard mutation-tested; `make verify` green.

## Docs

DIVERGENCES.md: a Gemini conversion entry listing every disposition, the vendor-profile
entry gaining `gemini`, the thinking-provenance entry gaining the `gemini:` tag;
docs/REFERENCE_PROJECTS.md names the Gemini authority; ARCHITECTURE.md's gateway row
and lossy-conversion paragraph; CLAUDE.md's repo-layout line for `convert/` and its
lossy-conversion bullet; README's live-tier row; a changelog fragment per PR;
docs/HISTORY.md's record when the plan archives; #900 closes, and #903 gains the
generation rows.
