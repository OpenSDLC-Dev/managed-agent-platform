# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**#78 — confirming documented wire assumptions against a real managed-agents
endpoint.** Two recording waves now, 785 request/response pairs for about US$1.02,
kept outside this repo in a private archive because they are bytes observed once at
cost. The first (2026-09-02) covered the Work API, turn semantics, multiagent
threads, memory stores, files, skills, vaults, deployments and permission gating;
the second (2026-09-03) took the tiers that needed model spend, an environment key,
or a resource we lacked. #78 stays open past this work — over 120 registry entries
name it as their tracker and the recordings reached only part of what they ask, and
the larger debt is now analysis rather than recording: 72 comparison rows from the
first wave have still never been read. Plan 37 (#51) archived 2026-09-01.

## Tasks

- [x] Correct the three wire behaviors the first wave proved wrong: work-API
      cross-environment 403, agent-update null/empty-body no-op, interrupt result text
- [x] Reconcile the first wave into the registry — three entries moved out of
      INFERRED into CONFIRMED as argued divergences (#540, #541, #545), two proved
      to match us outright, four registered readings were refuted, and ten
      mismatches now belong to issues (#539, #542-#544, #546-#550, #553)
- [x] Reconcile the second wave — fifteen entries settled, two narrowed, one
      dissolved; five mismatches now belong to issues (#570-#574) and the
      `list_cost` unit to #432
