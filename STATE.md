# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**#78 — confirming documented wire assumptions against a real managed-agents
endpoint.** Two recording sessions, 785 request/response pairs for about US$1.02, in
a private archive: bytes observed once, at cost. 2026-09-02 covered the Work API,
turn semantics, multiagent threads, memory stores, files, skills, vaults, deployments
and permission gating; 2026-09-03 ran two waves, one free over console-cookie
questions and one of 281 pairs against the tiers needing model spend, an environment
key, or a resource we lacked. #78 stays open past this work — 117 registry entries
still name it as their live tracker, and the debt is now analysis rather than
recording: 72 comparison rows have never been read. Plan 38 (#263) archived
2026-09-04, and plan 39 (#566, Skills on the GA wire shape) with its delivering PR.

## Tasks

- [x] Correct the three wire behaviors 2026-09-02 proved wrong: work-API
      cross-environment 403, agent-update null/empty-body no-op, interrupt result text
- [x] Reconcile 2026-09-02 into the registry — three entries moved out of INFERRED
      into CONFIRMED as argued divergences (#540, #541, #545), two proved to match
      us outright, four registered readings were refuted, and ten mismatches now
      belong to issues (#539, #542-#544, #546-#550, #553)
- [x] Reconcile 2026-09-03's second wave — fifteen archive comparison entries
      settled, two narrowed, one dissolved, touching 24 registry entries;
      thirteen mismatches are now issues (#570-#574, #577-#579, #581-#582,
      #589-#591), the `list_cost` unit is on #432, and #576 is registered
- [ ] Read the 72 un-analysed comparison rows from 2026-09-02 — the recording
      backlog is nearly exhausted and this one has never been touched
