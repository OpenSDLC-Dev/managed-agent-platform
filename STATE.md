# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 34](./docs/plan/34_doc-trim.md) (#413) — trimming the documentation to what code cannot say.
Tracked markdown was 2,665,735 bytes at plan start against 2,364,778 bytes of non-test Go;
the target is ~719 KB, cut from restatement and staleness only.

## Tasks

- [x] Slice 1 — `changelog.d/` 196 KB → 50 KB: a 1,500-byte cap and every fragment recut to it.
- [x] Slice 2 — twelve package comments rewritten from the code; three documented lists pinned by
      offline tests, each of which found real drift on its first run.
- [x] Slice 3 — `docs/ARCHITECTURE.md` 325 KB → 35 KB: the per-file tables were 92% of the file
      and the derivative (nearly every package carries more comment than the document spent on
      it, most 3–10×), so they became a map. The subsystems that span packages — session
      resources, the sandbox lifecycle — moved into Execution flow rather than going with them.
- [ ] Slice 4 — deployment docs, `self-hosted-security.md`, and the steering layer.
- [ ] Slice 5 — `DIVERGENCES.md` reshaped; the 32 archived plans compressed to decisions.
- [ ] Slice 6 — `HISTORY.md`, `docs/history/`, `docs/changelog/`.
