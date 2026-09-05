# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Plan 43 (#609) — one host comparison, canonicalized.** Three hand-rolled ASCII
case folds become one `egress.CanonicalHost` comparing IDNA A-labels, and an
environment's `networking.allowed_hosts` — arbitrary strings until now — is
validated on the way in. First half merged as #611; docs/plan/43 has the rest.

**#78 — confirming documented wire assumptions against a real managed-agents endpoint.**
Four recording days, 1,301 request/response pairs for about US$1.12: bytes observed
once, at cost, in a private archive. 2026-09-02 covered the Work API, turn semantics,
multiagent threads, memory stores, files, skills, vaults, deployments and permission
gating; 2026-09-03 ran two waves, one free and one of 281 pairs; 2026-09-04 settled the
skills GA wire shape; 2026-09-05 added #594's batch and #56's, the latter the first
**multi-workspace** capture. #78 stays open — 117 registry entries name it as their live
tracker, and the debt is now analysis rather than recording. Plan 38 (#263) archived
2026-09-04; plans 39 (#566, skills GA) and 40 (#353, environment packages,
folding in #576) 2026-09-05.

## Tasks

- [x] Reconcile 2026-09-02 into the registry (#540, #541, #545 to CONFIRMED, four
      registered readings refuted, ten mismatches now issues #539, #542-#544,
      #546-#550, #553) and 2026-09-03's second wave (24 entries touched, thirteen
      mismatches now #570-#574, #577-#579, #581-#582, #589-#591, `list_cost` on #432)
- [x] Record the multi-workspace capture plan 42 (#56) gated slice 1 on — items 1-5
      in full, item 6 on its key lane; the gate lifts, its environment-key half unobserved
- [x] Plan 43: `CanonicalHost` and `CanonicalEntry`, the six call sites, the three
      `allowed_hosts` write paths, the canonical CONNECT dial
- [ ] Plan 43: verifier, dual review, PR, CI green, squash merge
- [ ] Read the 72 un-analysed comparison rows from 2026-09-02
