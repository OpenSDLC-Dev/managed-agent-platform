# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**[docs/plan/20_gcp-deployment.md](./docs/plan/20_gcp-deployment.md) (in-progress)** — the
Google Cloud production-deployment plan, authored and approved 2026-08-01 from GCP probes
run the same day. Slices 1 and 2 have landed; slice 3 is the Cloud KMS cipher.

## Tasks

- [x] Plan 20 authored and approved — evidence in its Ground truth section
- [x] Slice 1 — GCS delete convergence (`internal/blob/s3`)
- [x] Slice 2 — sandbox pod placement and bounds:
  - [x] 2a — `seccompProfile: RuntimeDefault`, pod-level and unconditional
  - [x] 2b — opt-in `SANDBOX_EPHEMERAL_STORAGE_BYTES` disk cap + chart knob
  - [x] 2c — node selection and tolerations, parsed and fail-closed
- [x] Slice 3 — `internal/secrets/gcpkms` cipher + size guard + chart knob + `cmd/` wiring (this PR)
- [ ] Slice 4 — staging environment (`deploy/gcp/`) + mode-1 acceptance on GKE
- [ ] Slice 5 — mode-2 acceptance + `docs/deploy-gcp.md`
