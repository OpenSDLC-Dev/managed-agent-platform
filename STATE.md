# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Cutting v0.5.0** ([docs/RELEASING.md](./docs/RELEASING.md)). Review of the cut found the
migration notes it would ship stale and incomplete. A folded section is frozen, so the
fragments are corrected on `main` first and the cut is made again from there.

## Tasks

- [x] Fragments corrected — #643's entry no longer quotes a retry time true only before 0045,
      and one entry states what upgrading from v0.4.0 runs in one transaction
- [ ] Release PR — the cut, from `main` with those fragments
- [ ] Tag the squash-merge commit `v0.5.0` and push it — `release.yml` publishes the versioned
      images, the chart and the worker binaries
- [ ] `make changelog-archive VERSION=0.5.0`, in the next docs PR once the release run is green
