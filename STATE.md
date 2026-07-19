# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#88](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/88) — a `"*"` pass-through route
lets a client-supplied model string reach the `gen_ai.request.model` metric attribute. The metric is
accepted as an operator responsibility (no behavior change); the same trigger's unbounded provider
cache is fixed. No plan file (single-PR fix; triage said `needs_plan: true` only for the product
decision, which the maintainer made).

## Tasks

- [x] Decision recorded: pass-through cardinality documented in `deploy/compose/README.md` and the
      Helm `modelProviders` values comment, with the rationale in CHANGELOG.
- [x] Provider cache deleted (registry is now immutable and lock-free), pinned by
      `TestRegistryRetainsNothingPerModelString` and
      `TestRegistryDefaultRouteWithUpstreamModelIgnoresClientString` — both fail on `main`.
- [x] Verifier PASS, re-run after the review fixes; it measured the leak at 258 MB retained per
      200k distinct model strings on `main` versus none on the branch.
- [x] Reviews: Codex (`gpt-5.6-sol`, `ultra`) found the aliased factory table and a `Factory` doc
      that would have rebuilt #88 inside an adapter — both fixed; one finding refuted. A four-
      dimension Opus 4.8 pass returned six findings, all six refuted with evidence.
- [x] PR [#117](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/117) green on CI.
- [ ] `/code-review` (user-invocable only) before merge.
