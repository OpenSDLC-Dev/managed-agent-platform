# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 69](./docs/plan/69_principal-bound-auth.md) slice 3: backend safety preparation, not owned HTTP issuance.
Slice 2 acceptance passed; see the [acceptance record](./docs/HISTORY.md#plan-69-slice-2-browser-acceptance).
**no release until plan 69 slice 7**. Slice 3 is not ready to merge.

## Tasks

- [ ] Isolate owned digests; constrain identity, removal and old policy writes. Candidate awaits independent verification and review.
- [ ] Reject owned preset values at both servers' startup; add typed gateway-policy refusal.
- [ ] Obtain private owned-key response/assignment evidence ([recordings #82](https://github.com/OpenSDLC-Dev/managed-agents-wire-recordings/issues/82)); keep lower-role visibility and `can_manage:false` in [#40](https://github.com/OpenSDLC-Dev/managed-agents-wire-recordings/issues/40).
- [ ] Obtain service-account direct-key memory attribution evidence ([recordings #83](https://github.com/OpenSDLC-Dev/managed-agents-wire-recordings/issues/83)).
- [ ] Complete owned issuance/authorization and its mutation fence; refuse legacy human INSERTs when activating the new issuer.
- [ ] Complete owned public role/Bearer admission, remove IdP/proxy admission from `/v1`, update Console and run integrated acceptance.
