# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 68](./docs/plan/68_session-usage-event.md) (#432, one slice): the `session.usage`
event before every `session.status_idle`, and `usage.active_seconds`. One PR.

## Tasks

- [ ] Migration 0055: `sessions.active_seconds` and `running_since`, backfilled
- [ ] `session.usage` before each session-level idle in `Log.AppendInTx`
- [ ] `usage.active_seconds` on the session resource
- [ ] Sequence tests, registry, changelog
