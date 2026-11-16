# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 69](./docs/plan/69_principal-bound-auth.md) slice 6: move installs and callers onto native login and delegated inference.
Slice 5 merged in [PR #1043](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/1043); its independent [acceptance](./docs/HISTORY.md#plan-69-slice-5-independent-acceptance) is the baseline.
**no release until plan 69 slice 7**.

## Tasks

- [x] Send per-claim credentials from Brain to configured gateway routes, including grading.
- [x] Default Compose to password login and require a login mode in Helm.
- [x] Obtain CI, acceptance and eval credentials through setup, login and private issuance.
- [x] Deliver Console key management for both environment kinds ([Console PR #260](https://github.com/OpenSDLC-Dev/managed-agent-console/pull/260)).
- [x] Correct the app-edge capability failure found during the slice 6 staging rollout; verify the rendered container settings at runtime.
- [ ] Configure staging HTTPS and Google OIDC; obtain its CD service-account key through Console.
- [x] Complete local contract/runtime tests and independent `make verify` ([acceptance](./docs/HISTORY.md#plan-69-slice-6-independent-acceptance)).
- [ ] Complete committed dual review, CI and live staging acceptance before slice 7.
