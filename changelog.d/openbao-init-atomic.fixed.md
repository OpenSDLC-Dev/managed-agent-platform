- **A failed OpenBao init no longer leaves an init.json that lies about it** — both bundled
  init scripts, `deploy/compose/openbao-init.sh` and the chart's sidecar, created and
  truncated `init.json` before the `bao operator init` that fills it, so a failed init left a
  0-byte file behind. The branch that recovers a lost volume tested for a *missing* file,
  walked past the empty one, and the next run died on `no root_token` against a vault that was
  initialized and whose root token had never been captured — in-cluster, on every kubelet
  restart. The output now lands only once init has succeeded, and the recovery guard turns on
  the extracted token rather than on the file holding it: missing, empty and part-written all
  reach the diagnostic that names the volume. That last one matters because `bao` writes
  `root_token` last, so a truncated init.json usually cuts through the token itself. In the
  case #439 reported the volumes still have to be re-created; what changes is that the script
  says so instead of pointing at a file, and that a leftover `init.json.tmp` is named rather
  than swept — on the one path where it holds anything it is the only copy of the root token.
  Nothing executed either script before, so `make openbao-init-test` now drives both against a
  fake `bao` in CI, reverting each half of the fix to require its checks to go red. (#439)
