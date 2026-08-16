- **Plan 34 archived: the documentation is smaller than the system it documents** (#413) —
  Tracked markdown went 2,665,735 → 2,166,628 bytes (−19%), against 2,382,801 bytes of
  non-test Go. The final slice cut nothing. `docs/HISTORY.md` and `docs/history/` hold what a
  changelog structurally cannot — measurements, attacks that were tried, alternatives
  rejected — so their 414 KB → 120 KB target is retired rather than met by deleting evidence,
  the fourth of nine targets to fail on measurement. `HISTORY.md`'s provenance paragraph now
  carries what the next trimmer needs: that what sits under a cited heading is generally
  recorded nowhere else, and that the heading is not the unit of citation, since much of what
  points here quotes a sentence or a number instead of naming a section. Of the nine targets,
  four were retired, three missed after real cuts, and two met or beaten. Why each was
  retired, and the root cause they share, is in `docs/plan/34_doc-trim.md` and the plan's
  record in `docs/HISTORY.md`.
