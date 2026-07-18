# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size
budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in
[CLAUDE.md](./CLAUDE.md), the as-built system in
[docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in
[CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this
file's claims against reality on its docs-consistency rung.

## Active work

**[docs/plan/03_docs-restructure.md](./docs/plan/03_docs-restructure.md)** (in-progress)
— docs restructure: ARCHITECTURE.md + HISTORY slimming, STATE.md as a pure work tracker,
issue-triage subagent.

## Tasks

- [x] PR A — docs/ARCHITECTURE.md created; HISTORY.md slimmed (530→217 lines) under the
  one-writer rule. Merged as [#101](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/101).
- [x] PR B — STATE.md reduced to Active work + Tasks; environment notes and doc pointers
  folded into CLAUDE.md; verifier STATE checks updated — this PR.
- [ ] PR C — `.claude/agents/issue-triage.md` (Sonnet 5, read-only, JSON-only triage
  verdict) + the CLAUDE.md trigger rule.
