# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). No versions have
been released yet, so everything sits under **Unreleased**; entries are
grouped newest-first by the PR that landed them.

A change and its changelog entry land in the **same PR** — see CLAUDE.md →
"Iteration workflow".

## [Unreleased]

### Added

- `internal/api` + `cmd/controlplane` — wire-compatible control-plane CRUD
  (slice 2): agents (optimistic `version` in the POST-update body, mismatch →
  409; immutable version snapshots; pinned `?version=` reads; archive),
  environments (config union normalization, update/archive/delete), sessions
  (agent-union resolution into a full `resolved_agent` snapshot,
  `session_`/`sesn_` prefix equivalence, bidirectional list cursors,
  archive/delete) — all under `x-api-key` auth with the reference error
  envelope, keyset cursor pagination (stable under concurrent writes), and
  UTC timestamps. Session `archived_at` added by migration `0002`. Review
  hardening in the same PR: bootstrap-key rotation revokes the previous key,
  HTTP server slow-client timeouts, environment config updates merge instead
  of resetting omitted sub-fields, archived resources are read-only,
  transactional session creation, strict unknown-field validation, 413 on
  oversize bodies, and per-request OTel server spans continuing inbound
  `traceparent`. Verified end-to-end by driving the real `ant` CLI (v1.16.0)
  against `cmd/controlplane`. Deliberate v1 divergences are rejected with
  clear errors (multiagent, session resources, non-empty vault_ids on
  create, `scope:"account"`). (#7)
- Docs-consistency rule in the iteration workflow: STATE.md, README.md, and
  CHANGELOG.md move with the code in the same PR, and the verifier checks
  them as a dedicated rung. CHANGELOG.md introduced and backfilled;
  README's roadmap checkboxes replaced by pointers to STATE.md and
  CHANGELOG.md so per-slice progress lives in one place. (#6)
- `internal/store` — Postgres schema + embedded migrations (slice 1):
  `agents`/`agent_versions`, `environments` (kind ⇄ config-discriminator
  agreement CHECK), `sessions` (composite FK onto immutable agent-version
  snapshots, no `user_id` by design), append-only `events` with
  `UNIQUE (session_id, seq)`, `work_items`, `api_keys`/`environment_keys`;
  single-transaction advisory-locked migrator; `Open` = pool + ping +
  migrate; contract tests against a real Dockerized Postgres. CI now also
  cross-compiles `GOOS=linux GOARCH=arm` to protect the 32-bit BYOC worker
  build. (#5)
- `internal/telemetry` — OTel foundation (completes slice 0): tracer/meter
  init with OTLP/gRPC export, configurable sampling, offline no-op without a
  collector endpoint, W3C `traceparent`/`tracestate` `Inject`/`Extract` over
  string-map carriers (HTTP headers, work items). (#4)
- CI coverage gate: total statement coverage ≥ 90% over `./internal/...`,
  computed exactly from the coverage profile. (#3)
- Dual code review (Codex + Claude, one pass each) in the iteration
  workflow. (#2)
- CI pipeline (build / vet / gofmt / `test -count=1`), the
  branch → review → PR → CI → squash-merge workflow, the independent
  `verifier` subagent, and the local reference checkouts documented as
  wire-schema ground truth. (#1)
- STATE.md: cross-session delivery progress tracking.
- Project foundation: Apache-2.0 license, README, CLAUDE.md, and
  `internal/domain` — Anthropic-native core types (prefixed IDs, the full
  `{domain}.{action}` event taxonomy, session status machine,
  agent/environment resources).

### Changed

- Module path set to the canonical GitHub owner,
  `github.com/OpenSDLC-Dev/managed-agent-platform`.
