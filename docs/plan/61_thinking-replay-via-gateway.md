---
status: draft
issue: "#883"
---

# Thinking replay through the model gateway (plan 61)

Plan 60 sends a stored thinking block back only to the model id that produced it
and only under the prefix it was produced under — Anthropic's rule, which the
brain applies to every route (`admitThinking`, `internal/brain/thinking.go`).
DeepSeek's rule is another: it checks no signature and no prefix, but refuses a
request ending in tool results whose `tool_use` turn lost its thinking (#883,
probed 2026-10-04). So a prefix change in the middle of a DeepSeek tool loop — a
`system.message` folded into the prompt, the dated `web_search` description at
midnight, a resource or MCP tool added, a skill that failed to load once — drops
the loop's blocks, DeepSeek answers 400 on every retry, and the turn ends in a
`session.error` until the next user message.

#883 offered three answers; the user chose its option 2 (2026-10-09): the
vendor's rule lives in the model gateway's vendor profiles, and the brain learns
it from the gateway. The brain cannot know it alone — a provider is protocol,
model, base URL and key (design principle 4) — and the gateway already knows
which deployment answered and what its vendor checks.

## Ground truth (verified 2026-10-09)

- **The brain keeps a block per response, then admits it per request.** The
  stream stores each leading signed block with the digest of the request prefix
  it was produced under (`internal/brain/stream.go`, `keep`); replay admits a
  stored block when its model equals the request's and its digest equals the
  prefix chain at its position, which opens with the route
  (`provider.Descriptor.Route`). `thinking_blocks.prefix_digest` is plain `text`
  with no constraint (migration 0048).
- **The gateway already sends each deployment its own blocks and no other.**
  Every thinking block it returns carries its producer in a wrapped signature
  (`mapgw1.<deployment>.<value>`), and every history is filtered per deployment
  before it goes upstream (`internal/modelgateway/thinking.go`). A block a
  deployment did not produce never reaches it, whatever the brain sends.
- **On the conversion path no upstream checks anything.** A Messages request
  served by a Chat Completions upstream gets thinking blocks the gateway signs
  itself, its wrapper around an empty value (`signer`,
  `internal/modelgateway/converted.go`); such a block goes back as
  `reasoning_content`, "which DeepSeek requires in a tool loop"
  (`internal/modelgateway/convert/messages.go`).
- **The brain reads no response header today.** The Anthropic adapter reaches the
  response only through its SDK middleware (`internal/provider/anthropic`), which
  wraps the body for the stall guard. The gateway's answer carries headers it
  writes itself; an upstream's reach the caller only on a failure, and only
  `Content-Type`, `Retry-After` and `X-Should-Retry` (`failure.write`).
- **Anthropic's wording for a block replayed under another prefix is not
  recorded** — plan 60 records the rule (decision 5), not the refusal's text — so
  the strip-mode backstop (`thinkingRefusal`) catching it is unverified.

## Decisions

1. **The signal is a response header the gateway writes:
   `X-MAP-Thinking-Prefix: unchecked`.** Named like `X-MAP-Session-ID`, the
   brain's request header to the gateway. It says the answer's thinking may go
   back under any prefix. Rejected:
   - a mark inside the signature wrapper — the brain would parse a field both
     the Messages API and the gateway call opaque, and bind itself to the
     wrapper's format;
   - sending every block on every route and leaning on strip mode — Anthropic
     deployments would see a refused request each time a prefix changes, and
     whether strip mode catches that refusal is unverified (Ground truth);
   - a per-route flag in the brain's model-provider config — #883's option 1,
     not chosen: a route to the gateway spans vendors, so the rule is per
     answer, not per route.
2. **The gateway writes it on a successful Messages answer whose thinking no
   upstream checks against a prefix:**
   - any converted answer — the gateway signed its blocks itself;
   - a passthrough answer from a deployment whose vendor profile sets
     `ThinkingAnyPrefix`: DeepSeek, by #883's probe. Every other vendor keeps
     Anthropic's rule until a probe shows otherwise; MiniMax's Anthropic
     endpoint is probed in this plan the same way, and flagged only on that
     evidence.
   The header is the gateway's own, written after the attempt that answered is
   known; an upstream's header of that name is never relayed.
3. **The brain stores such a response's blocks under the route alone.** The
   Anthropic adapter reads the header off the response and reports it on the
   done chunk (`Chunk.ThinkingAnyPrefix`, `KindDone` only). The stream then
   stores the response's kept blocks with the digest `any:` followed by the hex
   SHA-256 of the framed route, in place of the prefix chain's — a value no
   chain digest (bare hex) can equal, so no migration. Blocks stored before this
   change keep their digests and the rule they were stored under.
4. **Replay admits a block whose digest is either one.** `admitThinking` keeps a
   stored block when its model matches and its digest equals the chain at its
   position or the route's `any:` digest. The model check stays, and the route
   binding keeps a block from leaving the route it came over; the gateway's
   provenance (Ground truth) keeps it from reaching any deployment but its
   producer.
5. **A direct route keeps the gap.** A brain route straight to DeepSeek has no
   gateway to tell it the rule, so it keeps Anthropic's, and #883's failure,
   there. The gateway is the brain's default route (plan 59 slice 5b; GCP staging
   turns it on under #906). An endpoint other than the gateway that wrote the
   header would relax the brain for its own route alone, and a refusal that
   follows drops every kept block (plan 60), as today.

## Design

- `internal/modelgateway/profile`: `ThinkingAnyPrefix bool` on `Profile`, set on
  DeepSeek, with the probe it rests on in its comment.
- `internal/modelgateway`: the attempt that answers a Messages request reports
  whether its thinking is prefix-unchecked (converted, or the deployment's
  profile flag); the success path writes the header before the first byte, on
  streamed and whole answers alike.
- `internal/provider`: `ThinkingPrefixHeader` beside `SessionHeader`, and
  `ThinkingAnyPrefix` on `Chunk`.
- `internal/provider/anthropic`: the middleware records the header in a holder
  the call's context carries; the stream sets it on its done chunk.
- `internal/brain`: `anyPrefixDigest(route)`; the stream rewrites the turn's kept
  blocks' digests when the done chunk says so; `admitThinking` accepts either
  digest.

## Verification

- Gateway: the header on a converted answer and on a passthrough answer from a
  flagged deployment, streamed and whole; absent on an unflagged deployment's
  answer, on an error, and when only a fallback attempt differs (the answering
  attempt decides); an upstream's own header of that name never reaches the
  caller.
- Adapter: a recorded stream with the header reports it on the done chunk, one
  without reports nothing; two concurrent calls on one client never see each
  other's.
- Brain: a block stored under the `any:` digest is admitted after a
  `system.message`, a tool set change and a reordered earlier message, and
  dropped on a model change and a route change; a response without the header
  stores chain digests exactly as before; the digest a turn stores is the one the
  next build admits.
- Live (`RUN_LIVE_MODEL_TESTS`, DeepSeek through an in-process gateway): a tool
  loop whose system prompt changes between the call and its result carries the
  loop's thinking and is answered 200, on DeepSeek's Anthropic endpoint and
  through conversion; the same loop on the code before this plan meets
  DeepSeek's 400. MiniMax's probe result, flagged or not, is recorded with it.

## Docs

The two DIVERGENCES.md entries the change moves (agent.thinking replay; the
gateway's thinking provenance, which gains the header); ARCHITECTURE.md's
execution flow, whose #883 caveat narrows to direct routes; a changelog
fragment; docs/HISTORY.md's record when the plan archives; #883 closes.
