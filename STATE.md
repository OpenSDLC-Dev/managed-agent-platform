# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#135](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/135) — a NUL (or any
unstorable byte) in a path ID or query parameter reached Postgres as a bind parameter and failed
`SQLSTATE 22021` → 500, the surface #114 left open. Fix is ID-format validation
(`domain.ID.Valid`) at every path id / `agent_id` / `page` cursor id, plus a storable-byte guard
on the free-form `types[]` filter. No plan file: single-PR scope (issue-triage `needs_plan:false`).

## Tasks

- [x] `domain.ID.Valid` (known prefix + Crockford-base32 token); `TestIDValid` pins it.
- [x] `checkID`/`checkWorkID` → 404 on every path id (agents/env/sessions/events handlers, work
      `{work_id}`, the Bearer session-read auth lane); work `{work_id}` checked after body validation
      so the `POST …/work/poll` empty-body 400 is preserved.
- [x] `agent_id` and the `page` cursor's decoded id → 400; `types[]` rejects only U+0000 / invalid
      UTF-8 (an unknown-but-storable type still filters to empty).
- [x] `TestPathAndQueryRejectNUL` sweeps every path/query surface (`%00` and `%80`): red on pre-fix
      code (500s; the work heartbeat a 412), green after. docs/DIVERGENCES.md + CHANGELOG.md updated.
- [x] `make verify` green: build + crossbuild + vet + fmt-check + full test suite, total coverage
      91.81% (≥90% gate).
- [x] Verifier (pinned) PASS; two independent Claude reviewers, no blockers (Codex unavailable —
      account usage limit, resets 2026-07-25; second Claude reviewer stood in, per PR #133).
- [ ] PR opened, CI green, threads settled, squash-merge.
