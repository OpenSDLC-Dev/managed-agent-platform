- **`TestEnqueueNotifiesWorkChannelOnCommit` stops racing the broker's coverage-start
  wake** (#486). The listener wakes every subscriber once when LISTEN becomes active
  (`setReady` then `wakeAll`), and `Ready` can return between those two steps — so the
  test's non-blocking drain of that one-time wake could miss it, leaving it to arrive
  inside the 150 ms window where the test asserts that no wake precedes the enqueue's
  commit, reddening `internal/queue` under gate load. The drain now blocks for the
  coverage-start wake it is guaranteed to receive, so only the enqueue's own commit
  NOTIFY can follow it. No production code changed.
