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
- [x] Reviews: Codex found the aliased factory table and a `Factory` doc that would have rebuilt #88
      inside an adapter. `/code-review` (Opus 4.8, xhigh) returned ten defects, seven taken —
      chiefly that the cardinality warning omitted session `agent_with_overrides` as an injection
      point, and that two of the new tests did not fail on the mutation they claimed to fence.
- [x] Timeout risk the reviews surfaced, pre-existing and out of scope, filed as
      [#121](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/121).
- [x] PR [#117](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/117) green on CI after
      the review fixes; ready to leave draft.
