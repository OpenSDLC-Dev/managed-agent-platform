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
      platform-executed tool leaving the session `running` matches the reference, the two
      client-answered lanes are #375's bug. #59 closed as answered.
- [ ] **#452** — the pointer invariant made executable: the guard, and the 33 pointers whose
      shared tracker still names nothing the entry citing it leaves open.
- [ ] **#447** — the session-scoped bound two agents messaging each other cannot escape.
      Design in progress; the plan file lands with the decision it records.
