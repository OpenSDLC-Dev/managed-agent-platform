# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Plan 75** ([docs/plan/75_verification-hardening.md](./docs/plan/75_verification-hardening.md)) — verification and testing hardening (8 slices).

## Tasks

- [x] Slice 1: `pgtest` template DB cloning, `dockertest` dead-owner reaping (#499), and `make test-fast` (PR #1075)
- [x] Slice 2: `.claude/skills/verify`, 3-round repair cap, and `main` branch protection alignment (PR #1077)
- [x] Slice 3: Property tests, `Fuzz*` targets, and scheduled fuzz workflow (PR #1078)
- [x] Slice 4: `make test-race`, CI `race` job, and test-hygiene guard (PR #1079)
- [ ] Slice 5: `REVIEW.md` diff-triggered reliability checklist and base-branch rule isolation (in progress)
- [ ] Slice 6: Full-topology offline E2E and wire differential test suite
- [ ] Slice 7: Verification stamping and `off`/`observe`/`enforce` commit gate hook
- [ ] Slice 8: `tools/revieweval` meta-evaluation framework, pruning policy, and plan archival
