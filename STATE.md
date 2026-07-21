# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#44](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/44) — emit the OTLP business
metrics Component 6 lists that #89 did not: TTFT, cache-token breakdown, session-status counts,
approval (HITL) wait, and queue depth/pending/workers_polling. No plan file: single-PR scope, no
wire-schema surface (issue-triage judged `needs_plan: false`).

## Tasks

- [x] `model.time_to_first_token` (brain), start boundary = work claim; no reading when a turn
      streams no content. TDD, `internal/brain`.
- [x] `model.cache.token.usage` (events) splits cache creation/read alongside the convention's
      merged `gen_ai.client.token.usage`. TDD, `internal/events`.
- [x] `session.status.transitions` (events) counted post-commit at each SetStatus commit site
      (AppendWith, brain settle/commitUnderLock, API send). Tested in events/brain/api.
- [x] `approval.wait.duration` (events, API-driven) measured in-DB from the requires_action idle.
      Tested via the confirmation harness in `internal/api`.
- [x] `queue.depth`/`pending`/`workers_polling` observable gauges over `Queue.Stats`, self_hosted
      only, registered in `cmd/controlplane`. Mutation-checked, `internal/queue`.
- [x] Telemetry contract test asserts every business metric name reaches an OTLP collector.
- [x] `make verify` green (91.67%); verifier PASS; two Claude reviewers, no blockers (Codex over
      quota to 2026-07-25, second Claude reviewer stood in). PR
      [#144](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/144) (draft).
- [ ] CI green, then mark ready and squash-merge.
