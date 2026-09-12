# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Aligning the archive's endings with the reference**, the three issues a 2026-09-12 recording
left decided: the reference reports an archived session `terminated` over a primary it leaves
unarchived, and this platform reports neither — it archived the primary instead.
#713 lands the thread row, #710 the session status as a read-time projection from
`archived_at`, #574 the listing that follows from it. The recording and what it settled are in
[docs/DIVERGENCES.md](./docs/DIVERGENCES.md)'s INFERRED section under #78.

## Tasks

- [x] #713 — the session archive stops mirroring `archived_at`/`updated_at` onto the primary
- [ ] #710 — render `terminated` when `archived_at` is set, and retire the reaper tier, its
      fixture-only test and the four docs that describe an ending the column never holds
- [ ] #574 — whether the default session listing hides `terminated`, once #710 produces one
