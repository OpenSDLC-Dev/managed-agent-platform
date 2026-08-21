- **A failed OpenBao init no longer leaves an init.json that lies about it** — both bundled
  init scripts, `deploy/compose/openbao-init.sh` and the chart's sidecar, created and
  truncated `init.json` before the `bao operator init` that fills it, so a failed init left a
  0-byte file behind. The branch that recovers a lost volume tested for a *missing* file,
  walked past the empty one, and the next run died on `no root_token` against a vault that was
  initialized and whose root token had never been captured — in-cluster, on every kubelet
  restart. The output now lands only once init has succeeded, and the recovery guard turns on
  the extracted token rather than on the file holding it, so every unusable `init.json`
  reaches the diagnostic that names the volume. In the case #439 reported the volumes still
  have to be re-created; what changes is that the script says so instead of pointing at a
  file. The chart's remedy line, which named a PVC that cannot be the problem, is corrected
  with it. `make openbao-init-test` now runs both scripts against a fake `bao` in CI, which
  nothing did before. (#439)
