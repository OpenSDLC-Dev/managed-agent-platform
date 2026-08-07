# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**None.** [Plan 25 — git/repo mounting](./docs/plan/25_git-repo-mounting.md) archived
with its second slice; #55 is closed. The one deliberate gap it leaves, BYOC repository
materialization, is tracked in #322 and is not started.

## Tasks

- [x] Slice 1 — the wire + sealed token storage: create acceptance/validation,
      migration 0020, rotation live, delete rejection, unit W tests + mutation
      evidence (PR #329).
- [x] Slice 2 — the clone: go-git executor materialization, `session.error`
      surfacing, the brain's "Mounted repositories" block, the `repo-answer`
      eval; plan archived, #55 closed (this PR).
