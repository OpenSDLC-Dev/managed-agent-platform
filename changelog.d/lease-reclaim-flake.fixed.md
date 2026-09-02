- **Two queue tests no longer race the 50 ms window they assert inside** (#537).
  `TestExpiredLeaseIsReclaimed` claimed a work item for 50 ms and then asserted a
  second claim found nothing yet; `TestPollReservesWithoutTransition` did the same
  with a 50 ms poll reservation. Each assertion holds only while its window is
  open, so a scheduling gap wider than 50 ms — one Postgres round trip on a
  contended runner — had the queue reclaim correctly and the test fail, reddening
  the `coverage` job on a commit carrying no Go at all. Both windows now widen to a
  minute, and the reclaim they go on to exercise is reached by backdating
  `lease_expires_at` in SQL rather than by sleeping the lease out, so no Go clock
  decides either transition. Test-only; no production code changed.
