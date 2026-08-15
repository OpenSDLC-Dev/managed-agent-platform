- **A wedged work item is ended and handed back, not held forever** (#383). A sandbox
  call that never returned (the Kubernetes exec stalls in CI, #318) kept its lease
  renewing, so the item stayed `active`, unreclaimable by any other executor, and crash
  recovery never fired. The executor and the BYOC worker now bound an item's *silence*,
  not its runtime: a holder that reports no finished step for `EXECUTOR_STALL_TIMEOUT` /
  `WORKER_STALL_TIMEOUT` (default 30m) has its work cancelled and its lease left to
  lapse, while results it already answered are committed best-effort, so a tool usually does
  not re-run — but a settlement slower than the lease's remainder, or a call that ignores
  cancellation, still commits nothing, and an outputs harvest always discards its staged bytes
  rather than commit half a snapshot. Both refuse
  a budget below the longest single step plus a minute (11m; more when
  `EXECUTOR_REPO_CLONE_TIMEOUT` is raised). Recovery still needs a second replica (#396);
  bounding the call itself is #395.
