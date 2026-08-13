# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

- [Plan 29](./docs/plan/29_mcp-toolset.md) (#45) — give an agent with an `mcp_toolset`
  the tools it configures: discovery, gating, credentials, failure semantics.

## Tasks — plan 29

- [x] Slices 1–3 — wire correctness ([#343](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/343)); `internal/mcp`, the dial-address guard, the
      `mcp_catalogs` migration and the `mcp_exec` discovery driver ([#352](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/352), [#377](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/377)); gate
      machinery ([#387](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/387)). Nothing offered to the model yet; inert until slice 4 stamps a policy.
- [ ] Slice 4 — activation: brain expansion, `agent.mcp_tool_use`, the execution driver,
      four-way settlement, output spill. **#45's acceptance criterion is met here.**
- [ ] Slice 5 — credentials: matching, injection, OAuth refresh, `mcp_authentication_failed`.
- [ ] Slice 6 — networking polish: `allow_mcp_servers`, retry escalation, `mcp_connection_failed`.
- [ ] Slice 7 — evals, the live tier, the `ant` acceptance transcript, archiving.

