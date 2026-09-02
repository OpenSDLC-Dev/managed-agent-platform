- **The memory-store sync suites stop asserting a version order the schema cannot
  guarantee** (#525). `versionsOf`, in both the executor's and the BYOC worker's memory
  tests, compared a path's version list in `ORDER BY created_at, id` order — but
  `memory_versions` carries no write-order key: `created_at` defaults to `now()`, which
  Postgres freezes at BEGIN, and the tiebreak behind it is a random `memver_` id. Both
  helpers now compare a sorted list, which still catches a wrong, missing, extra or
  duplicated version. Test-only; no production code changed.
