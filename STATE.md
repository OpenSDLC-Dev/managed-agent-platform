# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#69](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/69) — fold the three private
Docker-Postgres test harness copies (`internal/store`, `internal/api`, `internal/events`) into the
shared `internal/pgtest`. Triage: single-PR, no plan file. Test infrastructure only, no behavior
change.

## Tasks

- [x] `internal/pgtest` gains `FreshDB` (bare un-migrated DSN, for the store suite's own
      `Open`/`Migrate` tests); `NewPool` composes it.
- [x] store/api/events wire `TestMain` through `pgtest.Main`; the private copies are deleted.
      events keeps its package-local fixtures (`fixtures_test.go`) — fixture shape, not container
      plumbing, and shaped differently from the shared `NewSession`.
- [x] Affected suites green locally: store/api/events/queue against Docker Postgres,
      `go test -count=1` all ok.
- [ ] `make verify` + verifier subagent, dual review, PR, CI green, squash merge.
