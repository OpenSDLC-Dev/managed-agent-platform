# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 69](./docs/plan/69_principal-bound-auth.md) slice 1: stored roles and first-admin setup.
**no release until plan 69 slice 7**. Slice 1 awaits independent verification and review.

## Tasks

- [ ] Principal role/kind/deactivation schema and deletion guard: implemented; verification pending.
- [ ] Three roles and optional legacy-compatible mapping: implemented; verification pending.
- [ ] Shared stored-role resolver on the control-plane identity lane: implemented; verification pending.
- [ ] Resource role matrix and developer vault-credential writes: implemented; verification pending.
- [ ] Gateway operator token uses the shared resolver: implemented; verification pending.
- [ ] Once-ever setup and restricted local delivery: implemented; verification pending.
- [ ] Docs, Helm and upgrade notes: prepared; independent verification and review pending.
