- **Memory stores are now covered where plan 36 said they would be and were not**
  (#488) — two rows the slice recorded as owed. The four memory instruments
  (`memory.materialized`, `memory.materialize.duration`, `memory.sync.actions`,
  `memory.sync.duration`) get the meter-reading test the skills, files and repos
  ones already had, so they cannot stop recording with every other memory test
  still green. And the whole path now runs through a real Docker container the
  agent does not own: a store materialized by the daemon, an unprivileged shell
  appending to one of its files in place with `>>` and creating another with
  `>`, and the run-end sync pushing both. Two rows already pinned the 0666
  mode's *value*, which a maintainer flipping it would update in lockstep; this
  is the first that shows why it has to be 0666 — set back to 0644 the append
  fails with `Permission denied`. The scenario needs a sandbox image that hands
  the uid the workdir, the shell state root and `/mnt`, which is what
  [docs/self-hosted-security.md](docs/self-hosted-security.md) §2 and §4 already
  ask of a non-root image; the test builds one rather than pretending
  `SANDBOX_RUN_AS_USER` alone is enough on this backend.
