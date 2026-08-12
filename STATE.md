# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**none.**

[Plan 31](./docs/plan/31_console-sso-rbac.md) — console SSO and RBAC (#56) —
archived 2026-08-12 on what it shipped: slices 1–4
([#369](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/369),
[#370](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/370),
[#371](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/371),
[#374](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/374)) and an
acceptance run proving the chain end to end — a real Casdoor token minted by
code+PKCE, a viewer refused a mutation, an admin issuing an environment key over
the console API, and a real `ant beta:worker poll` authenticating with it
(docs/HISTORY.md). Slice 5, the api-key issuance surface, did not ship: it is
gated on a live observation of the reference console's dialect, so it moved to
[#378](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/378) under
the plan's own provision rather than shipping an invented dialect.

## Tasks

None in flight. The backlog is [GitHub issues](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues).
