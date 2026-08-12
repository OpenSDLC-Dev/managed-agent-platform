# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[Plan 31](./docs/plan/31_console-sso-rbac.md) — console SSO and RBAC (#56):
humans authenticate through any standards-compliant OIDC provider, and the
control plane resolves each request to a principal holding one of three
claim-mapped roles. Machine credentials and every documented CLI/SDK flow are
untouched. Six slices; slice 1 is in flight.

## Tasks

- [x] Slice 1 — `internal/identity`: the shared JWT verifier (bounded JWKS
      cache, algorithm allowlist, `iss`/`aud`/`sub`/`exp`/`azp` discipline),
      claim→role mapping, OIDC discovery, the `gcp-iap` preset, env config,
      and the `identitytest` fake provider. No route touched.
- [ ] Slice 2 — the principals migration and JIT provisioning; the identity
      lane in `dispatchAuth`, default-deny; `permission_error`; the console
      environment-key routes require `admin`.
- [ ] Slice 3 — the role matrix across the `/v1` and `/api/` route tables,
      plus the completeness test.
- [ ] Slice 4 — deployment wiring: the compose `iam` profile (Casdoor
      v3.152.0, hardened), Helm `identity.*`, GCP IAP Terraform, docs.
- [ ] Slice 5 — the api-key issuance surface, gated on a live observation of
      the reference console's dialect.
- [ ] Slice 6 — acceptance against a real Casdoor token, then archive.
