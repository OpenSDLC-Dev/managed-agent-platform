- **A killed test run no longer strands its Docker fixture forever** (#346). The four
  Dockerized fixtures — `pgtest`, `blobtest`, `secretstest`, `gcstest` — start one
  container per test binary and remove it in a `defer`, and a `defer` does not run when
  the process is killed: Ctrl-C, an IDE's stop button, `go test`'s own timeout panic, a
  laptop suspending mid-run. The container kept running and its anonymous volume kept
  growing, with nothing on the machine that would ever remove it — the fixtures shell out
  to `docker` rather than going through testcontainers, so there is no ryuk sidecar to
  notice. It compounds: eleven package binaries use `pgtest`, so one interrupted
  `go test ./...` stranded up to eleven containers, and one container is not one database
  (`NewPool`/`FreshDB` mint a fresh migrated database per test), which is why the observed
  strays held 1.2–2.2 GB each and 12.6 GB in total.
  Every fixture container is now started through `internal/dockertest`, which labels it
  with the harness that owns it and the time it started, and every fixture's `TestMain`
  opens by force-removing labelled containers older than six hours — with `-v`, so the
  anonymous volume goes with them. The next run cleans up after the killed one, and no
  wrapper, environment variable or external reaper has to cooperate: a bare
  `go test ./internal/store` sweeps too.
  Age is what separates a corpse from a peer. `go test ./...` runs those binaries
  concurrently, so removing every labelled container on sight would destroy a sibling
  suite's live database mid-run — a disk-space annoyance traded for a flaky suite. Six
  hours is three times the longest run the repo sanctions (`make eval` gives its binaries
  `-timeout 120m`, and they use `pgtest`); the price is that a stray outlives the run that
  killed it, but it is reclaimed by a later run rather than by hand. Both properties the
  fixtures deliberately had are preserved: `pgtest`, `blobtest` and `secretstest` still
  refuse `--rm`, so a crashed container still yields `containerDiag`'s state and logs, and
  every removal still passes `-v`. `gcstest` keeps the `--rm` it always had, which was
  never cover for this: `--rm` removes a container that exits, and an abandoned fake
  never does.
