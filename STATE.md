# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**[docs/plan/21_outcomes.md](./docs/plan/21_outcomes.md) (in-progress)** — the session
outcomes surface (#77, absorbing #161), started 2026-08-02.
**[docs/plan/20_gcp-deployment.md](./docs/plan/20_gcp-deployment.md) (in-progress)** —
GCP deployment; slices 1-3 + 4a landed, 4b blocked on GCP spend approval.

## Tasks — plan 21

- [x] Slice 1 — SDK bump v1.59.0→v1.61.0 + verification record (this PR; gate green
  at 90.80% on the bump)
- [ ] Slice 2 — define_outcome acceptance + storage + rendering + initial_events
- [ ] Slice 3 — brain grader loop (transcript-stage)
- [ ] Slice 4 — outputs_harvest work kind + deliverables
- [ ] Slice 5 — full-chain acceptance (doc example on latest SDK) + settlement

## Tasks — plan 20 (remaining)

- [ ] Slice 4b — apply, deploy, mode-1 acceptance battery (needs GCP spend approval)
- [ ] Slice 5 — mode-2 acceptance + `docs/deploy-gcp.md`
