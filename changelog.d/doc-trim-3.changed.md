- **ARCHITECTURE.md's package reference is a map now, not a second copy of the code** (#413) — the
  per-file tables were 298,654 of the file's 325,163 bytes, and they were the derivative: every
  package carries more comment than the document spent on it, most by three to ten times
  (`internal/api` 192 KB against 50 KB, `internal/executor` 169 KB against 43 KB). Those comments
  sit beside the code and cannot drift from it; the tables did, and one still spelled the gate
  token `gatetok_` where the constant has always been `gtk_`. What replaces them is each package's
  place in the flow — the one thing a package doc cannot hold, because it is about the neighbours —
  and a pointer to read the package. Nothing was deleted unchecked: of about a hundred issue
  references in the tables only four were absent from the source, three of those pure provenance
  and the fourth an open issue; every identifier was checked against the code; and the citation
  chain a 2026-07 note warned about turned out not to exist, since `DIVERGENCES.md` cites
  ARCHITECTURE nowhere.
