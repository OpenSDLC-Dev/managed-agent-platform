- **A failed OpenBao init no longer leaves an init.json that lies about it** — both bundled
  init scripts, `deploy/compose/openbao-init.sh` and the chart's sidecar, created and
  truncated `init.json` before the `bao operator init` that fills it, so a failed init left a
  0-byte file behind. The branch that recovers a lost volume tested for a *missing* file,
  walked past the empty one, and the next run died on `no root_token` against a vault that
  was initialized and whose root token had never been captured — in-cluster, on every kubelet
  restart. The output now lands only once init has succeeded, and the recovery branch tests
  for the token rather than for the file, so missing, empty and half-written all reach the
  diagnostic that names the volume — on stacks already stuck this way too. In the case #439
  reported the volumes still have to be re-created; what changes is that the script says so
  instead of pointing at a file, and that a leftover `init.json.tmp` is named rather than
  swept, since on the one path where it holds anything it is the only copy of the root token.
  The happy path is unchanged. Nothing executed either script before — CI's compose job never
  starts openbao and the helm job only renders the chart — so `make openbao-init-test` now
  drives both against a fake `bao`, reverting each half of the fix to require the checks
  guarding it to go red. (#439)
