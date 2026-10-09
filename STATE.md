# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 61](./docs/plan/61_thinking-replay-via-gateway.md) (#883): a stored thinking
block goes back under any prefix when the model gateway says the vendor that
produced it checks none (`X-MAP-Thinking-Prefix: unchecked`).

## Tasks

- [ ] Gateway: `ThinkingAnyPrefix` on DeepSeek's profile; the header on converted and flagged answers
- [ ] Anthropic adapter: the header reported on the done chunk
- [ ] Brain: blocks stored under the route's `any:` digest, and admitted under any prefix
- [ ] MiniMax probed; live loop through the gateway on DeepSeek, passthrough and converted
- [ ] Docs: DIVERGENCES.md, ARCHITECTURE.md, changelog fragment; the plan archives and #883 closes
