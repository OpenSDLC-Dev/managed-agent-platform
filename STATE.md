# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 69](./docs/plan/69_principal-bound-auth.md) slice 7b: retire obsolete gateway tables, credentials and the separate HTTP listener.
Slice 7a deployed in [PR #1048](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/1048).
[Staging acceptance](./docs/HISTORY.md#plan-69-slice-7a-staging-acceptance) records the live login, owned credentials and delegated model turn.
**no release until plan 69 slice 7**.

## Tasks

- [x] Accept native staging HTTPS/OIDC and the Console-issued developer CD key stored in Secret Manager.
- [x] Deliver the native-only Console companion ([PR #261](https://github.com/OpenSDLC-Dev/managed-agent-console/pull/261)) and pin its verified image.
- [x] Require login, restrict private APIs to cookies and public keys to current owners, and remove legacy model policies.
- [x] Switch CD to its issued key; verify fresh Compose, real `ant` and local gates ([acceptance](./docs/HISTORY.md#plan-69-slice-7-local-installation-acceptance)).
- [x] Verify hosted CI, native login, legacy credential refusal and a delegated staging model turn.
- [ ] Confirm direct staging inference with an owned key before cleanup.
- [ ] Verify and review the cleanup migration and private Service configuration, then deploy after native-only acceptance.
- [ ] Retire retained compatibility secrets and verify rollback to the accepted native-only application build.
- [x] Confirm retirement of the standalone legacy Console and verify the native entry point still works.
- [ ] Retire the separate HTTP listener, complete live acceptance, and archive the plan.
