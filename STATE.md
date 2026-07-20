# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[docs/plan/05_sdk-bump-1.58.0.md](./docs/plan/05_sdk-bump-1.58.0.md) — bump `anthropic-sdk-go`
v1.56.0 → v1.58.0 ([#120](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/120)).
A wire-schema event, not just a version number: the pinned SDK is the authoritative typed wire
schema. Verification complete, no drift found; awaiting review and CI.

## Tasks

- [x] Diff the two versions and answer the issue's three questions — no mirrored shape moved, no new
  stop reason or event type, no DIVERGENCES entry warranted. Record: docs/HISTORY.md §
  "anthropic-sdk-go v1.58.0 bump (#120)".
- [x] Move the pin; confirm no code change is required. `make verify` green, total statement
  coverage 91.92%.
- [x] Update the three live pinned-version labels and re-read every `file:line` the Stop Work
  divergence cites at v1.58.0 — all still hold.
- [ ] Land the PR: dual review complete (verifier PASS; Codex and Claude-side findings applied),
  CI green, squash merge.
- [ ] Follow-up PR: archive plan 05 and clear this file, as #125 did for plan 04.
