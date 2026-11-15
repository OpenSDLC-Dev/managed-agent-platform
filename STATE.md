# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 69](./docs/plan/69_principal-bound-auth.md) slice 5: initiators and per-claim delegated inference tokens.
Implementation, independent [acceptance](./docs/HISTORY.md#plan-69-slice-5-independent-acceptance) and dual code review are complete.
Slice 4 merged in [PR #1042](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/1042); its [acceptance record](./docs/HISTORY.md#plan-69-slice-4-independent-acceptance) remains the baseline.
**no release until plan 69 slice 7**.

## Tasks

- [x] Record and backfill session, deployment and dream initiators; preserve old-writer compatibility.
- [x] Mint lease-bound inference tokens and enforce membership before every model turn, grading included.
- [x] Admit delegated inference at the gateway with principal usage and organization budgets; preserve credential boundaries.
- [x] Add cookie-only deployment service-account selection and scheduled-run refusal with automatic pause.
- [x] Deliver the Console companion and update the wire registry and operator documentation.
- [x] Complete contract and runtime tests, `make verify`, independent verification, dual review and CI.
