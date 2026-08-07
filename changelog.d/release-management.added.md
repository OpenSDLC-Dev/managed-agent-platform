- **Release management lands — the fragment half** (plan 27 slice 1; the plan
  starts `in-progress`). Changelog entries move out of CHANGELOG.md's
  `[Unreleased]` section and into **`changelog.d/` fragments** — one file per
  PR per Keep-a-Changelog group, the body being the final entry verbatim (this
  entry is the first one) — so parallel PRs stop contending for the same
  top-of-file insertion point; only a release PR touches CHANGELOG.md, via the
  new `make changelog VERSION=X.Y.Z`. The assembler (`tools/changelog`, tested
  with mutation evidence) folds fragments into a dated section in KaC group
  order (entries newest-first by adding commit), moves any legacy
  `[Unreleased]` body byte-identically below them — the one-time affordance
  the 5,300-line backlog needs — leaves a pointer paragraph as the new
  `[Unreleased]` body, advances the Keep-a-Changelog link references, and
  refuses cleanly (empty release, existing version, malformed fragment names
  so a typo'd section cannot silently drop an entry) leaving both files and
  fragments untouched; `make changelog-notes` extracts a released section for
  the coming release workflow. **docs/RELEASING.md** is the new ritual:
  SemVer 0.x (Added/Changed → minor, fix-only → patch; 1.0 reserved for an
  explicit stability promise), plan-archive-driven timing, annotated
  `vX.Y.Z` tags, chart version/appVersion in lockstep — with the
  tag-triggered publishing pipeline explicitly marked as arriving in plan 27
  slice 3. CLAUDE.md/AGENTS.md step-2 wording and the verifier's
  docs-consistency rung now require the fragment instead of a direct
  CHANGELOG.md edit.
