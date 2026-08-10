# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 29](./docs/plan/29_mcp-toolset.md) — the MCP client and `mcp_toolset` wired
end to end (#45). Seven slices; #45's acceptance criterion is met at the end of
slice 4.

[Plan 30](./docs/plan/30_environment-keys-console-issuance.md) — console-issued
environment keys (#43): named, per-host, individually revocable. Slice 3 of 3
remains; the UI half is managed-agent-console plan 07.

**GCP continuous delivery** — plan-less, **delivered and green**; the record is
in [docs/HISTORY.md](./docs/HISTORY.md). One task remains, and it blocks every
session.

## Tasks

- [x] Plan 29 slice 1 — wire correctness: dangling/nested `mcp_toolset`
      rejection on every spec-resolving surface, resolved-config echo.
- [ ] Plan 29 slice 2 — `internal/mcp` client landed (streamable HTTP, bearer
      injection, self-driven paging, `internal/dialguard` extracted). Open:
      `mcp_catalogs` migration + store, `Conn.CallTool` (slice 4 needs it), the
      `mcp_exec` driver and its egress check.
- [ ] Plan 29 slices 3–7 — confirmation gating and denial synthesis; brain
      expansion, execution driver, settlement chaining, spill; vault credentials
      and OAuth refresh; networking polish; evals, `ant` acceptance, archive.
- [x] Plan 30 slice 1 — storage and primitives: migration 0021, `Issue`/`List`/
      `RevokeEnvironmentKey`, expiry-aware auth. [envkeys.go](./internal/api/envkeys.go)
- [x] Plan 30 slice 2 — the off-wire console API: `POST`/`GET …/tokens` and
      `POST …/tokens/{token_id}/revoke` under `/api/oauth/organizations/…`,
      mirrored from the reference console, management-auth only, `/v1` untouched.
      [consoleapi.go](./internal/api/consoleapi.go)
- [ ] Plan 30 slice 3 — a real `ant beta:worker` acceptance run on a
      console-issued key against the compose stack, then archive.
- [ ] Replace the `model-providers` placeholder (real endpoint, fake key) with a
      live route before anything runs a session.
