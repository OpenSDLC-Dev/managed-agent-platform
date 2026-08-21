- **`docs/DIVERGENCES.md`'s `Tracked:` pointers, and CLAUDE.md's post-v1 list, stop naming
  closed issues** — 65 of the registry's 111 pointers cited an issue that had since closed, so a
  reader following one landed on delivered work and learned nothing about what is still open.
  Five already carried a provenance annotation and were right; the other 60 are repaired, and
  the two failure modes differently. In the CONFIRMED section a closed tracker is provenance and
  now says so. An INFERRED entry whose tracker closed was orphaned, and re-points at an open
  tracker — #78, the recording tracker, in almost every case — with a parenthetical naming what
  that entry still leaves open, and the closed issue demoted to a `landed for #N` clause; the
  one entry no recording can settle went to #450 instead, and two CONFIRMED entries took the
  same re-point because their prose still named a live question their pointer did not. The
  file's Format legend now states the rule in writing, so the next contributor inherits it:
  `Tracked:` names an **open** issue, and a closed one may appear only as provenance.
  CLAUDE.md's enumeration `#50–#57 (+ #77)` — five of the nine already closed, and blind to
  #261, a post-v1 deferral filed outside the range — gives way to the `(post-v1)` title marker
  every such issue carries, and a bounded query for it. (#445)
