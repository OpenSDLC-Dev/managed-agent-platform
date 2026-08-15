- **Deployment identifiers leave the public repository** (#355, PR #356; console
  [#69](https://github.com/OpenSDLC-Dev/managed-agent-console/issues/69)). The
  deploy workflow, `staging-values.yaml` and the GCP runbook no longer name one
  operator's project, cluster, registry, buckets, KMS key or identities. Eleven
  GitHub Actions **variables** carry them instead (not secrets — none is a
  credential): `GCP_PROJECT_ID`, `GCP_ZONE`, `GKE_CLUSTER`, `ARTIFACT_REGISTRY`
  (one `HOST/PROJECT/REPOSITORY` string), `WIF_PROVIDER`,
  `DEPLOY_SERVICE_ACCOUNT`, `BLOB_BUCKET`, `KMS_KEY_NAME`,
  `CONTROLPLANE_SERVICE_ACCOUNT`, `BRAIN_SERVICE_ACCOUNT`,
  `EXECUTOR_SERVICE_ACCOUNT` — sourced per `deploy/gcp/README.md`. A deploy fails
  before authenticating if any is unset, blank or comma-bearing.
  `staging-values.yaml` becomes a reference deployment written against
  `registry.invalid` and `your-project`; the workflow overrides those keys and
  then reads the three Workload Identity annotations back from the cluster, so a
  lost override fails the run instead of silently degrading the brain.
