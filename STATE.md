# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 29](./docs/plan/29_mcp-toolset.md) — the MCP client and `mcp_toolset` wired
end to end (#45); #45's acceptance criterion is met at the end of slice 4.
**GCP continuous delivery** — plan-less, delivered and green; the record is in
[docs/HISTORY.md](./docs/HISTORY.md). One task remains and blocks every session.

## Tasks

- [x] Plan 29 slice 1 — `mcp_toolset` wire correctness on every spec-resolving
      surface, plus the resolved-config echo.
- [ ] Plan 29 slice 2 — the `internal/mcp` client landed. Open: `mcp_catalogs`
      migration + store, `Conn.CallTool` (slice 4 needs it), the `mcp_exec`
      driver and its egress check.
- [ ] Plan 29 slices 3–7 — confirmation gating; brain expansion and execution;
      vault credentials; networking; evals, `ant` acceptance, archive.
- [ ] Replace the GCP staging `model-providers` placeholder (real endpoint, fake
      key) with a live route before a session runs there (`deploy/gcp/README.md`).
