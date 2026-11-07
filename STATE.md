# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 69](./docs/plan/69_principal-bound-auth.md) slice 3: principal-owned key delivery.
Public owned-key admission, HTTP issuance and its mutation fence are verified. Console and integrated acceptance remain pending.
Slice 2 acceptance passed; see the [acceptance record](./docs/HISTORY.md#plan-69-slice-2-browser-acceptance).
**no release until plan 69 slice 7**.

## Tasks

- [x] Isolate owned digests; constrain identity, removal and old policy writes. Verified and reviewed in [PR #1022](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/1022).
- [x] Reject owned preset values at both servers' startup; add typed gateway-policy refusal. Verified and reviewed in [PR #1022](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/1022).
- [x] Obtain owned-key response/assignment evidence ([recordings #82](https://github.com/OpenSDLC-Dev/managed-agents-wire-recordings/issues/82#issuecomment-6702692704)). Lower-role visibility and `can_manage:false` remain unconfirmed in [#40](https://github.com/OpenSDLC-Dev/managed-agents-wire-recordings/issues/40).
- [x] Confirm both owners' direct-key Memory attribution ([recordings #83](https://github.com/OpenSDLC-Dev/managed-agents-wire-recordings/issues/83#issuecomment-6702693061)).
- [x] Verify owned public role/Bearer admission and remove public IdP/proxy sign-in, including cleaning redirects ([admission tests](./internal/api/ownedkeyauth_test.go), [redirect tests](./internal/api/ownedkeyredirect_test.go)).
- [x] Verify owned HTTP issuance and fenced per-row mutations ([issuer tests](./internal/api/ownedkeys_test.go), [fence tests](./internal/api/ownedkeymutation_test.go)). Migration 0061 rejects new human-unowned INSERTs; 0060 is unchanged.
- [ ] Update the native Console for principal-owned key creation and row permissions.
- [ ] Run real SDK/`ant` and fresh Compose acceptance; pair the Console source/image and add executable CI coverage.
