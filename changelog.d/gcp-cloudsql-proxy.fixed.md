- **The chart's docs no longer promise a Cloud SQL proxy backstop that does not exist** (#492).
  Three places — the chart README, [docs/deploy-gcp.md](./docs/deploy-gcp.md) and the
  `instanceConnectionName` guard's own comment — said a well-formed but wrong instance
  connection name is caught because "the proxy rejects it at startup". It is not: the pinned
  proxy dials nothing unless `--run-connection-test` is passed, which the chart does not pass,
  and none of its three health endpoints dials either — so the pod goes ready and the release
  succeeds on an instance nobody can reach. The render guard is a shape filter, not a
  backstop; #493 tracks giving the chart a real one.
