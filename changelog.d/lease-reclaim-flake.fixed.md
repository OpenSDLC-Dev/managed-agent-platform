- **Two queue tests no longer race the 50 ms window they assert inside** (#537).
  `TestExpiredLeaseIsReclaimed` claimed a work item for 50 ms and then asserted a
  second claim found nothing yet; `TestPollReservesWithoutTransition` did the same
  with a 50 ms poll reservation. Each assertion holds only while its window is
  open, so a scheduling gap wider than 50 ms — one Postgres round trip on a
  contended runner — had the queue reclaim correctly and the test fail, reddening
  the `coverage` job on a commit carrying no Go at all. Expiry now comes off the
  database clock, the way `internal/queue/lifecycle_test.go` already takes it:
  both windows widen to a minute, and a helper backdates `lease_expires_at` by SQL
  where the tests used to sleep, so no wall clock decides either outcome. The
  helper returns the new lease for the claimant to hold, which keeps the assertion
  that a reclaimed item's original holder has lost its proof turning on the rival
  claim rather than on the test's own write. No production code changed, and the
  three sibling tests that also take a 50 ms lease are left alone: none of them
  asserts a negative that holds only while a short lease is still alive, so delay
  can only make their lapse more certain.
