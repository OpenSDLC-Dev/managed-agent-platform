# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**[docs/plan/19_gcp-deployment.md](./docs/plan/19_gcp-deployment.md) (approved)** — the
Google Cloud production-deployment plan, authored and approved 2026-08-01 from GCP probes
run the same day. Development starts at slice 1.

## Tasks

- [x] Plan 19 authored and approved (this PR) — evidence in its Ground truth section
- [ ] Slice 1 — GCS delete convergence (`internal/blob/s3`)
- [ ] Slice 2 — sandbox pod resource bounds + the free hardening (seccomp, no-priv-esc)
- [ ] Slice 3 — `internal/secrets/gcpkms` cipher + chart knob + `cmd/` wiring
- [ ] Slice 4 — staging environment (`deploy/gcp/`) + mode-1 acceptance on GKE
- [ ] Slice 5 — mode-2 acceptance + `docs/deploy-gcp.md`
