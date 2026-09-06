# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Plan 42 (#56) — multi-tenant activation: the workspace becomes a real scoping key.**
Six slices. Slice 1 lands with this PR: the workspace registry (seeded `default`),
every credential resolving one `domain.Scope`, both tenancy response headers, and
the one header-narrowing rule (its malformed- and not-found bodies verbatim from
the reference). Slices 2-6 are not started.

**#78 — confirming documented wire assumptions against a real managed-agents endpoint.**
Four recording days, 1,301 request/response pairs for about US$1.12, observed once at
cost in a private archive; 2026-09-05's batch was the first multi-workspace capture,
the recording plan 42 slice 1 was gated on. #78 stays open — 121 registry entries name
it as their live tracker, and the debt is now analysis rather than recording.

## Tasks

- [x] Slice 1 — workspace registry, per-credential scope, tenancy response headers,
      the header-narrowing rule (#56)
- [ ] Slice 2 — the scope-stamping completeness guard
- [ ] Slice 3 — every insert stamps the scope; the write floor
- [ ] Slice 4 — read predicates and cross-references (the behavior change)
- [ ] Slice 5 — the leaks inheritance does not cover
- [ ] Slice 6 — the operator surface, the acceptance run, close-out (#56 closes)
- [ ] Read the 72 un-analysed comparison rows from 2026-09-02 (#78)
