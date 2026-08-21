# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

The two follow-ups the plan-35 closeout work filed, taken together because the first is
what makes the second's record true. Triaged: #445 needs no plan file, #447 does — its
own body admits three designs and none is obviously right.

## Tasks

- [x] **#445** — the 60 stale `Tracked:` pointers repaired: annotated where the closure was
      delivery, re-pointed at an open tracker where it orphaned an INFERRED entry, naming what
      that entry still leaves open. The Format legend now states the rule. CLAUDE.md's post-v1
      enumeration gives way to the `(post-v1)` title marker and a bounded query. Defects the
      sweep surfaced outside that scope are filed as issues, not carried here.
- [ ] **#447** — the session-scoped bound two agents messaging each other cannot escape.
      Design in progress; the plan file lands with the decision it records.
