# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

Three unrelated things in flight.

[Plan 29](./docs/plan/29_mcp-toolset.md) — the MCP client and `mcp_toolset`
wired end to end (#45). Seven slices; #45's acceptance criterion is met at the
end of slice 4.

[Plan 30](./docs/plan/30_environment-keys-console-issuance.md) — console-issued
environment keys (#43): named, per-host, individually revocable. Three slices;
the UI half is managed-agent-console plan 07 and trails slice 2.

**GCP continuous delivery** — plan-less, **delivered and green**; the record is
in [docs/HISTORY.md](./docs/HISTORY.md). One task remains, and it blocks every
session.

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
- [ ] Slices 3–7 — confirmation gating and denial synthesis; brain expansion,
      execution driver, settlement chaining, spill; vault credentials and OAuth
      refresh; networking polish; evals, `ant` CLI acceptance, archive.
- [x] **Plan 30 slice 1 — storage and primitives.** Migration 0021 adds `name` /
      `expires_at` and retires `environment_keys_one_live`, so an environment
      holds one key per host; `IssueEnvironmentKey` (server-generated secret,
      hash-only, one-year expiry) / `List` / `Revoke` replace
      `EnsureEnvironmentKey`, and an expired key 401s exactly as a revoked one.
      Evidence: [envkeys.go](./internal/api/envkeys.go),
      [envkeys_test.go](./internal/api/envkeys_test.go).
- [ ] Plan 30 slices 2–3 — the off-wire `/api/oauth/…/tokens` issuance endpoints
      mirroring the reference console's API, then a real `ant beta:worker`
      acceptance run on a console-issued key, then archive.
- [ ] Replace the `model-providers` placeholder (real endpoint, fake key) with a
      live route before anything runs a session.
