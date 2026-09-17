# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Closing out what the archive-endings cluster left open, by cutting v0.4.0**
([docs/RELEASING.md](./docs/RELEASING.md)). #713, #710, #574, #716, #730 and #731 landed. #720
is the second half of a two-phase migration — the CHECK it wants would refuse the archive
writes of any replica still running the release before #713, so it needs a release between
the two, and this is that release. The fragments it folds carried `Added` and `Changed`
groups, so the policy makes the bump minor; the platform version, the chart
`version` and its `appVersion` move together.

## Tasks

- [x] #716 — the dream closing arm re-reads its session under the row lock before archiving
- [x] #730 — the thread and session archive rules recorded together; both halves pinned by tests
- [x] #731 — an archive ending a retrying child counts its fold move; a wait stays off such a child
- [x] Release PR — section assembled byte-for-byte from every pending fragment, Chart.yaml at
      0.4.0, both READMEs' version, and the citations the cut would have left dangling re-pointed
- [ ] Tag the squash-merge commit `v0.4.0` and push it — `release.yml` publishes the images,
      the chart and the worker binaries, and nothing before the tag publishes anything
- [ ] `make changelog-archive VERSION=0.4.0`, in the next docs PR once the release run is green
- [ ] #720 — once v0.4.0 is out: a second one-shot clear, then the primary-unarchived CHECK
