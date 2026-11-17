# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 69](./docs/plan/69_principal-bound-auth.md) slice 7: remove preset credentials and legacy admission.
Slice 6 passed independent live [acceptance](./docs/HISTORY.md#plan-69-slice-6-independent-acceptance) after [PR #1046](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/1046).
**no release until plan 69 slice 7**.

## Tasks

- [x] Accept native staging HTTPS/OIDC and the Console-issued developer CD key stored in Secret Manager.
- [x] Deliver the native-only Console companion ([PR #261](https://github.com/OpenSDLC-Dev/managed-agent-console/pull/261)) and pin its verified image.
- [x] Require login, restrict private APIs to cookies and public keys to current owners, and remove legacy model policies.
- [x] Switch CD to its issued key; verify fresh Compose, real `ant` and local gates ([acceptance](./docs/HISTORY.md#plan-69-slice-7-local-installation-acceptance)).
- [ ] Complete committed reviews, hosted CI and native-only staging acceptance before cleanup.
- [ ] After accepting the code rollout, remove obsolete policy tables and retained compatibility secrets without breaking rollback.
- [ ] Retire the separate HTTP listener and legacy Console resources, complete live acceptance, and archive the plan.
