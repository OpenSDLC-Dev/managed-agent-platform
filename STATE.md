# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Cutting v0.3.0** ([docs/RELEASING.md](./docs/RELEASING.md)). The 54 pending `changelog.d/`
fragments — plans 28–34, the GCP delivery lane, and a run of issue fixes — carry `Added`
and `Changed` groups, so the policy makes the bump minor. The one release train moves the
platform version, the chart `version` and its `appVersion` together.

Tasks:
- [x] Release PR (#421, merged) — section assembled (byte-for-byte from all 54 fragments),
      Chart.yaml at 0.3.0, README status line, and every dangling citation re-pointed
- [x] `changelog-notes` links absolutely (#423), so the Release body does not ship 30 targets
      that 404 from the release page. It lands before the tag, which therefore names main's
      tip rather than #421's merge commit ([docs/RELEASING.md](./docs/RELEASING.md) step 8)
- [ ] Tag that commit `v0.3.0` and push it — `release.yml` publishes the images, the chart
      and the worker binaries, and nothing before the tag publishes anything
- [ ] `make changelog-archive VERSION=0.3.0`, in the next docs PR once the release run is green

[Plan 34](./docs/plan/34_doc-trim.md) (#413) archived 2026-08-16; its delivery record is
[docs/HISTORY.md](./docs/HISTORY.md). Nothing else is in flight — the backlog is GitHub issues.
