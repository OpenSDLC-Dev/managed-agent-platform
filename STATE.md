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
- [ ] Slice 2 — `Owned`/`Reap` on both backends + Docker list endpoint + Helm RBAC
      `list` + contract-suite rows.
- [ ] Slice 3 — the reaper loop, terminal tiers (deleted/archived/terminated), advisory
      lock, metrics, knobs.
- [ ] Slice 4 — checkpoint/restore engine: migration (consumption marker), capture
      (three roots, sentinel strip, validate, spool), restore (in-sandbox extract).
- [ ] Slice 5 — the idle-TTL tier with its exclusions (cloud-only, no pending work, no
      unanswered asks), blob-less disablement, acceptance runs; archives the plan.
