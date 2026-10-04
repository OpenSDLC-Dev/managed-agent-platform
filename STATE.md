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
- [ ] Slice 2 — Anthropic inference, in four PRs; freezes the admin API
  - [x] 2a — `cmd/modelgateway`: `/v1/messages` (streamed and not) and `count_tokens`
        passed through, `/v1/models`, routing with retry and fallback, the stall guard
  - [ ] 2b — thinking provenance and the strip-mode backstop; the profiles' auth header
        and edits (2a sends every upstream `x-api-key`)
  - [ ] 2c — usage rows, retention and rollups; RPM and TPM limits; telemetry
  - [ ] 2d — compose and Helm, and a GCP identity granted the cipher (ending its
        `unhosted` entry in `tools/kmsrole`); the live tier on MiniMax (CN) and DeepSeek
- [ ] Slice 4 — OpenAI surfaces, embeddings and rerank with the `gitee` profile; dikw-core
      through the gateway
- [ ] Slice 5 — brain cutover: one route through the gateway; `ant` sessions on MiniMax
      and DeepSeek
- [ ] Slice 6 — stateless Responses
