# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**GCP continuous delivery** — plan-less, single-PR: give the staging
environment a pipeline. Build → push → deploy → smoke, on every push to `main`,
in **mode 2** (Cloud SQL + GCS + Cloud KMS behind `existingSecret`).
Infrastructure stays human-driven; CD runs no Terraform (plan 20, Decision 9).
Every step below was run by hand against the real project first — Cloud Build
2m16s, three Deployments Ready, smoke 200 with the key and 401 without — and the
repo then made to match what ran.

## Tasks

- [x] `deploy/gcp/cloudbuild.yaml` — `DOCKER_BUILDKIT=1` (a pre-existing bug: the
      `FROM --platform=$BUILDPLATFORM` line needs it) and `CLOUD_LOGGING_ONLY`,
      required once a build names a service account. The two IAM grants that
      needs on `cd-deployer@` are recorded in `deploy/gcp/README.md`.
- [x] `deploy/gcp/staging-values.yaml` — the mode-2 input: `existingSecret`,
      bundled services off, all three Workload Identity annotations, the sandbox
      pair, LoadBalancer, modest requests. No credential, no `image.tag`.
- [x] `.github/workflows/deploy.yml` — WIF auth, Cloud Build with an explicit
      `--service-account`, the `map-platform` Secret assembled `--from-file`,
      `helm upgrade --install --wait --atomic`, then the two-sided smoke.
- [x] CI's `helm` job renders `staging-values.yaml` and asserts both halves of
      the sandbox pair reach the executor (the chart has no `values.schema.json`).
- [x] CD documented in `deploy/gcp/README.md` and `docs/deploy-gcp.md`.
- [ ] First run **from the workflow itself**: every step has been proven by hand,
      but no push to `main` has yet driven it end to end.
- [ ] Replace the `model-providers` placeholder (real endpoint, fake key) with a
      live route before anything runs a session.
