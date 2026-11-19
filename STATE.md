# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Plan 70** ([docs/plan/70_security-hardening-vault-egress.md](./docs/plan/70_security-hardening-vault-egress.md), [#1052](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/1052)) — Vault credential destination binding and egress gate hardening.

## Tasks

- [x] Slice 1 — Vault credential destination binding (PR 1: API expansion check, sealed destination digest, resolver check, tests, docs)
- [ ] Slice 2 — Egress gate resource limits (PR 2: concurrency and aggregate body buffer budgets, Docker/Kubernetes gate container limits)
- [ ] Slice 3 — Egress policy withdrawal (PR 3: current-policy outbound admission, active tunnel and request withdrawal contract)
