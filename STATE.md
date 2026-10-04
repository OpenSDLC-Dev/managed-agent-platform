# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Cutting v0.5.1** ([docs/RELEASING.md](./docs/RELEASING.md)). Every fragment it folds is a
`Fixed` entry, so the policy makes the bump a patch; the platform version, the chart `version`
and its `appVersion` move together.

## Tasks

- [x] Release PR — section assembled byte-for-byte from every pending fragment, Chart.yaml at
      0.5.1, both READMEs' version, and no citation the cut would have left dangling
- [ ] Tag the squash-merge commit `v0.5.1` and push it — `release.yml` publishes the versioned
      images, the chart and the worker binaries
- [ ] `make changelog-archive VERSION=0.5.1`, in the next docs PR once the release run is green
