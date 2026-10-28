# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 69](./docs/plan/69_principal-bound-auth.md) slice 2 is in progress.
Native HTTP login and first-admin setup are implemented.
Member and service-account management is implemented; private resource access is next.
**no release until plan 69 slice 7**.

## Tasks

- [x] Browser sessions and local credentials: [session](./internal/login/session_test.go),
  [password](./internal/login/password_test.go) and [throttle](./internal/login/throttle_test.go) contracts.
- [x] OIDC code flow and browser-bound attempts: [RP](./internal/identity/oidc_test.go)
  and [attempt](./internal/login/oidc_test.go) contracts.
- [x] Native HTTP login and first-admin setup: [login](./internal/api/login_test.go),
  [setup](./internal/api/passwordsetup_test.go) and [hardening](./internal/api/login_fix1_test.go) contracts.
- [x] Member and service-account management: [members](./internal/api/members_test.go),
  [service accounts](./internal/api/serviceaccounts_test.go) and [concurrency](./internal/api/members_race_test.go) contracts.
- [ ] Add private resource proxy and gateway cookie admission.
