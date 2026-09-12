- **The last six object deletes that orphaned on a store refusal now record what they owe**
  ([#703](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/703), which folds in
  #693) — deleting a file, closing or settling a dream, deleting a skill or one of its
  versions, and the harvest replacing a snapshot each removed their rows and then removed the
  objects best-effort. The rows were gone by then, and the ids were the objects' only names, so
  a store that refused or a process that died in that window left bytes no tier could
  enumerate. Each now enqueues its keys on the same transaction that removes the rows, for the
  sweeper plan 50 built — the shape `deleteSession` and the expired-file sweep already used, so
  no object a committed row named is orphaned by a store having a bad day any more. A skill cascade is the
  biggest beneficiary: it was N sequential deletes with nothing left to answer the client with,
  and a bad day orphaned every archive the skill had rather than the odd one. One class of
  object deliberately keeps the best-effort delete, under helpers renamed to say why: the one
  whose row **never committed** is owed to nothing, and queueing it would retry a delete past
  the cancellation that is all that protects a commit which may in fact have landed.
