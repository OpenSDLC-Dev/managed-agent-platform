# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Plan 39 — Skills converged onto the GA wire shape** ([docs/plan/39_skills-ga-shape.md](./docs/plan/39_skills-ga-shape.md), #566).
The reference's 2026-08-27 migration renamed the Skills wire fields and changed how a
version is addressed; this platform still served the pre-migration shape, so no released
SDK could create a skill here in either direction. Design is pinned to a 2026-09-04
recording of the live endpoint (163 entries, US$0), kept in the private
`managed-agents-wire-recordings` repository.

## Tasks

- [x] Plan written; ground truth recorded and verified against the raw bytes.
- [x] SDK pin to v1.70.1 and the BYOC worker's id-based resolution (slice 1).
- [x] Wire shapes, create form, list ceiling, delete cascade and the only-version
      refusal in `internal/api` (slices 2-3).
- [x] Three-way version resolution in brain and executor; `skver_` minted with
      `skillver_` accepted on input; CLAUDE.md's prefix list (slice 4).
- [x] `docs/DIVERGENCES.md` entries and the `changelog.d/` fragments. README
      needs none: it describes no wire shape.
- [x] `make verify` green over 57 packages, coverage gate held. No figure is
      recorded here: the container-backed suites move the third digit run to run.
- [x] Section 6 acceptance — the `ant` CLI and the SDK clients, on current
      releases with no `anthropic==0.97.0` pin. Recorded in docs/HISTORY.md.
- [x] Dual review (Codex, Claude) and the verifier: PASS with findings, no blockers.
- [ ] The review fix set, then re-verify, then the PR.
