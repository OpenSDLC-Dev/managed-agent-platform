# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**The session-delete family** — three issues on what `deleteSession` does after its commit,
taken in this order because each leaves ground the next stands on. No plan file: `issue-triage`
judged #646 direct, and the other two are judged as they start.

## Tasks

- [x] #646 — the terminal broadcasts run detached from the request, so a client that hangs up
  after the commit no longer cancels the child-termination frame every other subscriber is owed.
- [ ] #354 — the delete does not kick the executor's reaper, so a deleted session's sandbox
  can outlive it by a full reap interval.
- [ ] #645 — deliverable objects the post-commit cleanup does not remove are orphaned for good;
  no tier knows their keys, so nothing retries them.
