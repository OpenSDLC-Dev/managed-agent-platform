# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Cutting v0.5.0** ([docs/RELEASING.md](./docs/RELEASING.md)). The fragments it folds carry
`Added` and `Changed` groups, so the policy makes the bump minor; the platform version, the
chart `version` and its `appVersion` move together. Review of a first cut found its migration
notes stale and incomplete. A folded section is frozen, so #874 corrected the fragments on
`main` and the cut was made again from there.

## Tasks

- [x] Fragments corrected (#874) — #643's entry no longer quotes a retry time true only before
      0045, and one entry states what upgrading from v0.4.0 runs in one transaction
- [x] Release PR — section assembled byte-for-byte from every pending fragment, Chart.yaml at
      0.5.0, both READMEs' version, and no citation the cut would have left dangling
- [ ] Tag the squash-merge commit `v0.5.0` and push it — `release.yml` publishes the versioned
      images, the chart and the worker binaries
- [ ] `make changelog-archive VERSION=0.5.0`, in the next docs PR once the release run is green
