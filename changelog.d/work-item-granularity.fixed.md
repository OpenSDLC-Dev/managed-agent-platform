- **The registry says when a work item is enqueued** — There the session itself is the work
  item, queued when it is created or when a long-dormant one receives a message, and the worker
  spawns an execution context per item and runs that session's tool calls. Here an item is
  created only by a commit that leaves a runnable sandbox tool call standing, so a session that
  never runs one never appears in `…/work` at all, and one that does appears first at its first
  confirmed sandbox call rather than at create. The payload is unchanged and an item's duration is its
  holder's business on both sides; only the trigger differs, argued from the brain owning the
  session while a worker holds neither a database handle nor a model credential. Of the five
  kinds sharing `work_items` only `tool_exec` reaches the wire — and the real `ant beta:worker`
  has settled this platform's items unmodified in every acceptance run that drove it. (#457)
- **`workAPIScope`'s comment names the hazards instead of counting the kinds** — It said two
  other row kinds share `work_items`; five do. The predicate was always right, so nothing was
  reachable that should not be, but a reader checking it against the comment would have read
  the scope as narrower than the table warrants. (#462)
