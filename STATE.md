# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Closing out what the archive-endings cluster left open.** #713, #710, #574, #716 and
#730 landed; #731 settles the `rescheduling` child gaps #730's review found. #720 is the second
half of a two-phase migration — the CHECK it wants would refuse the archive writes of any
replica still running the release before #713, so it needs a release between the two, which
is the release this work also cuts.

**Binding to the SDK by symbol** ([plan 51](./docs/plan/51_sdk-reference-binding.md), #722), so a
pin bump touches only what the reference changed: `tools/sdkref` keeps a citation's temporal
claim apart from its locator. The corpus it governs — the registry and the Go comments — is
migrated; it fails nothing until slice 4 turns failing on.

## Tasks

- [x] #716 — the dream closing arm re-reads its session under the row lock before archiving
- [x] #730 — the thread and session archive rules recorded together; both halves pinned by tests
- [x] #731 — an archive ending a retrying child counts its fold move; a wait stays off such a child
- [ ] cut the release that carries #713, #710, #574, #716, #730 and #731 ([docs/RELEASING.md](./docs/RELEASING.md))
- [ ] #720 — once that release is out: a second one-shot clear, then the primary-unarchived CHECK
- [x] Plan 51 slice 1 — `tools/sdkref`'s three rungs, and `make sdk-bump-report` as their front end
- [x] Plan 51 slice 2 — `docs/DIVERGENCES.md`'s citations migrated to the grammar; closes #660
- [x] Plan 51 slice 3 — the Go comments' citations migrated to the grammar
- [ ] Plan 51 slice 4 — rungs 1 and 2 fail, the `go.mod`-triggered workflow lands, and the bump
      ritual is written into `docs/REFERENCE_PROJECTS.md`
