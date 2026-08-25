- **`TestLongTimeToFirstTokenKeepsLease` no longer abandons a healthy turn on a
  loaded fixture Postgres** (#483). The lease keeper bounds each `Extend` by what the
  lease has left, so the test's 250 ms lease left a sub-200 ms budget that one slow
  UPDATE on a contended fixture could overrun — reddening the `coverage` job on a
  docs-only commit. The lease is now 1500 ms, matching the keeper-budget tests whose
  `Extend` tolerates more than a second of contention, with the model's simulated
  think time scaled to still outlast the whole lease. Production's 2-minute lease TTL
  and the keeper's deliberate abandon-on-timeout are unchanged; this is a test-timing
  fix.
