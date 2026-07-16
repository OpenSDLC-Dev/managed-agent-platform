# AGENTS.md

Instructions for AI coding agents and reviewers (Codex, CodeRabbit, and
similar) working in this repository.

**[CLAUDE.md](./CLAUDE.md) is the canonical contributor guide** — the
architecture, the non-negotiable design principles, and the working
conventions all live there. Read it before making or reviewing changes. The
points below are the ones most often violated by tools that skip it:

- **Never guess the wire schema.** The REST API is wire-compatible with
  Anthropic Claude Managed Agents. Field names, enum values, paths, ID
  prefixes, pagination and error envelopes come from the reference SDK
  (`anthropic-sdk-go` — see [docs/REFERENCE_PROJECTS.md](./docs/REFERENCE_PROJECTS.md));
  deliberate divergences and unconfirmed inferences are recorded in
  [docs/DIVERGENCES.md](./docs/DIVERGENCES.md). A wire shape without a
  source is a defect.
- **Never commit to `main`.** Every change goes branch → PR → CI green →
  squash merge.
- **Checks that must pass:** `go build ./...`, `GOOS=linux GOARCH=arm go
  build ./internal/...`, `go vet ./...`, `gofmt -l .` (empty), `go test
  -count=1 ./...`, and total statement coverage **≥ 90%** over
  `./internal/...`. The store and API tests start their own Postgres in
  Docker and hard-fail without it.
- **Docs move with code, in the same PR:** STATE.md's snapshot updated
  (within its size budget — completed-work narrative goes to
  docs/HISTORY.md, the backlog to GitHub issues), a CHANGELOG.md entry for
  every notable change, a docs/DIVERGENCES.md entry for any new wire
  divergence, README.md when affected.
- **Migrations are immutable once merged**; new DDL goes in a new numbered
  file under `internal/store/migrations/`.
- **`internal/domain` stays stdlib-only** and Anthropic-native — no adk-go,
  genai, or provider SDK imports there.
