---
status: archived
issue: "#67"
---

# Thinking persistence and replay (plan 60)

The brain throws away every thinking block a model sends. The provider vocabulary
carries thinking text but neither a block's `signature` nor a `redacted_thinking`
block (`internal/provider/provider.go`; `internal/provider/anthropic/anthropic.go`
handles `thinking_delta` alone), the wire `agent.thinking` event is content-free
(`{id, processed_at, type}`, checked against anthropic-sdk-go v1.70.1 —
betasessionevent.go BetaManagedAgentsAgentThinkingEvent), and replay renders it as
nothing (`internal/brain/replay.go`). That was safe while no model thought unasked
(#67). It no longer is: every model the platform now targets thinks by default, so
each tool-use continuation the brain sends drops the reasoning that led to the call.
After this plan the brain keeps each committed turn's signed thinking internally and
sends it back on later requests, under a guard that never turns a changed prompt into
a rejected one. Plan 59 (the model gateway) waits on this for its brain cutover.

## Ground truth (verified 2026-10-04)

- **Anthropic.** "Required: within a tool-use turn, pass thinking blocks back.
  Recommended: across turns, pass everything back. Allowed: outside tool use, omit
  prior turns' thinking", complete and unmodified, `redacted_thinking` included
  ([thinking](https://platform.claude.com/docs/en/build-with-claude/thinking#preserving-thinking-blocks)).
  Every Claude 5-generation model thinks with no `thinking` field and defaults
  `display` to `"omitted"`: the block's `thinking` is empty, its `signature` carries
  the reasoning, and a stream sends an empty `thinking_delta` then a
  `signature_delta`. From Claude Fable 5.1 the API checks each returned block
  against everything sent before it — the top-level `system`, the `tools`, every
  earlier message — and a changed prefix is a 400 by default on accounts created on
  or after 2026-08-31; dropping thinking blocks from the start of the history, from
  its end, or all of them stays valid, while removing one from the middle and keeping
  later ones does not; an undecryptable signature is always a 400; a block the
  current model cannot read is dropped without error
  ([preserved thinking](https://platform.claude.com/docs/en/build-with-claude/preserved-thinking)).
- **DeepSeek and MiniMax**, probed live in tool loops of up to four requests:
  `deepseek-flash` and `deepseek-v4-pro` (both of DeepSeek's endpoints, with and
  without `thinking: enabled`) return signed thinking with no `thinking` field sent,
  36-character signatures; on `https://api.minimax.cn/anthropic`,
  `MiniMax-M3.1-Flash-Preview` thinks after a tool result rather than before its first
  call — `[thinking, text, tool_use]` on the second request — with 64-character
  signatures; `MiniMax-M3`, whose thinking is off unless a request sends `thinking:
  {"type": "adaptive"}`
  ([Anthropic API](https://platform.minimax.cn/docs/api-reference/text-anthropic-api)),
  returned none until asked and signed blocks every round once asked. DeepSeek
  documents a 400 for a tool-using request that omits `reasoning_content`
  ([thinking mode](https://api-docs.deepseek.com/zh-cn/guides/thinking_mode)), and
  enforces it where the tool ids are not its own: on its Anthropic endpoint both models
  refuse a request ending in tool results whose `tool_use` turn carries no thinking and
  an id DeepSeek did not issue — `` The `content[].thinking` in the thinking mode must
  be passed back to the API. `` — and answer the same request 200 under DeepSeek's own
  ids (`call_00_…`), the reasoning restored server-side. A made-up signature, or none,
  is accepted, and a loop followed by a later user message, beside the tool result or
  after it, is not checked. The brain sends event ids as tool ids, so every DeepSeek
  tool loop it drove was refused at its first tool result; the first probes kept
  DeepSeek's ids and missed that, and this plan's live test found it (Verification).
  MiniMax answered every continuation 200 without its thinking, with it, and with its
  text edited under its own ids, and without its thinking under foreign ones
  (`MiniMax-M2.7` directly, `MiniMax-M3.1-Flash-Preview` through the brain's event
  ids). So Anthropic's API and
  DeepSeek's enforce replay, by different rules, and MiniMax's does not.
- **The SDK at the pin** models both missing pieces: `signature_delta` on
  `RawContentBlockDeltaUnion.Signature`, and `redacted_thinking`'s `Data` on the
  content-block-start union (anthropic-sdk-go v1.70.1, `message.go`).

## Decisions

1. **Persist internally, never on the wire.** `agent.thinking` keeps its shape. The
   block goes to a new table, `thinking_blocks`, keyed by the thinking event's id
   and cascading with its session; nothing that serves events reads it.
2. **Persist at settlement, in the settlement's transaction.** A thinking event is
   appended while the turn streams, as today; its block is written only when the
   turn commits, by the same transaction that appends the turn's message and tool
   intents. A turn that fails, loses its lease or is interrupted leaves its thinking
   events content-free, as every thinking event is today, so a reclaimed turn's
   replay never mixes an abandoned attempt's blocks into the next one's.
3. **Only a response's leading signed blocks, and only beside an answer.** What is
   stored is the run of thinking blocks a response opens with: each `thinking` block
   with a non-empty signature (an unsigned one is refused by an endpoint that
   checks) and each `redacted_thinking` block, up to the first text, tool call or
   unsigned block. A block after that point would replay ahead of the text before it
   — the log appends a turn's thinking as it streams and its text at settlement —
   and so under a prefix it was not produced under; dropping it instead is dropping
   from the end of the history, which stays valid, and every later block is produced
   without it. Nothing is stored when the turn committed no text and no tool call:
   an assistant message of thinking alone is not a turn the Messages API accepts
   back, and such a reply already "closes nothing" in replay's outcome logic.
4. **Replay every admitted block in place.** "Pass everything back" is the
   recommendation, and the API, not the brain, decides which earlier blocks a model
   keeps. Blocks render ahead of the turn's text and tool calls, where the response
   had them (decision 3).
5. **The guard: same model, same prefix — the API's own rule, checked first.** Each
   stored block records the upstream model id its request was sent to and a SHA-256
   over everything the model read before the block: the request's `system`, its
   `tools`, every content block of its messages with its role, and the blocks of its
   own response ahead of it. Replay sends a block only when the model and the digest
   it computes at that point of the request it is building both match; an admitted
   block joins what later blocks are checked against, a refused one does not. A
   model switch would otherwise send one vendor's signature to another (a DeepSeek
   signature is undecryptable to Anthropic: a 400). A changed system prompt or tool
   set — a `system.message` folded into the prompt, the dated `web_search`
   description at midnight, a resource or MCP tool added mid-session, a skill that
   failed to load once — or any reordering of earlier messages would otherwise make
   a block invalid, a 400 on newer accounts. Under the guard those blocks drop
   instead, and the request stays valid: a block is sent only under the prefix it
   was produced under, and every block produced after a drop was produced without
   the dropped one, so the set sent is always one the API accepts. The cost of a
   prefix change is the reasoning before it, never the request. Hashing per block
   rather than per request (a digest of `system` and `tools` alone was the first
   draft) makes the check exact instead of an argument about which renderings can
   change after the fact; it costs one SHA-256 pass over each request, on both
   sides. The guard is Anthropic's rule, and DeepSeek's is another: it checks no
   prefix, but refuses a tool continuation whose loop lost its thinking under foreign
   ids (Ground truth). So a prefix change in the middle of a DeepSeek tool loop — the
   same rare events — costs that one request: its retries meet the same 400, the turn
   ends in a `session.error`, and the next user message lifts the check. Sending the
   block anyway would cost an Anthropic route the same request instead; which rule a
   route follows needs knowledge of the vendor the brain does not have (#883).
6. **Considered and not done here: making the request append-only.** Rendering
   `system.message` in place (as a mid-conversation system message, which only
   Claude Fable 5.1, Opus 5.5 and Sonnet 5.5 accept, or as user text, which changes
   what the model reads) and freezing the date would keep more reasoning across
   those events. The guard makes them an optimization rather than a correctness fix,
   and each changes model-visible rendering, so each waits for evidence that the
   lost reasoning matters.
7. **OpenAI protocol: unchanged.** `provider/openai` reads no `reasoning_content`
   and keeps dropping thinking blocks on the way out (its documented lossy gap).
   Behind the model gateway the brain speaks Anthropic Messages to every v1 vendor,
   DeepSeek's OpenAI endpoint answered without `reasoning_content` live, and making
   that adapter surface reasoning is a visible change of its own.

## Design

- **Provider vocabulary** (`internal/provider`): `KindThinkingSignature` (signature
  text for the block at `Index`, concatenated across deltas) and
  `KindRedactedThinking` (the opaque `Data` of a complete block at `Index`), with
  `Chunk.Signature` and `Chunk.Data`. The anthropic adapter maps `signature_delta`,
  `redacted_thinking` starts, and a `thinking` start that already carries text or a
  signature (an endpoint may send the block whole, as it may a tool input).
- **Stream** (`internal/brain/stream.go`): a thinking block accumulates its text and
  signature verbatim until it closes; a redacted block opens and closes its event at
  once. `turnResult.thinking` holds `{eventID, prefix digest, block}` for each
  storable block, the digest chained from the request the turn sent.
- **Store** (`internal/store/migrations/0048_thinking_blocks.sql`, `internal/events`):
  `thinking_blocks(event_id PRIMARY KEY → events ON DELETE CASCADE, session_id, model,
  prefix_digest, block json, created_at)`. `json`, not `jsonb`: it keeps the bytes the
  brain wrote, which are the bytes a signature covers, and accepts the `\u0000`
  escape that `jsonb` refuses. `AppendOptions.Thinking` is written by the append's
  own transaction; `Log.ThinkingBlocks` reads a session's rows for replay.
- **Settlement** (`internal/brain`): `commitTurn` sets `opts.Thinking` when the turn
  committed text or a tool call; the tool, end-turn and delegated paths all append
  with it.
- **Replay**: `buildRequest` takes the stored blocks and the request's model,
  renders each `agent.thinking` with a stored block as that block, and, once the
  final `system` is known (its `system.message` tail comes last), admits or removes
  each in one pass over the request in order.

## Verification

- Adapter contract: a recorded stream with an empty `thinking_delta` then a
  `signature_delta`, a summarized block, a `redacted_thinking` block and a whole
  thinking start, each mapped to chunks.
- Stream and settlement: stored only on commit, only signed or redacted, only beside
  text or a tool call, on each of the three settlement paths (tool, end turn,
  delegated); a failed turn stores nothing, and a lost lease is a settlement whose
  transaction rolls back (Store).
- Replay: in-place order; a model mismatch, a tool set change and a reordered earlier
  message each drop the block, and a `system.message` drops exactly the blocks
  produced before it; the digest a turn stores equals the one the next build computes
  for the same block; a non-leading block is never stored.
- Store: `pgtest` — rows roll back with a failed settlement and go with their
  session.
- Live tier (`RUN_LIVE_MODEL_TESTS`, `MODEL_*` pointed in turn at `deepseek-flash`,
  `deepseek-v4-pro`, `MiniMax-M3` and `MiniMax-M3.1-Flash-Preview`):
  `TestLiveThinkingReplay` drives a tool loop of up to four requests through the brain
  and the anthropic adapter, behind a local reverse proxy that keeps each request body
  as sent, and checks that every request carries, as stored, each block the turns
  before it kept, and is answered. The bodies are read off the wire because an
  endpoint that does not enforce replay answers just the same without the blocks. The
  brain sends no `thinking` field, so `MiniMax-M3` runs its loop without thinking and
  the test log says so rather than claiming a replay; letting a route ask a model to
  think is not this plan's. Everything the thinking path touches is the brain's, which
  the test runs whole, so no compose session is added; the results are in
  docs/HISTORY.md.

## Docs

`replay.go`'s mapping comment; docs/ARCHITECTURE.md's execution flow (the brain now
keeps internal state beside the log); docs/DIVERGENCES.md's thinking entry, which
#67 tracked, moved from the inferences to the architecture notes, since no wire shape
changes; docs/HISTORY.md's record; two changelog fragments, the replay added and
DeepSeek's tool loops fixed.
