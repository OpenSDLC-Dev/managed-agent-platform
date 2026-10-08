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
- [x] Slice 2 — Anthropic inference, in seven PRs (2a–2g): `/v1/messages`, `count_tokens`
      and `/v1/models` with routing, retry and fallback; thinking provenance; the profiles'
      edits; usage and limits; telemetry; compose, Helm and a GCP identity (awaiting an
      operator's `foundation/` and `environment/` apply); the live tier
- [ ] Slice 4 — OpenAI surfaces, in four PRs
  - [x] 4a — Chat Completions passed through, `/v1/models` and errors in OpenAI's shape
  - [ ] 4b — embeddings and rerank with the `gitee` profile
  - [ ] 4c — Anthropic → Chat Completions conversion, for OpenAI-only credentials
  - [ ] 4d — dikw-core through the gateway
- [ ] Slice 5 — brain cutover: one route through the gateway; `ant` sessions on MiniMax
      and DeepSeek
- [ ] Slice 6 — stateless Responses
