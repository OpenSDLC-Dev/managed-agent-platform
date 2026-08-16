- **Plan 34 archived: the documentation is smaller than the system again** (#413) — Tracked
  markdown went 2,665,735 → 2,165,474 bytes (−19%), below the 2,382,801 bytes of non-test Go it
  documents, which was the plan's stated problem. The final slice cut nothing. `HISTORY.md` and
  `docs/history/` hold what a changelog structurally cannot — measurements, attacks that were
  tried, alternatives rejected — and its 414 KB → 120 KB target is retired as the fifth of nine
  to fail on measurement rather than the first to be met by deleting evidence. `HISTORY.md`'s
  provenance paragraph now tells the next trimmer why: the heading is not the unit of citation,
  since more citations quote body prose than name a heading, so preserving every heading would
  still orphan them. The five retirements share one cause, recorded in `docs/HISTORY.md` — each
  target came from a cluster's size without asking what the cluster must still hold, then was
  defended by an instrument narrower than the thing it tested. Two targets were beaten anyway
  (`ARCHITECTURE.md` by 71 KB, `changelog.d/`).
