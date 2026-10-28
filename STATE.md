# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 69](./docs/plan/69_principal-bound-auth.md) slice 2 is in progress.
Task 1 prepares browser-session and local-credential primitives only.
**no release until plan 69 slice 7**.

## Tasks

- [x] Browser sessions and local credentials: [session](./internal/login/session_test.go),
  [password](./internal/login/password_test.go) and [throttle](./internal/login/throttle_test.go) contracts.
- [ ] Add the OIDC code flow, native login and setup, and management routes.
  No HTTP handler uses the new primitives yet.
