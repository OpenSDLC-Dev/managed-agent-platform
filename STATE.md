# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**#78 — confirming documented wire assumptions against a real managed-agents endpoint.**
Four recording days to 2026-09-05, 1,301 request/response pairs for about US$1.12,
observed once at cost in a private archive; the last added #594's batch and #56's,
the first **multi-workspace** capture. #78 stays open — 117 registry entries name it
as their live tracker, and the debt is now analysis rather than recording. Plans
archived: 38 (#263) 2026-09-04; 39 (#566, skills GA) and 40 (#353, environment
packages, folding in #576) 2026-09-05; 43 (#609, one host comparison) and 44 (#601,
one resolution per dial) 2026-09-06.

## Tasks

- [x] Reconcile 2026-09-02 into the registry (#540, #541, #545 to CONFIRMED, four
      registered readings refuted, ten mismatches now issues #539, #542-#544,
      #546-#550, #553) and 2026-09-03's second wave (24 entries touched, thirteen
      mismatches now #570-#574, #577-#579, #581-#582, #589-#591, `list_cost` on #432)
- [x] Record the multi-workspace capture plan 42 (#56) gated slice 1 on — items 1-5
      in full, item 6 on its key lane; the gate lifts, its environment-key half unobserved
- [ ] Read the 72 un-analysed comparison rows from 2026-09-02
