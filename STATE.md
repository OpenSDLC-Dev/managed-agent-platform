# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

- [Plan 29](./docs/plan/29_mcp-toolset.md) (#45) — give an agent with an `mcp_toolset`
  the tools it configures: discovery, gating, credentials, failure semantics.

## Tasks — plan 29

- [x] Slices 1–3 — wire correctness ([#343](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/343)); `internal/mcp`, the dial-address guard, the
      `mcp_catalogs` migration and the `mcp_exec` discovery driver ([#352](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/352), [#377](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/377)); gate
      machinery ([#387](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/387)). Nothing offered to the model yet; inert until slice 4 stamps a policy.
- [x] Slice 4 — activation, and with it **#45's acceptance criterion**: the execution driver
      and MCP-first settlement ([#398](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/398)); the brain offering an `mcp_toolset`'s tools as
      `mcp__{server}__{tool}` ([#402](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/402)); an oversized answer spilling into the
      session's sandbox ([#404](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/404)).
- [x] Slice 5 — credentials: matching, bearer injection on both dial paths, and
      `mcp_authentication_failed_error` split off the connection failure ([#405](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/405)); an
      expired `mcp_oauth` token refreshing at the dial, the rotation sealed back
      onto the row ([#406](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/406)).
- [x] Slice 6 — networking polish: `allow_mcp_servers` widens the per-session gate, so a
      sandbox reaches the servers its agent declares
      ([#407](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/407)); the
      discovery pass dials concurrently, so declaration order stops deciding who gets
      reached, and a server it cannot reach is said out loud
      ([#409](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/409)).
- [ ] Slice 7 — evals, the live tier, the `ant` acceptance transcript, archiving.
