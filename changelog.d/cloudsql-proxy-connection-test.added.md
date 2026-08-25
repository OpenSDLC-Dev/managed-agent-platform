- **`cloudSQLProxy.connectionTest` makes an unreachable Cloud SQL instance fail the deploy**
  (#493). Without it the proxy reports itself started as soon as its listener is up — it
  dials nothing, and none of its three health endpoints dials either, `/readiness` included —
  so a connection name that is well-formed but names nothing produces three Ready pods and a
  green release that fails on its first query. Setting it passes `--run-connection-test`, so
  the proxy dials first, an instance it cannot reach exits the container non-zero, and
  `helm upgrade --wait --atomic` rolls the release back. It is **off by default**: it also
  fails a deploy on a transient Admin API blip, though `restartPolicy: Always` means the
  kubelet retries the sidecar and only a persistent failure survives the `--wait` window.
  GCP staging turns it on, because it substitutes the connection name in from the Admin API
  on every run — see [deploy/gcp/staging-values.yaml](./deploy/gcp/staging-values.yaml),
  which explains why that trade is the right one there and what the deploy's read-back check
  does *not* cover.
