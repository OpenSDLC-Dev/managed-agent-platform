# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#55](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/55) — Files API, per [docs/plan/08_files.md](./docs/plan/08_files.md) (in-progress). The Files half only; git/repo mounting stays deferred on #55.

## Tasks

- [x] **Slice 1 — the `/v1/files` registry** (upload/list/get/download/delete + migration `0008`). — [PR #156](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/156).
- [ ] Slice 2 — session `resources[]` (`type: "file"`) + the five `sesrsc_` sub-endpoints.
- [ ] Slice 3 — executor materialization + streaming sandbox write + brain injection + `file-answer` eval.
- [ ] Slice 4 — BYOC worker + session-scoped env-key content lane; archive.
