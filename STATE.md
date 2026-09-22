# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 54](docs/plan/54_inference-geo-wire.md): inference_geo protocol compatibility (#433).

## Tasks

- [ ] Preserve and validate the wire field, including update and snapshot semantics.
- [ ] Verify API persistence, SDK decoding and fixed-route runtime behavior.
- [ ] Complete independent verification, reviews and PR CI.
