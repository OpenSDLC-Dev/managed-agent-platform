# STATE.md — Current State

The session-resumption file: what this project is right now, and where everything else
lives. **Size budget: ~60 lines.** Completed-work narrative moves to
[docs/HISTORY.md](./docs/HISTORY.md) in the same PR that completes the work; the backlog
lives in [GitHub issues](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues),
never here. The verifier enforces both on its docs-consistency rung.

## Snapshot

- **Last updated:** 2026-07-17
- **v1 is complete.** All delivery slices (0–9) landed and verified: wire-compatible
  control-plane CRUD, append-only event log + SSE streaming, config-driven model providers
  (Anthropic-protocol and OpenAI-compatible), the brain orchestration loop, executor +
  Docker/K8s sandboxes running the built-in toolset, permission policies with the
  `user.tool_confirmation` round-trip, the wire-compatible work API + BYOC worker
  (dead-worker reclaim, one OTel trace across the process boundary), a Helm chart, and a
  local docker-compose stack. The slice-8 acceptance — a real `ant beta:worker` end to
  end — has been run and passed (see docs/HISTORY.md).
- **Current focus:** an operator surface to issue environment keys
  ([#43](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/43)) — the first of
  the two gaps before a self-hosted deploy is turn-key (the other: published container
  images + a real helm-install acceptance, [#75](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/75)).
- **Release:** **v0.1.0** (2026-07-17), tagged on main — the complete v1 loop. New work
  accumulates under CHANGELOG's `[Unreleased]`.

## Where things live

- **Backlog & open questions:** [GitHub issues](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues)
  — the only backlog. Post-v1 deferrals are #50–#57 (+ #77); do not build ahead of them.
  Wire assumptions awaiting a real managed-agents recording are cross-linked from
  docs/DIVERGENCES.md's INFERRED section (#27, #58–#61, #63, #67, #78).
- **Completed-work archive:** [docs/HISTORY.md](./docs/HISTORY.md) — the full per-slice
  narrative (moved verbatim from this file), alongside [CHANGELOG.md](./CHANGELOG.md).
- **Wire divergences & inferences:** [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) — the
  single registry; the verifier's wire-compat allowlist.
- **Reference projects:** [docs/REFERENCE_PROJECTS.md](./docs/REFERENCE_PROJECTS.md) —
  the read-only checkouts (URLs, relative paths, authority order).
- **Design plan (historical):** `~/.claude/plans/agent-managed-agent-encapsulated-moonbeam.md`
  — mostly implemented; its open remnants are tracked as issues.
- **Conventions & workflow:** [CLAUDE.md](./CLAUDE.md) (canonical), [AGENTS.md](./AGENTS.md)
  (for external AI reviewers).

## Environment notes

- **Go 1.26** (Homebrew). **Docker** available; **`psql` is not** — use the Postgres
  container (the store/API tests start their own `postgres:16-alpine`; a missing daemon is
  a hard test failure, not a skip). The K8s sandbox contract test needs a cluster — a local
  [kind](https://kind.sigs.k8s.io) cluster works; CI provisions one.
- **`ant` CLI:** no binary installed — build it from the read-only checkout (path in
  docs/REFERENCE_PROJECTS.md): `go build -o <scratch>/ant ./cmd/ant`. Management commands
  ignore `ANTHROPIC_BASE_URL` — pass `--base-url http://127.0.0.1:<port>` explicitly (only
  the worker/auth subcommands honor the env var).
- **Module path** `github.com/OpenSDLC-Dev/managed-agent-platform` — the owner's mixed
  case is intentional and must match the GitHub owner exactly (Go escapes the uppercase
  letters in the module cache).
