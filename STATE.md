# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 69](./docs/plan/69_principal-bound-auth.md) slice 4: gateway authority, organization model budgets and principal usage.
Implementation and independent acceptance are verified in the [acceptance record](./docs/HISTORY.md#plan-69-slice-4-independent-acceptance).
Slice 3 merged in [PR #1041](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/1041); its [client acceptance record](./docs/HISTORY.md#plan-69-slice-3-final-candidate-client-acceptance) remains the baseline.
**no release until plan 69 slice 7**.

## Tasks

- [x] Admit owned inference keys by current owner role; retain unowned grants and all private credential boundaries.
- [x] Enforce organization RPM, input TPM and output TPM by public alias, with cookie-only management and current-admin mutation fences.
- [x] Attribute usage to principals; preserve historical totals, credential filtering and mixed-version writers.
- [x] Deliver the Console companion's principal usage, owner-role access and model budget controls.
- [x] Complete contract tests, runtime acceptance, `make verify` and independent verification.
- [ ] Complete dual code review.
