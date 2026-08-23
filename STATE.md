# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

The registry follow-ups the #445 pointer sweep filed, and the plan-35 bound it did not.
None needed a plan file: the registry work repairs a file whose conventions are written
down, and #447 was re-triaged once its design was, #446 being the same mechanism.

## Tasks

- [x] **#450 / #451** — the CONFIRMED and INFERRED headings state their test now: two of the
      four self-contradicting entries move under it, the rule keeps the other two, and the
      session-status entry and its cadence twin say what the public docs settle — the
      custom-tool lane is #375's bug, a `self_hosted` worker's a new inference. #59 closed.
- [x] **#452** — the pointer invariant made executable. `tools/registrycheck`'s shape rules run
      in the gate, its issue-state rule on a schedule; the 33 shared-tracker pointers now name
      what their own entry leaves open, the five whose tracker had been re-scoped out from under
      them are re-pointed, and it surfaced the two defects below.
- [x] **#458** — the two entries whose text read as mirrors are converged, and each now says
      which divergence it is the record of. The skills entry's was the deferral behind the
      field, whose naming sentence had been deleted when slice 5 closed it; the `expires_at`
      entry's was the refusal of a past instant that #389 lifted. Neither changes section.
- [x] **#457 / #462** — the work API's enqueue trigger is a divergence, and now says so: there
      the session itself is queued, here only a commit leaving runnable sandbox work creates an
      item. Of the four kinds the issue names, only `tool_exec` reaches a BYOC worker. #462,
      found writing it, repairs `workAPIScope`'s comment in the same PR.
- [x] **#447** — the session-scoped bound two agents messaging each other cannot escape.
      `sessions.delegation_turns` counts what a session spends when nobody is asking, and the
      claim is refused at 625 rather than the settlement cut. The third escape route the issue
      does not name is `chainInput`, which reads a peer's message as input and beats the cap.
