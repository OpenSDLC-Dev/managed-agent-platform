# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 31](./docs/plan/31_console-sso-rbac.md) — console SSO and RBAC (#56):
humans authenticate through any standards-compliant OIDC provider, and the
control plane resolves each request to a principal holding one of three
claim-mapped roles. Machine credentials and every documented CLI/SDK flow are
untouched. Six slices; the enforcement point is complete and slices 1–3 have
landed ([#369](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/369), [#370](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/370), [#371](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/371)) and slice 4 (deployment wiring) is in review.

[Plan 29](./docs/plan/29_mcp-toolset.md) — MCP end to end (#45) runs in parallel
on its own branch, so Tasks below is plan 31's: slices 1–2 have landed (wire
correctness; the client, `mcp_catalogs`, the discovery driver), slice 3 is next.

## Tasks

- [x] Slice 1 — `internal/identity`: the shared JWT verifier (bounded JWKS
      cache, algorithm allowlist, `iss`/`aud`/`sub`/`exp`/`azp` discipline),
      claim→role mapping, OIDC discovery, the `gcp-iap` preset, env config,
      and the `identitytest` fake provider. No route touched.
- [x] Slice 2 — the `principals` migration and JIT provisioning; the identity
      lane in `dispatchAuth` (machine credentials first, JWT-silhouette split,
      an explicit `/api/` arm); the per-route minimum role and `permission_error`;
      default-deny everywhere, with the console environment-key routes at `admin`.
- [x] Slice 3 — the role matrix across the `/v1` and `/api/` route tables
      (23 viewer / 25 developer / 5 admin; work + gate routes stay role-free),
      the three streaming routes checked explicitly, and the source-parsing
      completeness test that fails on a route without a role.
- [x] Slice 4 — deployment wiring ([#374](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/374)):
      compose's `iam` profile (Casdoor 3.152.0 behind a SAML-denying,
      TLS-terminating proxy), the same seed rendered by Helm without its demo
      accounts and owning `built-in/admin`, GCP's IAP `trusted_proxy` Terraform.
- [ ] Slice 5 — the api-key issuance surface, gated on a live observation of
      the reference console's dialect.
- [ ] Slice 6 — acceptance against a real Casdoor token, then archive.
