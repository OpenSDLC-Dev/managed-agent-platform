# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Web-tools follow-up hardening** — the four issues split out of plan 15 (#47), worked in order: [#223](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/223) → [#225](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/225) → [#222](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/222) (plan) → [#226](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/226) (plan). Triage: #223/#225 single-PR direct; #222 and #226 need docs/plan/ files first (#222: repair-strategy decision, race reachable only on self_hosted `web_exec`; #226: wire shape unrecorded + no-sandbox web tools).

## Tasks

- [x] #223 — NUL-strip tool output at the toolset boundary (three-layer tests) + the executor result-event backstop for web error text
- [ ] #225 — operator-side `WEBTOOL_ALLOWED_DOMAINS` allowlist in the executor web driver (HostSet semantics)
- [ ] #222 — plan, then fix the self_hosted web_exec double-answer race
- [ ] #226 — plan the spill-to-file design (blocked on wire recording / sandbox-provisioning decision)
