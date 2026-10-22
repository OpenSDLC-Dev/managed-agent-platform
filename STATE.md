# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Six quick fixes from the 2026-10-17 issue triage**, plan-less, one PR each, in the
order below. Issue triage found that none of them needs a plan. Two of them land only
part of their issue, which stays open for the rest.

## Tasks

- [x] #589: refuse a past `mcp_oauth` `expires_at` on credential create (an update still accepts one).
- [x] #549: refuse a memories-list `path_prefix` that does not end with `/` (order and `depth` stay open).
- [x] #543: refuse a cron schedule that restricts both day-of-month and day-of-week.
- [x] #573: reserve the `mcp__` prefix against custom tool names (the 64-byte cap stays open).
- [ ] #552: stamp `memory_versions.created_at` with `clock_timestamp()`.
- [ ] #634: two file-tool errors stop advising `bash`.
