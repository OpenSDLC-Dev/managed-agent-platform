- **GCP staging runs the Cloud SQL Auth Proxy.** `cloudSQLProxy.enabled` is on in
  [deploy/gcp/staging-values.yaml](./deploy/gcp/staging-values.yaml), so the control plane,
  brain and executor each get the proxy as a native sidecar, and `deploy.yml` resolves the
  instance's connection name from the Cloud SQL Admin API at deploy time — no instance
  identifier is written into this repository, and no new repository variable is needed. The
  point is that a rebuilt Cloud SQL instance gets a new private IP while its connection name
  does not change, so the DSN stops being a thing a rebuild silently invalidates. The IAM
  half was already in place: `environment/` grants all three service accounts
  `roles/cloudsql.client` unconditionally. **One step is deliberately not automatic** —
  `database-url` is a human-created secret by design, so pointing it at the proxy's loopback
  socket is an operator action, documented with its rollback in
  [deploy/gcp/README.md](./deploy/gcp/README.md). Until then the proxy runs unused and
  connectivity is unchanged.
