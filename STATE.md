# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 32](./docs/plan/32_management-api-keys.md) — management API keys (#378):
named, expiring, admin-issued over the console API, mirroring the reference
dialect recorded live on 2026-08-13. Tasks below is its progress.

[Plan 29](./docs/plan/29_mcp-toolset.md) — the MCP client and `mcp_toolset` end
to end (#45) runs in parallel on its own branch; slices 1–2 have landed
([#343](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/343),
[#352](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/352),
[#377](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/377)) and its
remaining slices are tracked on that branch, not here.

## Tasks

- [x] Slice 1 — the lifecycle in the schema, no new surface: migration 0024
      (`status` replacing `revoked_at`, `expires_at`, `partial_key_hint`,
      `created_by`; `api_keys_one_live` narrowed to the rows nobody issued),
      `authenticate` honouring status and expiry against the database clock,
      `EnsureAPIKey` recording a hint and keeping rotation-by-restart
      ([#385](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/385)).
- [ ] Slice 2 — the console surface: issue (returning the resource plus
      `raw_key`), list (bare array, archived rows included), update status and
      name. Admin-gated, `/api/console/organizations/{org}/workspaces/{ws}/api_keys`.
- [ ] Slice 3 — DIVERGENCES entries, a management-key section in
      docs/self-hosted-security.md, and an acceptance run: issue a key, drive
      `/v1` with it, disable it, re-enable it, archive it.
