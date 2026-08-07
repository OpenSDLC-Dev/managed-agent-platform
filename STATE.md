# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**[Plan 27 — release management](./docs/plan/27_release-management.md)** (`in-progress`):
SemVer 0.x, plan-archive-driven releases, changelog fragments, and a tag-triggered
publishing pipeline (GHCR images, OCI Helm chart, worker binaries, GitHub Release),
ending with v0.2.0 cut as the acceptance run.

## Tasks

- [x] Slice 1 — fragment mechanism: `changelog.d/` + `tools/changelog`
      (mutation-checked tests), `make changelog`/`changelog-notes`,
      docs/RELEASING.md, governance rewording (PR #332).
- [x] Slice 2 — version embedding: `internal/version`, Dockerfile ARG + ldflags,
      startup-log version attribute on all five binaries, worker `--version`
      (this PR; the Make-side ldflags land with their consumer in slice 3).
- [ ] Slice 3 — publishing pipeline: `release.yml` + release Make targets +
      deploy-doc updates.
- [ ] Slice 4 — cut v0.2.0 (acceptance: kind install from published artifacts);
      archive the plan.
