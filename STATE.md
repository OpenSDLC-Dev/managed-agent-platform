# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Cutting v0.6.0** ([docs/RELEASING.md](./docs/RELEASING.md)). The pending changes include
new features and changed behavior, so this is a minor release.

## Tasks

- [x] Assemble the release section and bump the platform and Helm chart versions.
- [ ] After the release PR passes review and CI, squash-merge it and push the annotated `v0.6.0` tag.
- [ ] Archive the release section with `make changelog-archive VERSION=0.6.0` after the release workflow passes.
