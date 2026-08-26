- **Memory stores are now covered where plan 36 said they would be and were not**
  (#488) — two rows the slice recorded as owed. The four memory instruments
  (`memory.materialized`, `memory.materialize.duration`, `memory.sync.actions`,
  `memory.sync.duration`) get the meter-reading test the skills, files and repos
  ones already had, so they cannot stop recording with every other memory test
  still green. And the whole path now runs through a real Docker container the
  agent does not own: a store materialized by the daemon, an unprivileged shell
  appending to one of its files in place with `>>` and creating another with
  `>`, and the run-end sync pushing both. That append is the case the 0666 mode
  exists for — set back to 0644 the new test fails with `Permission denied`,
  where nothing else in the suite noticed. The scenario needs a sandbox image
  that hands the uid the workdir, the shell state root and `/mnt`, which is what
  [docs/self-hosted-security.md](docs/self-hosted-security.md) §2 already asks
  of a non-root image; the test builds one rather than pretending
  `SANDBOX_RUN_AS_USER` alone is enough on this backend.
