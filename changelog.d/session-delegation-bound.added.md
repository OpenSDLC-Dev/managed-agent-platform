- **Two agents messaging each other can no longer loop forever on delegation alone** — #442
  bounded one thread chaining its own turn, but that count lives on the claimed work item, whose
  life is exactly one uninterrupted run of chained turns. A ping-ponging pair escaped it three
  ways: the peer's wake inserts a fresh row taking the column default, restarting the count; a
  turn that woke anybody requeues with it cleared; and `chainInput` reads a peer's message as
  input, which beats the cap outright. The third is not in the issue, and a pair that both stay
  `running` never wake each other at all — so closing the first two would have left it alive. A
  session now counts every turn that called a delegation tool in `sessions.delegation_turns`, on
  a row no wake replaces, and **refuses the claim** at 625 (`maxLiveThreads × maxSettlementChain`).
  Refusing rather than cutting keeps it safe: it never intervenes in a turn's fate, so it cannot
  idle a thread whose sandbox command is still in flight, and `send_to_agent` stays truthful —
  the peer really is woken, and its own next claim is refused. A refused turn issues no model
  request; the thread idles `end_turn` carrying `session_delegation_exhausted_error`. A message
  from outside returns the budget, a tool result does not — so a `self_hosted` pair emitting one
  real tool call per message is still unbounded, where a `cloud` one is not. Ours, not the
  reference's `budget`. (#447)
