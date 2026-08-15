# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 34](./docs/plan/34_doc-trim.md) (#413) — trimming the documentation to what code cannot say.
Tracked markdown is 2,665,735 bytes against 2,364,778 bytes of non-test Go; the target is
~719 KB, cut from restatement and staleness only.

## Tasks

- [x] **Slice 1 — fragments and facts.** The `changelog.d/` size convention (60–120 words,
      1,500-byte cap) and the recut of every fragment over it; the release blocker in
      `cd-build-on-runner.fixed.md`, which `loadFragments` rejects for want of a `- ` prefix;
      the sandbox-pool sizing correction in `docs/deploy-gcp.md`, which told operators nothing
      reaps a pod although `internal/executor/executor.go:311` runs `reapLoop`.
- [ ] Slice 2 — the seven package-comment repairs and the drift-pinning test (touches `.go`).
- [ ] Slice 3 — `docs/ARCHITECTURE.md`: 155 per-file rows and 27 preambles to `go doc` stubs.
- [ ] Slice 4 — deployment docs, `self-hosted-security.md`, and the steering layer.
- [ ] Slice 5 — `DIVERGENCES.md` reshaped; the 32 archived plans compressed to decisions.
- [ ] Slice 6 — `HISTORY.md`, `docs/history/`, `docs/changelog/`.

Plans 29 and 33 archived 2026-08-15; their delivery records are in
[docs/HISTORY.md](./docs/HISTORY.md). The rest of the backlog is GitHub issues.
