- **A killed test run no longer strands its Docker fixture forever** (#346). The four
  Dockerized fixtures (`pgtest`, `blobtest`, `secretstest`, `gcstest`) remove their
  per-test-binary container in a `defer` a killed process never reaches, so a Ctrl-C or
  a `go test` timeout stranded the container and its anonymous volume for good: one
  machine accumulated twenty strays holding 12.6 GB. Every fixture container now carries
  the label `dev.opensdlc.managed-agent-platform.test-fixture` naming its harness, and
  each fixture's `TestMain` force-removes labelled containers older than six hours, with
  `-v` so the volume goes too. Nothing external has to cooperate: a bare
  `go test ./internal/store` sweeps. The six-hour floor spares a sibling suite running
  concurrently, so a stray outlives the run that killed it and is reclaimed later.
