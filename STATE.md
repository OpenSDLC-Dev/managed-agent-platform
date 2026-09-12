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
- [x] #710 — an archived session renders `terminated`, projected from `archived_at` on the
      session object and the `statuses[]` filter; the reaper's `terminated` tier stays, since
      #577 would produce the status it reads
- [ ] #574 — whether the default listing hides `terminated`; #710 leaves only archived ones,
      which that listing already excludes, so the open case is a stored one (#577's)
