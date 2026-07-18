# STATE.md — Current State

The session-resumption file: what this project is right now, and where everything else
lives. **Size budget: ~60 lines.** Completed-work narrative moves to
[docs/HISTORY.md](./docs/HISTORY.md) in the same PR; the backlog lives in GitHub issues,
never here. The verifier enforces both on its docs-consistency rung.

## Snapshot

- **Last updated:** 2026-07-18
- **v1 is complete.** All delivery slices (0–9) landed and verified: wire-compatible
  control-plane CRUD, append-only event log + SSE streaming, config-driven model providers
  (Anthropic-protocol and OpenAI-compatible), the brain orchestration loop, executor +
  Docker/K8s sandboxes running the built-in toolset, permission policies with the
  `user.tool_confirmation` round-trip, the wire-compatible work API + BYOC worker
  (dead-worker reclaim, one OTel trace across the process boundary), a Helm chart, and a
  local docker-compose stack; the slice-8 acceptance — a real `ant beta:worker` end to
  end — ran and passed (docs/HISTORY.md).
- **Current focus:** none in flight. The **eval test system** ([#30](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/30)
  phase 1) is complete — ten regression tasks **10/10 green** live via `make eval` (plan:
  [docs/plan/02_evals-system.md](./docs/plan/02_evals-system.md), archived; follow-ups #96,
  #99, and phase-1.5 `tool_choice` on #30 are filed, not queued). Next: environment-key
  issuance ([#43](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/43)) and
  published images + a helm-install acceptance ([#75](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/75)).
- **Release:** **v0.1.0** (2026-07-17) — the complete v1 loop, tagged at the release PR's
  squash-merge; new work accumulates under CHANGELOG's `[Unreleased]`.

## Active plan

None. When one is active, this section links its [docs/plan/](./docs/plan/) file and carries
its progress track (updated in every PR that advances it; the plan file itself carries none).

## Where things live

- **Backlog & open questions:** [GitHub issues](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues)
  — the only backlog. Post-v1 deferrals are #50–#57 (+ #77); do not build ahead of them.
  Wire assumptions awaiting a real managed-agents recording are cross-linked from
  docs/DIVERGENCES.md's INFERRED section (#27, #58–#61, #63, #67, #78).
- **Completed-work archive:** [docs/HISTORY.md](./docs/HISTORY.md) — the full per-slice
  narrative (moved verbatim from this file), alongside [CHANGELOG.md](./CHANGELOG.md).
- **Plans:** [docs/plan/](./docs/plan/) — one file per plan (`NN_short-name.md`), status in
  its frontmatter; conventions in CLAUDE.md → "Plans, state, and backlog". The v1 design
  rationale is [01_v1-managed-agent-platform.md](./docs/plan/01_v1-managed-agent-platform.md)
  (archived; mostly implemented, open remnants tracked as issues).
- **Wire divergences & inferences:** [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) — the
  single registry; the verifier's wire-compat allowlist.
- **Reference projects:** [docs/REFERENCE_PROJECTS.md](./docs/REFERENCE_PROJECTS.md) —
  the read-only checkouts (URLs, relative paths, authority order).
- **Conventions & workflow:** [CLAUDE.md](./CLAUDE.md) (canonical), [AGENTS.md](./AGENTS.md)
  (for external AI reviewers).

## Environment notes

- **Go 1.26** (Homebrew). **Docker** available; **`psql` is not** — use the Postgres
  container (the store/API tests start their own `postgres:16-alpine`; a missing daemon is
  a hard test failure, not a skip). The K8s sandbox contract test needs a cluster — a local
  [kind](https://kind.sigs.k8s.io) cluster works; CI provisions one.
- **`ant` CLI:** no binary installed — build from the read-only checkout (path in
  docs/REFERENCE_PROJECTS.md): `go build -o <scratch>/ant ./cmd/ant`. Management commands
  ignore `ANTHROPIC_BASE_URL` — pass `--base-url` explicitly (only worker/auth honor the env var).
- **Module path** `github.com/OpenSDLC-Dev/managed-agent-platform` — the owner's mixed
  case is intentional and must match the GitHub owner exactly (Go escapes the uppercase
  letters in the module cache).
