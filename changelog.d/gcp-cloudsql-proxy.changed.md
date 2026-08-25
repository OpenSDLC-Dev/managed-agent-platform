- **GCP staging runs the Cloud SQL Auth Proxy** (#492). `cloudSQLProxy.enabled` is on in
  [deploy/gcp/staging-values.yaml](./deploy/gcp/staging-values.yaml), so the control plane,
  brain and executor each get the proxy as a native sidecar, and `deploy.yml` resolves the
  instance's connection name from the Cloud SQL Admin API at deploy time — no operator's
  project is written into this repository, and no new repository variable is needed. The
  point is that a rebuilt Cloud SQL instance gets a new private IP while its connection name
  does not change, so the DSN stops being a thing a rebuild silently invalidates. The
  workloads' IAM was already in place — `environment/` grants all three service accounts
  `roles/cloudsql.client` unconditionally — but **the deploy identity needs a new
  `roles/cloudsql.viewer` grant** to read the instance, recorded in
  [deploy/gcp/README.md](./deploy/gcp/README.md) beside the other grants that live outside
  Terraform. **One step is deliberately not automatic**: `database-url` is a human-created
  secret by design, so pointing it at the proxy's loopback socket is an operator action,
  documented with its rollback in the same file. Until then the proxy runs unused and
  connectivity is unchanged.
