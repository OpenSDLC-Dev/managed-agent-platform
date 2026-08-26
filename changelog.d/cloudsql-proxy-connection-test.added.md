- **`cloudSQLProxy.connectionTest` makes the Cloud SQL proxy prove its instance is reachable**
  (#493). The proxy reports itself started as soon as its listener is bound — it dials
  nothing, and none of its three health endpoints dials either, `/readiness` included. Once
  the DSN routes through the proxy that is survivable, because the platform pings the
  database before serving and exits when it cannot, so a wrong instance crash-loops the pods
  and `helm upgrade --wait --atomic` rolls the release back anyway; what it costs is the
  diagnosis, since the visible failure is three application containers unable to reach
  Postgres. Setting this passes `--run-connection-test`, so the sidecar dials first and exits
  naming the instance it could not open. It matters most in the window where the proxy is
  enabled but the DSN does **not** route through it yet — a cutover's first step — because
  nothing exercises the proxy there at all. Off by default, since it otherwise duplicates the
  platform's own check a second earlier. It is a boolean: pass it with `--set`, never
  `--set-string`. GCP staging turns it on.
