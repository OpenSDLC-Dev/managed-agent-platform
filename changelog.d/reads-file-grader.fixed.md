- **The `reads-file` eval grader recognizes every shape a real read takes**.
  It accepted only a `read` of the exact mount path or a `bash` command carrying
  the whole path as a substring, so a read the toolset admits — a
  workdir-relative `read` path, a `grep` rooted at the file or an ancestor
  directory, a `cd` and `cat` split across two bash calls by the persistent
  shell — reddened the trial as "the agent never read the mounted file". The
  2026-08-19 nightly's `repo-answer` failed exactly that way with the correct
  passphrase in its final answer. The grader now mirrors the toolset's
  path-resolution rule for `read`, counts a `grep` whose search root covers the
  file (`glob` still does not count — it returns names, no bytes), matches
  `bash` on the file's basename, and — as `writes-file` already required —
  counts only calls whose result succeeded, so a failed `cat` no longer passes
  for a read.
