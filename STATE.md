# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**None.** [Plan 33](./docs/plan/33_bounding-a-wedged-work-item.md) (#383) archived 2026-08-15 —
a work item is bounded by its own silence in both halves of the pull protocol, so a wedged call
is cancelled and its item reclaimed rather than held forever; its slices 2 and 3 are deferred to
#395 and #396. [Plan 29](./docs/plan/29_mcp-toolset.md) (#45) archived the same day — an agent
with an `mcp_toolset` calls a real MCP server's tools, with discovery, gating, credentials
and failure semantics. Both delivery records are in [docs/HISTORY.md](./docs/HISTORY.md).

The backlog is GitHub issues; nothing is in flight.
