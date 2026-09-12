- **A backlog of expired files drains in one pass, and an open dream's transcript is never
  swept** — the expired-file sweep took one bounded batch per hour, a ceiling that existed to
  cap how many object deletes a single tick owed. It owes none since it started recording its
  debt instead of paying it, so the batch now bounds a transaction rather than an hour and a
  tick keeps going until a batch comes back short: a registry with 50,000 expired rows clears
  on the next tick instead of over the following two days, during which its metadata outlived
  the window the reference publishes. The sweep also refuses a file an open dream owns, which
  is the refusal the manual delete route already makes — nothing stamps an expiry on a
  transcript today, but that was a fact about the current writers rather than an invariant,
  and the cost of it changing was a transcript removed from under a runner still appending to
  it. A closed dream's transcript stays ordinary. One log key is corrected: the success line
  said `expired_before` and carried a duration, where its sibling sweep says `older_than`
  (#698).
