- **`TestKeySetFetchDeadline` stops racing the deadline it exists to test**
  (#381, #422). The test drives the one branch a fake clock cannot reach — a real
  context deadline on a key-set fetch — and then asserted that the fake provider
  had *served* a second request. But a fetch its 50 ms deadline kills before the
  server accepts it never reaches that handler, and that is the deadline working
  rather than failing: the assertion required a request designed to lose a race
  to win it first, so under CI load it reddened the `coverage` job on diffs
  containing no Go at all. The fetches are now watched at the client, where the
  outcome is not a race — the attempt is recorded whichever phase the deadline
  interrupts, and `context.DeadlineExceeded` is what the test pins. No production
  code changed.
