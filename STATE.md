# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Processing order within a commit** ([plan 56](./docs/plan/56_processing-order.md), #793):
the log is written in the order its events are processed, as the reference lists it. Three
PRs; #793 stays open until the third, and #539 closes with the first.

## Tasks

- [x] PR-A — a wake's running pair before the `user.message` or `user.define_outcome` it
      consumes, an interrupt after the results it settles (#539), a delivered message
      after its target's running event, and the child-resume shape registered as
      deliberate
- [x] PR-B — a message posted mid-turn replays after the reply it never saw, and each
      inbound event is stamped where it is consumed; list and stream keep receipt order
- [ ] PR-C — an answer-resumed thread's running pair ahead of what its turn consumes, a
      denial's result included (2026-09-12-archived-threads batch1 idx 31-35)
