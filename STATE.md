# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Model gateway** ([plan 59](./docs/plan/59_model-gateway.md)): `cmd/modelgateway`, a
standalone server speaking Anthropic Messages and the OpenAI-compatible APIs to the brain
and to internal applications, configured from managed-agent-console. Five slices here;
slice 3 is the console repository's own plan.

## Tasks

- [x] Slice 1 — store and catalogue: the `modelgateway` schema (migration 0049), the
      platform key check shared (`internal/apikey`), the admin API under both auth modes,
      the notify-driven snapshot, the four chat vendors' profiles
- [ ] Slice 2 — Anthropic inference, routing, usage and limits, `cmd/modelgateway` in
      compose and Helm, the live tier on MiniMax (CN) and DeepSeek; freezes the admin API
- [ ] Slice 4 — OpenAI surfaces, embeddings and rerank with the `gitee` profile; dikw-core
      through the gateway
- [ ] Slice 5 — brain cutover: one route through the gateway; `ant` sessions on MiniMax
      and DeepSeek
- [ ] Slice 6 — stateless Responses
