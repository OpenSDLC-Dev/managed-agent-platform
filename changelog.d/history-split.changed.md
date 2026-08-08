- **docs/HISTORY.md splits by period** (plan 28 slice 2, archiving the
  plan). The 31 sections whose events belong to 2026-07 — 1,327 lines —
  move to `docs/history/2026-07.md`, relative links re-based for the new
  directory the way `changelog-archive` does it, byte-reversibly: a
  recomposition from the two output files reproduces the pre-split file
  byte-for-byte. HISTORY.md keeps its intro, the Delivery-slices table,
  and the current month over a pointer paragraph naming the archives; 96
  references to moved sections re-point (69 citations in
  docs/DIVERGENCES.md, the rest across archived plan files,
  docs/ARCHITECTURE.md, README.md and one Go comment), while references
  inside archives themselves — the byte-frozen `docs/changelog/` files and
  a `docs/history/` file recording its own era — resolve through
  HISTORY.md's pointer instead.
