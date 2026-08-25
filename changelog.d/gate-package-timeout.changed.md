- **The merge gate's test step now allows 30 minutes per package instead of `go test`'s
  10-minute default** (#490). The two largest Postgres-backed suites, `internal/api` and
  `internal/executor`, now run about ten minutes each on a loaded 8-CPU box — the
  executor measured past the default at 613 s, the api just under at 591 s — and a
  package killed at the ceiling reports `panic: test timed out` with a sub-second test
  "running", which reads like a hang rather than the budget it is. The default is sized
  for unit tests; these provision containers and migrate a database per test, and their
  per-test guards are the real limits. The change also breaks a feedback loop: a
  timed-out binary skips the `defer` that removes its pgtest fixture, so every ceiling
  hit left idle containers to slow the next run. A genuinely wedged suite still fails,
  three times slower to say so.
