# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 65](./docs/plan/65_repo-skill-discovery.md) (#921): skills discovered in a mounted
repository and announced to the agent, as the reference announces them, after both skills
blocks take the reference's `<available_skills>` template (#581).

## Tasks

- [x] Slice 1, the template (#581): the attached block's XML list and absolute cloud locations.
- [ ] Slice 2, root discovery: scan before the first request, the repository block, the trust note.
- [ ] Slice 3, nested discovery on `read`; archives the plan and closes #921.
