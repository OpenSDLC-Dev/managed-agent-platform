# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 66](./docs/plan/66_clone-host-failures.md) (#940): a repository clone that the
git host fails partway names the host, in one PR.

## Tasks

- [x] A transport records each clone's transport failures; the fixture's host fails each way.
- [x] The host's in-band messages and go-git's decoders' refusals read `network`.
- [ ] Reviews, then the plan archives in the same PR.
