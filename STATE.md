# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Closing out what the archive-endings cluster left open.** #713, #710 and #574 landed; the
two issues their reviews filed did not. #716 is a defect and lands now. #720 is the second
half of a two-phase migration — the CHECK it wants would refuse the archive writes of any
replica still running the release before #713, so it needs a release between the two, which
is the release this work also cuts.

## Tasks

- [x] #716 — the dream closing arm re-reads its session under the row lock before archiving
- [ ] cut the release that carries #713, #710, #574 and #716 ([docs/RELEASING.md](./docs/RELEASING.md))
- [ ] #720 — once that release is out: a second one-shot clear, then the primary-unarchived CHECK
