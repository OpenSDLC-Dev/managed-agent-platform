- **Binaries know their version** (plan 27 slice 2). New `internal/version`
  package — a single `Version` variable, `"dev"` unless the build injects it
  via `-ldflags -X` — wired into the Dockerfile as an `ARG VERSION=dev` on the
  shared build stage, so both the server image and the gate image stamp their
  binaries at release-build time. All five binaries log it on their existing
  startup line (`controlplane listening` / `brain running` / `executor
  running` / `worker running` / `gate listening` gain a `version` attribute),
  and the worker — the one binary users download and run standalone — answers
  `--version` (and `-version`) with the bare version string before touching
  any configuration. Deliberately no version API endpoint: that would be
  net-new wire surface (plan 27 decision 3).
