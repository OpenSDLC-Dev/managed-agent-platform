- **Continuous delivery to the GCP staging environment** (#347; the ref assertion below was
  corrected in #351). A new
  `.github/workflows/deploy.yml` builds, pushes, installs and smoke-tests on every push to
  `main` and on `workflow_dispatch`, using the new `deploy/gcp/staging-values.yaml`. The
  deployment is **mode 2** (Cloud SQL, Cloud Storage, Cloud KMS) behind the pre-created Secret
  `map-platform`, so the values file carries no credential: the job authenticates by Workload
  Identity Federation and assembles that Secret from Secret Manager, and there is no GitHub
  secret. A dispatch from any ref but `refs/heads/main` is refused before the auth step, and
  again cloud-side by the provider's `assertion.ref`; `id-token: write` is scoped to the one
  job rather than the workflow. CD never runs Terraform — `make gcp-bootstrap` and
  `make gcp-db-init` stay human-driven, which is also why rotation differs by secret:
  [deploy/gcp/README.md](./deploy/gcp/README.md) carries the per-secret procedure. The smoke
  check demands 200 with the management key and 401 without — over plain HTTP on a bare IP, so
  the key travels unencrypted, which is deliberate for staging only and costed in
  [docs/deploy-gcp.md](docs/deploy-gcp.md) under "Exposing the control plane". `model-providers`
  holds a placeholder key a human must replace.
