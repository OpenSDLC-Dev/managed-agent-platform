# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Plan 12 — vaults + egress-time credential injection** ([docs/plan/12_vaults-credentials.md](./docs/plan/12_vaults-credentials.md), `in-progress`, issue #50). Four PR slices; BYOC delivery and TLS-terminating substitution deliberately split out (#165, #166).

## Tasks

- [x] Slice 1 — cipher seam + infrastructure (PR #168, merged): `internal/secrets` (Cipher iface, `local` + `openbao` backends, contract suite, `secretstest` harness), controlplane/executor plumbing, compose `openbao` + init one-shot, helm StatefulSet + `externalOpenBao` + `localCipher` — all runtime-verified.
- [x] Slice 2 — `/v1/vaults` + credentials CRUD, wire-complete (this PR): migration 0011, `vcrd` prefix, full auth-union validation, sealed secrets via the cipher, `mcp_oauth_validate` live probe, DIVERGENCES entries + `work.secret` re-pointed at #165.
- [ ] Slice 3 — session `vault_ids` attachment + read-time resolution.
- [ ] Slice 4 — egress gate phase 1 (domain gate proxy, placeholder minting + substitution engine, K8s UID-owner iptables sidecar, DIVERGENCES.md:42 superseded).
