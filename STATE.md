# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#114](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/114) — reject U+0000 in the
non-metadata text fields with a 400 (it is currently a Postgres-level 500), the same bug class #73
closed for `metadata`. Plan-less: single-PR scope, no new wire shape.

## Tasks

- [x] Reproduce — 17 field-and-endpoint pairs 500 against real Postgres (`SQLSTATE 22021` text
      binds, `22P05` jsonb binds), the issue's table plus the nested `tools`/`mcp_servers`/`skills`
      and `allowed_hosts` cases. Evidence: `TestStringFieldsRejectNUL` failing 17/17 pre-fix.
- [x] Move the guard to `decodeObject` (`internal/api/wire.go`) as a walk over the whole decoded
      body, naming the offending path — a per-field check on `stringField`/`requiredString` would
      miss the nested raw-JSON payloads. `rejectMetadataNUL` is unreachable under it, so it goes.
- [x] Pin it with `TestStringFieldsRejectNUL` (`internal/api/edge_test.go`), with
      `TestMetadataRejectsNUL` still green over its fifteen metadata surfaces.
- [x] CHANGELOG entry; widen the existing docs/DIVERGENCES.md INFERRED entry — the registered
      behaviour is unchanged, only its scope.
- [x] Verifier PASS; review findings applied — the second decode now uses `UseNumber` (a plain
      `any` decode rejected out-of-range literals like `1e400`, turning a working request into a
      400; pinned by `TestNULGuardKeepsOutOfRangeNumbers`), the walk is gated on the raw escape,
      and the "every JSON body" wording is scoped to bodies that are JSON objects. Path/query NUL
      is the same class on a surface with no body decode — filed as #135.
- [ ] PR green, threads settled, squash merge. Codex reviewer unavailable (quota exhausted).
