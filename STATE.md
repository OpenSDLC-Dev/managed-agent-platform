# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 69](./docs/plan/69_principal-bound-auth.md) slice 4: gateway authority, organization model budgets and principal usage.
Implementation and independent [acceptance](./docs/HISTORY.md#plan-69-slice-4-independent-acceptance) are complete; final committed review is in progress.
Slice 3 merged in [PR #1041](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/1041); its [client acceptance record](./docs/HISTORY.md#plan-69-slice-3-final-candidate-client-acceptance) remains the baseline.
**no release until plan 69 slice 7**.

## Tasks

- [x] Admit owned inference keys by current owner role; retain unowned grants and all private credential boundaries.
- [x] Enforce organization model budgets across replicas, including row waits, proxy cookies, strict replacement bodies and packaged routes.
- [x] Attribute usage to principals; preserve history and mixed writers, one completion timestamp and independent bounded writes.
- [x] Deliver the Console companion's principal usage, owner-role access and model budget controls.
- [x] Repeat contract tests, runtime acceptance, `make verify` and independent verification after review fixes.
- [ ] Complete dual code review.
