- **Two agents messaging each other can no longer run forever** — #446 bounded one thread
  chaining its own turn, but that count lives on the claimed work item, whose life is exactly one
  uninterrupted run of chained turns. A ping-ponging pair escaped it three ways: the peer's wake
  inserts a fresh row taking the column default, restarting the count; a turn that woke anybody
  requeues with it cleared; and `chainInput` reads a peer's message as input, which beats the cap
  outright. The third is not in the issue — a pair that both stay `running` never wake each other
  at all, and still reset every turn — so closing the first two would have left the loop alive. A
  session now counts every turn that called a delegation tool in `sessions.delegation_turns`, on a
  row no wake replaces, and **refuses the claim** at 625 (`maxLiveThreads × maxSettlementChain`).
  Refusing rather than cutting is what keeps it safe: it never intervenes in a turn's fate, so it
  cannot idle a thread whose sandbox command is still in flight, and `send_to_agent` stays
  truthful because the peer really is woken — its own next claim is refused. A refused turn issues
  no model request. The thread idles `end_turn` carrying `session_delegation_exhausted_error`, and
  a message from outside returns the budget; a tool result deliberately does not, since on
  `self_hosted` every tool call arrives as one. Ours, not the reference's `budget`. (#447)
