- **A second compose stack no longer joins the first** — `deploy/compose/docker-compose.yml`
  pinned its default network's name so the executor's `SANDBOX_DOCKER_GATE_NETWORK` could name
  it verbatim, and so `docker compose -p other up` did not isolate: the second stack landed on
  the running one's network, where `postgres`, `openbao` and `minio` resolved to whichever
  container answered. Not theoretical — a branch stack's brain migrated a running stack's
  database, because every binary migrates at startup. The gate network is now derived from the
  compose project name, which Compose resolves before it interpolates, so the default stack's
  network name is unchanged, an already-running stack is adopted rather than recreated, and
  `-p <name>` alone brings up an isolated second stack. CI renders the file under a second
  project to keep it that way, and the compose README says what still crosses: the published
  API port, and the `:local` image tags both checkouts build into. Separately, the silence that
  let the damage through is closed — a process with migrations to apply now names the host,
  port and database it is about to change, and each version, before the first statement runs;
  finding nothing to apply stays silent. (#438)
