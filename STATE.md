# STATE.md — Current State

The session-resumption file: what this project is right now, and where everything else
lives. **Size budget: ~60 lines.** A change's narrative is written once, in
[CHANGELOG.md](./CHANGELOG.md); the backlog lives in GitHub issues — never grow this
file with either. The verifier enforces both on its docs-consistency rung.

## Snapshot

- **Last updated:** 2026-07-18
- **v1 is complete** — all delivery slices (0–9) landed and verified. The as-built system
  is [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md); the slice-8 acceptance (a real
  `ant beta:worker` end to end) ran and passed (docs/HISTORY.md).
- **Current focus:** the docs restructure (plan 03, below). The **eval test system** ([#30](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/30)
  phase 1) is complete — ten regression tasks **10/10 green** live via `make eval`
  (plan 02 archived; follow-ups #96/#99/phase-1.5 filed). Next: environment-key issuance
  ([#43](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/43)) and published
  images + a helm-install acceptance ([#75](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/75)).
- **Release:** **v0.1.0** (2026-07-17) — the complete v1 loop, tagged at the release PR's
  squash-merge; new work accumulates under CHANGELOG's `[Unreleased]`.

## Active plan

**[docs/plan/03_docs-restructure.md](./docs/plan/03_docs-restructure.md)** — docs
restructure: ARCHITECTURE.md + HISTORY slimming (PR A, this one), STATE.md as a pure
work tracker (PR B), issue-triage subagent (PR C).

- [x] PR A — docs/ARCHITECTURE.md created; HISTORY.md slimmed (530→199 lines) under the
  one-writer rule, written into CLAUDE.md/AGENTS.md/CHANGELOG+HISTORY headers/verifier rung 5.
- [ ] PR B — STATE.md reduced to Active work + Tasks (~30 lines); verifier STATE checks updated.
- [ ] PR C — `.claude/agents/issue-triage.md` (Sonnet 5, JSON-only triage verdict) + CLAUDE.md trigger rule.

## Where things live

- **Backlog & open questions:** [GitHub issues](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues)
  — the only backlog. Post-v1 deferrals are #50–#57 (+ #77); do not build ahead of them.
  Wire assumptions awaiting a real managed-agents recording are cross-linked from
  docs/DIVERGENCES.md's INFERRED section (#27, #58–#61, #63, #67, #78).
- **Completed-work record:** [CHANGELOG.md](./CHANGELOG.md) — the one narrative per change —
  plus [docs/HISTORY.md](./docs/HISTORY.md) for acceptance runs, rejected decisions, and
  archived-plan summaries. The as-built system: [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md).
- **Plans:** [docs/plan/](./docs/plan/) — one file per plan (`NN_short-name.md`), status in
  its frontmatter; conventions in CLAUDE.md → "Plans, state, and backlog". The v1 design
  rationale is [01_v1-managed-agent-platform.md](./docs/plan/01_v1-managed-agent-platform.md) (archived).
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
