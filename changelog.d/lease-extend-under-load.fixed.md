- **Two lease-keeper tests no longer abandon a healthy turn on a loaded fixture
  Postgres** (#483). The keeper bounds each `Extend` by what the lease has left, so the
  brain's `TestLongTimeToFirstTokenKeepsLease` (250 ms lease) and the executor's
  `TestLeaseRenewedDuringSlowProvision` (300 ms) left a sub-200 ms budget that one slow
  UPDATE on a contended fixture could overrun — reddening the `coverage` job on commits
  carrying no relevant Go. Both leases are now 1500 ms, matching the keeper-budget tests
  whose `Extend` tolerates more than a second of contention, with the tests' timing
  scaled to still exercise the keeper's renewal. Production's 2-minute lease TTL and the
  keeper's deliberate abandon-on-timeout are unchanged; this is a test-timing fix.
