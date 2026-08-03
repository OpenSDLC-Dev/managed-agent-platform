# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**#269** — the chart's Cloud SQL Auth Proxy sidecar and the brain's Google identity.
Plan-less (issue-driven); triaged as single-PR work.

## Tasks

- [x] `brain.serviceAccount.annotations` + ServiceAccount template, bound by the Deployment
- [x] `cloudSQLProxy` — off-by-default native sidecar in all three deployments, four
      render-time guards
- [x] `foundation/` `map-brain` account; `environment/` Workload Identity binding +
      `roles/cloudsql.client` for all three
- [x] Docs: deploy-gcp.md's two chart gaps retired, chart + `deploy/gcp` READMEs, CI
      assertions (the brain's ServiceAccount invariant restated, not dropped)

Evidence: `make gcp-fmt gcp-validate gcp-split-check gcp-lint` green (10 protected
resources); the helm job's steps run locally; the rendered manifests accepted by a real API
server under `kubectl apply --dry-run=server`. **Not** exercised on a live GKE cluster — the
proxy path itself is unrun.
