# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 53](docs/plan/53_model-effort.md): honor explicit model effort (#160).

## Tasks

- [ ] Implement wire parsing, update semantics and provider propagation.
- [ ] Verify regression tests, runtime behavior and repository gates.
- [ ] Complete independent verification, reviews and PR CI.
