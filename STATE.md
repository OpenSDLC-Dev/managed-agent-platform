# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 69](./docs/plan/69_principal-bound-auth.md) slice 3: principal-owned key delivery.
Public admission and issuance are independently verified. Both pinned-source native and published-image packaged client tiers passed independent local verification; paired delivery remains pending.
Slice 2 acceptance passed; see the [acceptance record](./docs/HISTORY.md#plan-69-slice-2-browser-acceptance).
**no release until plan 69 slice 7**.

## Tasks

- [x] Isolate owned digests; constrain identity, removal and old policy writes. Verified and reviewed in [PR #1022](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/1022).
- [x] Reject owned preset values at both servers' startup; add typed gateway-policy refusal. Verified and reviewed in [PR #1022](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/1022).
- [x] Obtain owned-key response/assignment evidence ([recordings #82](https://github.com/OpenSDLC-Dev/managed-agents-wire-recordings/blob/a7fa9c0715c92f85ffbdf6f9d595e7d014d6a4d8/2026-11-07-owned-key-memory-actors/README.md)). Lower-role visibility and `can_manage:false` remain unconfirmed in [#40](https://github.com/OpenSDLC-Dev/managed-agents-wire-recordings/issues/40).
- [x] Confirm both owners' direct-key Memory attribution ([recordings #83](https://github.com/OpenSDLC-Dev/managed-agents-wire-recordings/blob/a7fa9c0715c92f85ffbdf6f9d595e7d014d6a4d8/2026-11-07-owned-key-memory-actors/README.md)).
- [x] Verify owned public role/Bearer admission and remove public IdP/proxy sign-in, including cleaning redirects ([admission tests](./internal/api/ownedkeyauth_test.go), [redirect tests](./internal/api/ownedkeyredirect_test.go)).
- [x] Verify owned HTTP issuance and fenced per-row mutations ([issuer tests](./internal/api/ownedkeys_test.go), [fence tests](./internal/api/ownedkeymutation_test.go)). Migration 0061 rejects new human-unowned INSERTs; 0060 is unchanged.
- [x] Verify and review the native Console source task for ownership and row permissions ([Console e9a383a](https://github.com/OpenSDLC-Dev/managed-agent-console/commit/e9a383a990e7d04682a97b1911b03839a4d097e4)).
- [x] Verify prepublication native browser/SDK clients and mixed receivers ([acceptance record](./docs/HISTORY.md#plan-69-slice-3-prepublication-client-checks)).
- [x] Independently verify the Docker context's exact local hook-cache exclusion ([COPY guard](./internal/sandbox/sandboxtest/buildcontext_test.go)).
- [x] Pair the merged Console source/image, add executable native CI coverage and independently verify both local client tiers ([acceptance record](./docs/HISTORY.md#plan-69-slice-3-paired-local-client-acceptance)).
- [ ] Independently verify the gateway mixed-credential follow-up, rebuild the affected image and repeat fresh packaged acceptance against the final product/configuration manifest.
- [ ] Pass hosted CI, obtain final dual review, settle the platform PR's review threads and complete paired delivery.
