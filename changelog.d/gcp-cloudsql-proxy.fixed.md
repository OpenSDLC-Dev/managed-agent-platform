- **The chart's docs no longer promise a Cloud SQL proxy backstop that does not exist** (#492).
  Three places — the chart README, [docs/deploy-gcp.md](./docs/deploy-gcp.md) and the
  `instanceConnectionName` guard's own comment — said a well-formed but wrong instance
  connection name is caught because "the proxy rejects it at startup". It is not: the proxy
  dials nothing unless asked to, and none of its three health endpoints dials either, so it
  reports itself up on an instance nobody can reach. The render guard is a shape filter, not
  a backstop, and the documents now say which layer actually catches what.
