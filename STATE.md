# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**[Plan 24 — sandbox teardown](./docs/plan/24_sandbox-teardown.md)** (#64; settles the
workspace-continuity half of #28): the executor-resident reaper, `Owned`/`Reap` on
`sandbox.Provider`, and the blob checkpoint/restore that lets an idle-reaped session
resume with its workspace.

## Tasks

- [x] Slice 1 — wire guards: archive/delete of a `running` session answer 400; plan
      lands in-progress; DIVERGENCES INFERRED entry. Evidence:
      `TestRunningSessionArchiveAndDeleteRejected` (red pre-guard, green post).
- [x] Slice 2 — `Owned`/`Reap` on both backends + Docker list endpoint + Helm RBAC
      `list` + contract-suite rows. Evidence: 4 shared contract rows green on real
      Docker + kind; 5 targeted mutants each killed by a backend suite.
- [x] Slice 3 — the reaper loop, terminal tiers (deleted = tombstoned, archived,
      terminated), advisory lock, metrics, knobs. Evidence: reaper_test.go (13 rows
      incl. foreign-sandbox skip, self-hosted skip, lock-held skip, under-lock recheck,
      blob-before-reap ordering, provision blocking on the lock) + the metric pin.
- [x] Slice 4 — checkpoint/restore engine: `Export` on both backends (+3 contract
      rows), migration 0019 (consumption marker), capture (three roots, sentinel
      strip, validate, budget, spool), restore (marker-gated, replace-first,
      in-sandbox extract, consumed flip). Evidence: checkpoint_test.go (8 rows) +
      the metric pin + the Docker stopped-container export row.
- [ ] Slice 5 — the idle-TTL tier with its exclusions (cloud-only, no pending work, no
      unanswered asks), blob-less disablement, acceptance runs; archives the plan.
