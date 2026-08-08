# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**[Plan 28 — changelog and history slimming](./docs/plan/28_changelog-slimming.md)**
(`in-progress`): released CHANGELOG sections archive per release behind index
stubs; HISTORY splits by period.

## Tasks

- [x] Slice 1 — `archive` subcommand (round-trip-guarded, link-rebasing,
      mutation-checked), `make changelog-archive`, CHANGELOG split for
      0.2.0/0.1.0, governance wording, RELEASING.md post-release step
      (this PR).
- [ ] Slice 2 — HISTORY.md split to docs/history/ by period; archive the plan.
