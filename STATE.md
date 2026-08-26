# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

The three plan-36 leftovers that need no decision from anyone, in order: the test
rows slice 4 deferred (#488), plan 36's recording checklist migrated into the issue
that now tracks its inferences (#78), and memory version retention (#476, where the
count the reference leaves unstated is chosen as the newest 5).

## Tasks

- [x] #488 — a meter-reading test for the four memory instruments, and the whole
      memory path through a real Docker container the agent does not own: an
      unprivileged `>>` onto a root-materialized file, which is the first row to
      show why that file has to be 0666 rather than pin the constant's value
- [x] #78 — plan 36's fifteen recording items are in the issue body now, as plan
      35's were when that plan archived; the issue stays open, as the tracker for
      the twenty registry entries that cite those items
- [ ] #476 — prune `memory_versions` older than 30 days that are not among their
      memory's newest 5, never a live head, never a deleted memory's lineage
