# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**[#703](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/703) — the last object
deletes that orphaned on a store refusal.** No plan file: the mechanism is
[plan 50](./docs/plan/50_object-delete-retry.md)'s, archived, and this replicates it across the
call sites it did not reach. #693 is folded in here.

## Tasks

- [x] The six call sites enqueue on their deleting transaction: `deleteFile`, the dream close
      and settle, the skill and skill-version deletes, and the harvest's snapshot replacement
- [x] The never-committed rollbacks keep the best-effort delete, under helpers renamed to say
      so — `discardUncommittedObject` and `discardUncommittedArchive`
- [ ] Verifier, both reviewers, PR, CI, merge
