# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

The registry follow-ups the #445 pointer sweep filed, and the plan-35 bound it did not.
Triaged: #450–#452 need no plan file, repairing a file whose conventions are already
written; #447 does, its own body admitting three designs and none obviously right.

## Tasks

- [x] **#450 / #451** — the CONFIRMED and INFERRED headings state their test now: two of the
      four self-contradicting entries move under it, and the rule keeps the other two. The
      session-status entry and its cadence twin say what the public docs settle — a
      platform-executed tool leaving the session `running` matches the reference, the
      custom-tool lane is #375's bug, and a `self_hosted` worker's is a new inference.
      #59 closed as answered.
- [x] **#452** — the pointer invariant made executable. `tools/registrycheck`'s shape rules run
      in the gate, its issue-state rule on a schedule; the 33 shared-tracker pointers now name
      what their own entry leaves open, and the five whose tracker had been re-scoped out from
      under them are re-pointed. Two defects the sweep surfaced are filed as #457 and #458.
- [ ] **#447** — the session-scoped bound two agents messaging each other cannot escape.
      Design in progress; the plan file lands with the decision it records.
