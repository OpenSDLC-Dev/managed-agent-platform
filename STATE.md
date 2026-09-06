# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Plan 41 — dreams** ([docs/plan/41_dreams.md](./docs/plan/41_dreams.md), [#475](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/475)), five slices.
Slice 1 landed the five `/v1/dreams` routes, `drm_` and migration `0034` with no runner behind them —
a created dream stays `pending` until slice 2. Slice 0's recording waits on preview enrollment.

**Plan 42 — multi-tenant activation** ([docs/plan/42_multi-tenant-activation.md](./docs/plan/42_multi-tenant-activation.md), [#56](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/56)),
six slices. Slice 1 lands with this PR: the workspace registry (migration `0035`, seeded `default`),
every credential resolving one `domain.Scope`, both tenancy response headers, the header-narrowing rule.

**#78 — confirming documented wire assumptions against a real managed-agents endpoint.** Four
recording days, 1,346 request/response pairs for about US$1.12, in a private archive. It stays open:
128 registry entries name it as their live tracker, and the debt is analysis, not recording.

## Tasks

- [x] Plan 41 slice 1 — the surface and the storage: the five routes, `drm_`, migration `0034`,
      create-time validation, the list, and archive and cancel over the state machine
- [ ] Plan 41 slice 2 — the runner, with a one-stage pipeline · slice 3 — the full pipeline · slice 4 — `update_existing`
- [ ] Record plan 41's `/v1/dreams` checklist (its §8.2) — attempted 2026-09-05, 45 pairs, US$0; waits on preview enrollment
- [x] Plan 42 slice 1 — workspace registry, per-credential scope, tenancy response headers, the header rule (#56)
- [ ] Plan 42 slice 2 — the scope-stamping completeness guard
- [ ] Plan 42 slice 3 — every insert stamps the scope; the write floor
- [ ] Plan 42 slice 4 — read predicates and cross-references (the behavior change)
- [ ] Plan 42 slice 5 — the leaks inheritance does not cover
- [ ] Plan 42 slice 6 — the operator surface, the acceptance run, close-out (#56 closes)
- [ ] Read the 72 un-analysed comparison rows from 2026-09-02 (#78)
