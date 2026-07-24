# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Plan 12 — vaults + egress-time credential injection** ([docs/plan/12_vaults-credentials.md](./docs/plan/12_vaults-credentials.md), `in-progress`, issue #50). Slices 1–3 merged; slice 4 (the egress gate) lands as several sub-PRs. BYOC delivery and TLS-terminating substitution deliberately split out (#165, #166).

## Tasks

- [x] Slice 1 — cipher seam + infrastructure (PR #168, merged): `internal/secrets` (Cipher iface, `local` + `openbao` backends, contract suite, `secretstest` harness), controlplane/executor plumbing, compose `openbao` + init one-shot, helm StatefulSet + `externalOpenBao` + `localCipher` — all runtime-verified.
- [x] Slice 2 — `/v1/vaults` + credentials CRUD, wire-complete (PR #169, merged): migration 0011, `vcrd` prefix, full auth-union validation, sealed secrets via the cipher, `mcp_oauth_validate` live probe, DIVERGENCES entries + `work.secret` re-pointed at #165.
- [x] Slice 3 — session `vault_ids` attachment (PR #170, merged): `POST /v1/sessions` accepts/validates (existing + unarchived, `FOR SHARE`) and round-trips `vault_ids`; update stays wire-faithfully rejected; DIVERGENCES:28 create-rejection lifted + create-time error-shape inference recorded. Read-time resolution moves to slice 4 (built beside its egress consumers so its shape isn't guessed).
- [ ] Slice 4 — egress gate phase 1, landing as sub-PRs. **Done:** (a) `sandbox.Spec.Env` seam threaded into both backends with `ValidateEnv` key gating (PR #172); (b) `internal/egress` substitution engine — `HostSet` allowed_hosts matcher, `vltph_` placeholder mint, `Engine.Substitute` (host + injection_location gated, host-unreachable reporting), pure + unit-tested (this PR). **Remaining:** read-time resolution (attached vaults → active env-var credentials, first-vault-wins) + placeholder minting into `Spec.Env`; per-session gate proxy + `HTTP(S)_PROXY` injection + `credential_host_unreachable_error` emission; K8s UID-owner iptables sidecar (replacing route-flush); contract rows "limited = only allowed_hosts" + DIVERGENCES.md:42 superseded.
