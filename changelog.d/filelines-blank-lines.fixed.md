- **The `FileLines` eval grader forgives blank lines** — `printf '\nentry\n' >>`
  against a file that already ends in a newline leaves an empty line between
  entries, and the 2026-09-01 manual eval run red `journal-multiturn`'s retry on
  exactly that shape with every platform signal intact: the first line was
  unchanged and the second sat below it, so the empty line carried model append
  hygiene, not platform information. Content stays exact — whitespace-only
  lines, stray prose, reordering and missing lines all still fail. `perm-deny`'s
  untouched-file assertion moves to `FileEquals`, whose byte-for-byte claim is
  what that trial's comment always said it made — "unchanged" is not a place for
  forgiveness.
