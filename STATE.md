# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#114](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/114) — reject U+0000 in the
non-metadata text fields with a 400 (it is currently a Postgres-level 500), the same bug class #73
closed for `metadata`. Plan-less: single-PR scope, no new wire shape.

## Tasks

- [x] Reproduce — 17 field-and-endpoint pairs 500 against real Postgres (`SQLSTATE 22021` on the
      `text` binds, `22P05` on the `jsonb` binds), covering the issue's table plus the nested
      `tools`/`mcp_servers`/`skills` and `allowed_hosts` cases. Evidence: `TestStringFieldsRejectNUL`
      failing 17/17 before the fix.
- [x] Move the guard to `decodeObject` (`internal/api/wire.go`) as a walk over the whole decoded
      body, keys and values alike, naming the offending path — a per-field check on
      `stringField`/`requiredString` would miss the nested raw-JSON payloads. `rejectMetadataNUL` is
      unreachable under it and is removed.
- [x] Pin it with `TestStringFieldsRejectNUL` (`internal/api/edge_test.go`), green, with
      `TestMetadataRejectsNUL` still green over its fifteen metadata surfaces.
- [x] CHANGELOG entry; widen the existing docs/DIVERGENCES.md INFERRED entry — the registered
      behaviour is unchanged, only its scope. `make verify` green, coverage 91.89%.
- [ ] Verifier PASS, dual review, PR green, threads settled, squash merge.
