- **The `reads-file` eval grader recognizes reads its old rule refused** — it
  accepted only a `read` of the exact mount path or a `bash` command carrying
  the whole path as a substring, so reads the toolset admits — a
  workdir-relative `read` path, a `grep` rooted over the file, a `cd` and
  `cat` split across two bash calls by the persistent shell — reddened the
  trial as "the agent never read the mounted file"; the 2026-08-19 nightly's
  `repo-answer` failed exactly that way with the correct passphrase in its
  final answer. The grader now mirrors the toolset's path-resolution rule for
  `read`, counts a `grep` whose search root covers the file and whose result
  carried matches (`glob` still does not count — names, no bytes), matches
  `bash` on the file's basename — deliberately loose, cwd-blind evidence, with
  each trial's answer grader load-bearing — and counts only calls whose result
  succeeded, so a failed `cat` no longer passes for a read. (#526)
