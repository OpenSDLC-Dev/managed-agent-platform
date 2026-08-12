# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 29](./docs/plan/29_mcp-toolset.md) — the MCP client and `mcp_toolset` end to end
(#45): an agent configured with an `mcp_toolset` gets no tools today, and this plan
closes that with discovery, gating, credential injection and the reference's failure
semantics. Seven slices, two landed.

## Tasks

- [x] Slice 1 — wire correctness: bidirectional reference validation, nested
      `mcp_toolset` shape validation, resolved-config echo ([#343](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/343)).
- [x] Slice 2 — `internal/mcp` + catalog: the client and the shared dial-address guard
      ([#352](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/352)), then the `mcp_catalogs` migration and the executor's
      `mcp_exec` discovery driver ([#377](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/377)). Nothing offered to the model yet.
- [ ] Slice 3 — gate machinery: `confirmableToolUseTypes`, MCP-shaped denial/interrupt.
- [ ] Slice 4 — activation: brain expansion, `agent.mcp_tool_use`, the execution driver,
      four-way settlement, output spill. **#45's acceptance criterion is met here.**
- [ ] Slice 5 — credentials: matching, injection, OAuth refresh, `mcp_authentication_failed`.
- [ ] Slice 6 — networking polish: `allow_mcp_servers`, retry escalation, `mcp_connection_failed`.
- [ ] Slice 7 — evals, the live tier, the `ant` acceptance transcript, archiving.
