# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

The registry follow-ups the pointer and section sweeps filed, and the plan-35 bound they did
not. Triaged: no registry issue needs a plan file, each repairing a file whose conventions
are already written; #447 does, its own body admitting three designs and none obviously right.

## Tasks

- [x] **#450 / #451** — the CONFIRMED and INFERRED headings state their test: two of the four
      self-contradicting entries moved under it, the rule kept the other two, and the
      session-status pair says what the public docs settle. #375 owns the bug; #59 closed.
- [x] **#452** — the pointer invariant made executable. `tools/registrycheck`'s shape rules run
      in the gate, its issue-state rule on a schedule; the 33 shared-tracker pointers now name
      what their entry leaves open, five are re-pointed, and it surfaced the defects below.
- [x] **#458** — the two entries whose text read as mirrors are converged, and each now says
      which divergence it records: the skills entry's deferral, whose naming sentence slice 5
      deleted, and the `expires_at` refusal of a past instant that #389 lifted. Neither moved.
- [x] **#457 / #462** — the work API's enqueue trigger is a divergence, and now says so: there
      the session itself is queued, here only a commit leaving runnable sandbox work creates an
      item. Of the four kinds the issue names, only `tool_exec` reaches a BYOC worker. #462,
      found writing it, repairs `workAPIScope`'s comment in the same PR.
- [x] **#463 / #465** — the two the headings still caught. The session-id-prefix mirror was
      never observed on the reference at all, so it moves to INFERRED under #78 rather than to
      the notes; the `expires_at` `name` bound and three siblings now name what settles them.
- [ ] **#447** — the session-scoped bound two agents messaging each other cannot escape.
      Design in progress; the plan file lands with the decision it records.
