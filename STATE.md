# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 35](./docs/plan/35_multiagent-threads.md) (#53) — multi-agent session threads, the
coordinator topology: a coordinator agent's roster runs as concurrent session threads on
one shared sandbox, wire-compatible with the reference's `sthr_` thread resource, routes
and events; coordinator sessions on cloud and `self_hosted` alike, the BYOC worker
thread-unaware. Every scope decision is settled; the design is the plan's fifteen decisions.

## Tasks

- [x] **Slice 0 — SDK bump v1.61.0 → v1.63.1**: pin, pairwise diffs, live labels, registry
      citations re-read, registry entries + issues #430–#433 for the excluded v1.62 surface,
      four v1.63.x toolset/runner behaviors converged, HISTORY record; plan → `in-progress`.
- [x] Slice 1 — roster resolution and the session snapshot (design decision 10):
      `internal/api/roster.go`; inert at runtime until slice 3.
- [x] Slice 2 — thread resource and the primary thread: migration 0025, `sthr_`, five routes,
      primary-thread status events on every session (decisions 1, 2, 12).
- [x] Slice 3 — thread execution substrate: migration 0026, `(session, thread)` turns, the
      status fold, the runnable set, MCP per thread, outcomes on quiescence, interrupts, the
      `tool_exec` re-arm (decisions 3, 4, 5, 9, 14, 15); child rows via a test seam.
- [ ] Slice 4 — coordinator delegation (decisions 6, 7, 8, 11, 13): the six tools answered in
      the commit that emits them, agent-to-agent messages, the `self_hosted` view rule, the
      coordinator-mode worker scan, the roster's skills union. Code, tests and the
      `coordinator-team` eval trial in; the `ant` CLI acceptance is run and recorded, the
      live eval run this slice owes #53 is not.
- [ ] Slice 5 — close-out: HISTORY records, ARCHITECTURE/README, plan → `archived`, #53 closed.
