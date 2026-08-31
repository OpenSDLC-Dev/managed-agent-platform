- **journal-multiturn's file check forgives blank lines** — the 2026-09-01
  manual eval run red the trial's retry on `printf '\nentry-two\n' >>` against
  a file already ending in a newline: an empty line between entries, with every
  platform signal (first line unchanged, second below it) intact. The trial now
  grades with `FileLinesIgnoringBlanks`, a variant scoped to files the model
  assembles by appending across turns; `FileLines` itself stays exact, so no
  single-write trial loosens. `perm-deny`'s untouched-file assertion moves the
  other way, to the byte-exact `FileEquals` its comment always claimed —
  "unchanged" is not a place for forgiveness. (#533)
