# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**#78 — confirming documented wire assumptions against a real managed-agents
endpoint.** A recording session against the live endpoint (2026-09-02, ~US$0.15 of
model spend) covered the Work API, turn semantics, multiagent threads, memory
stores, files, skills, vaults, deployments and permission gating. Findings were
compared entry-by-entry against [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) and
adversarially verified; 13 of 16 proposed code changes were rejected on that
second pass, which is why only the confirmed-wrong ones land. #78 stays open
past this work — 124 registry entries name it as their tracker and the recording
reached only part of what they ask. Plan 37 (#51) archived 2026-09-01.

## Tasks

- [x] Correct the three wire behaviors the recording proved wrong: work-API
      cross-environment 403, agent-update null/empty-body no-op, interrupt result text
- [x] Reconcile every recorded finding into the registry: three entries the
      recording settled and moved out of INFERRED (#540, #541, #545), one
      inference narrowed to what it actually proves, and four mismatches handed
      to the issues that now own them (#539, #542, #543, #544)
