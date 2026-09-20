# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**#375 — external tool waits** ([plan 52](docs/plan/52_external-tool-waits.md)).
The ordered result processing implementation passed independent verification;
dual code review, PR and CI are pending.

## Tasks

- [x] #375 — real API/SSE regression, ordered waits and recovery, lifecycle checks
- [x] #375 — make verify (90.03%), recording comparison, independent verification
- [ ] #375 — dual code review, PR, green CI and squash merge
