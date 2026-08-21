# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Plan 35's three follow-up issues**, taken in order. No plan file: an adversarial
review of slice 4 already scoped each one, and none changes a wire shape.
[Plan 35](./docs/plan/35_multiagent-threads.md) (#53) itself archived 2026-08-21 —
its delivery record is [docs/HISTORY.md](./docs/HISTORY.md).

## Tasks

- [x] **#431** — the advisor roster entry. Decided at slice 1 and already enforced
      (`400 entry type must be "agent" or "self"`); this PR re-trues the two
      [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) entries that described it wrong,
      re-points the plan-35 `Tracked:` pointers from the closed #53 to #78, and the
      issue closes with it.
- [ ] **#442** — bound the settlement chain (the one with a user-visible consequence:
      a chained `model_turn` starves every other session on a single-brain
      deployment), align `WakeThread`'s unanswered-`tool_use` guard with the API's,
      cover `commitDelegatedTurn`'s ask-gated branch, settle the redundant `park` half.
- [ ] **#441** — the BYOC worker cancels an answered call mid-run, as plan 35
      decision 9 says it does, and skips a duplicate-result 400 instead of aborting
      the pass.
