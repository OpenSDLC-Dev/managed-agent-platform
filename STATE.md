# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#73](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/73) — reject U+0000 in metadata
with a 400 across every metadata-accepting endpoint (it is currently a Postgres-level 500, and on
three endpoints a silent 200). Plan-less: single-PR scope, no new wire shape.

## Tasks

- [x] Reproduce — a NUL in a metadata key or value 500s on agent/environment/session create and
      update and on the work-item patch (`SQLSTATE 22P05` on the `jsonb` bind, `22021` on the work
      patch's `text[]` delete-key bind), and a NUL *delete* key returns 200 on the three Go-side
      merges. Evidence: `TestMetadataRejectsNUL` failing 15/15 before the fix.
- [x] Hoist the guard into `parseMetadata` and `splitMetadataPatch` (`internal/api/wire.go`), keys
      and values alike, so no endpoint can drift from its peers.
- [x] Pin it with a shared sweep across all seven surfaces — `TestMetadataRejectsNUL` in
      `internal/api/edge_test.go`, green.
- [x] CHANGELOG entry.
- [ ] Verifier, dual review, PR green, squash merge.
