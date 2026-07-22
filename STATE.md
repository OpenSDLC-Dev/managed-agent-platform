# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#55](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/55) — Files API, per [docs/plan/08_files.md](./docs/plan/08_files.md) (in-progress). The Files half only; git/repo mounting stays deferred on #55.

## Tasks

- [x] **Slice 1 — the `/v1/files` registry.** Migration `0008_files.sql`; `api/files.go` + `filesupload.go` + `filesmetrics.go`: upload (multipart, 500 MB cap, filename validation, `downloadable:false`), list (classic `Page` envelope + `after_id`/`before_id`/`limit`/`scope_id`), get-metadata, download (400 gate + streaming path), delete; routes + 405 fallbacks; end-to-end integration test over real Postgres + blob store; structured logging + metrics on every link. — [PR #156](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/156) (draft); `make verify` green (cover 90.68%).
- [ ] Slice 2 — session `resources[]` (`type: "file"`) + the five `sesrsc_` sub-endpoints.
- [ ] Slice 3 — executor materialization + streaming sandbox write + brain injection + `file-answer` eval.
- [ ] Slice 4 — BYOC worker + session-scoped env-key content lane; archive.
