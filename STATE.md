# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

None.

## Tasks

None in flight. The last delivery was the test-suite hardening the plan-36 gate runs
surfaced — two lease-keeper flakes (#483), the broker's coverage-start wake race (#486),
and the gate's per-package timeout (#490) — all test-environment fixes, no production
code. What #490 did not take, reaping a fixture whose owning process died and cutting
per-test database creation, is #499. Pick the next piece of work from the GitHub issue
backlog.
