- **A second compose stack no longer joins the first** — `deploy/compose/docker-compose.yml`
  pinned its default network's name so the executor's `SANDBOX_DOCKER_GATE_NETWORK` could name
  it verbatim, and so `docker compose -p other up` did not isolate: the second stack landed on
  the running one's network, where `postgres`, `openbao` and `minio` resolved to whichever
  container answered. Not theoretical — a branch stack's brain migrated a running stack's
  database, because every binary migrates at startup. The gate network and the two `:local`
  image tags now derive from the compose project name, which Compose resolves before it
  interpolates, so the default project renders unchanged and a stack already up under it is
  adopted rather than recreated, while `-p <name>` gives a second stack its own network,
  containers, volumes and images. CI renders the file under two project names to keep it that
  way. Host ports are what a project name still does not separate, and the compose README says
  so. Separately, the silence that let the damage through is closed — a process with migrations
  to apply now names the database, the server address that actually answered and the host it
  was configured with, and each version, before the first statement runs; finding nothing to
  apply stays silent. (#438)
