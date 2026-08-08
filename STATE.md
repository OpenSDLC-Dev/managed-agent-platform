# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 29](./docs/plan/29_mcp-toolset.md) — the MCP client and `mcp_toolset`
wired end to end (#45). Seven slices; #45's acceptance criterion is met at the
end of slice 4.

## Tasks

- [x] **Slice 1 — wire correctness.** Dangling `mcp_toolset` rejected on all four
      spec-resolving surfaces and on stored specs, `mcp_toolset` nested-shape
      validation (#26's fail-open class closed on the MCP arm), resolved-config
      echo for both toolset kinds at render. #66's one-way reading corrected,
      #59 settled and closable. Evidence: [materialize.go](./internal/toolset/materialize.go),
      [wire.go](./internal/api/wire.go),
      [mcptoolset_test.go](./internal/api/mcptoolset_test.go).
- [ ] Slice 2 — `internal/mcp` client + `mcp_catalogs` + the discovery driver.
      Client landed: streamable HTTP, bearer injection, self-driven paging, a
      listing that survives a hostile server, and `internal/dialguard` extracted
      as one guard under it and the vault probe. Still open: `mcp_catalogs`
      migration + store, `Conn.CallTool` (needed by slice 4), the `mcp_exec`
      driver and its egress check. Evidence: [mcp.go](./internal/mcp/mcp.go),
      [dialguard.go](./internal/dialguard/dialguard.go).
- [ ] Slice 3 — MCP confirmation gating and denial synthesis.
- [ ] Slice 4 — brain expansion, execution driver, settlement chaining, spill.
- [ ] Slice 5 — vault credentials, injection, OAuth refresh.
- [ ] Slice 6 — networking polish and retry semantics.
- [ ] Slice 7 — evals, live tier, `ant` CLI acceptance, archive.
