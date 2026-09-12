- **The last six object deletes that orphaned on a store refusal now record what they owe**
  ([#703](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/703), which folds in
  #693) — deleting a file, closing or settling a dream, deleting a skill or one of its
  versions, and the harvest replacing a snapshot each removed their rows and then removed the
  objects best-effort. The rows were gone by then, and the ids were the objects' only names, so
  a store that refused or a process that died in that window left bytes no tier could
  enumerate. Each now enqueues its keys on the same transaction that removes the rows, for the
  sweeper plan 50 built — the shape `deleteSession` and the expired-file sweep already used, so
  "rare orphans accepted" is no longer a property of this platform. A skill cascade is the
  biggest beneficiary: it was N sequential deletes with nothing left to answer the client with,
  and a bad day orphaned every archive the skill had rather than the odd one. Two things
  deliberately keep the best-effort delete, under helpers renamed to say why: an object whose
  row **never committed** must not be queued, because an ambiguous commit failure is exactly
  when a possibly-live object has to be preserved rather than retried into deletion.
