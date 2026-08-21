# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

Three issues left open by plan 35's delivery, taken in this order. No plan file — each
is bounded and already scoped in its own issue, which stays the record of that scope.
[Plan 35](./docs/plan/35_multiagent-threads.md) (#53) archived 2026-08-21; its delivery
record is [docs/HISTORY.md](./docs/HISTORY.md).

## Tasks

- [x] **#431** — the advisor roster entry: its refusal recorded as settled and the
      registry corrected around it (#444).
- [ ] **#442** — the four settlement-chain follow-ups the slice-4 review deferred. All
      four implemented and green locally; PR open.
- [ ] **#441** — the BYOC worker's mid-run cancel, per plan 35 decision 9.
