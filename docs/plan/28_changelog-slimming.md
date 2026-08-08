---
status: archived
---

# Plan 28 — changelog and history slimming

**Archived 2026-08-08 — completed.** Slice 1 landed in #337, slice 2 in the
archiving PR. One acceptance figure landed off its estimate: the split kept 1,823 of
HISTORY.md's 3,150 lines in place, far above the ~1,000 target — 2026-08
alone is the project's heaviest month, and the figure falls as it
archives in turn. The delivery
record is docs/HISTORY.md § "Changelog and history slimming (plan 28)".

## Why

The v0.2.0 cut left CHANGELOG.md at 6,448 lines (~510 KB): `§ [0.2.0]` alone
is 5,507 lines, because the first cut under the fragment scheme deliberately
absorbed the legacy `[Unreleased]` backlog byte-identically (plan 27,
decision 4). docs/HISTORY.md is past 3,000 lines on the same trajectory —
every acceptance run, review-hardening record, and archived plan appends to
it. Both files grow without bound; both are read mostly at their heads. The
user asked for the slimming on 2026-08-08, choosing the shape below.

## Decisions

1. **Per-release archive files, everything released.** Each dated section of
   CHANGELOG.md moves to `docs/changelog/<X.Y.Z>.md` — byte-reversibly, not
   byte-verbatim: relative link targets are re-based for the new directory
   (`](./` two levels up, the bare `](docs/` form one; anything else relative
   is refused until taught), because a verbatim copy would break every one of
   them from `docs/changelog/`. CHANGELOG.md keeps the preamble, the
   `[Unreleased]` pointer paragraph, and one **index stub** per release: the
   exact dated heading (`## [X.Y.Z] - YYYY-MM-DD` — the grammar `latest` and
   `release-tag-check` parse stays untouched in the file they already read)
   followed by a one-line body naming the group summary and linking the
   archive file. The trailing link-reference block stays in CHANGELOG.md —
   section bodies use inline links only, so the archive file needs no
   reference definitions; its own dated heading's brackets render literally
   there, as raw Keep a Changelog does. Rejected: a single archive file — it
   regrows the original problem; keeping only the latest release inline — the
   5,507-line `§ [0.2.0]` *is* the latest, so nothing would slim today.
2. **Mechanized, not manual.** A new `archive` subcommand on
   `tools/changelog` performs the move: extract the section, write the
   archive file, replace the body with the stub, and refuse to write anything
   unless the document round-trips (stub → section restores it byte-for-byte)
   **and** the archive file's bytes invert to the moved section under the
   inverse link rewrite. `make changelog-archive VERSION=X.Y.Z` wraps it.
   Rejected: a documented manual step — a 5,500-line move performed by hand
   is exactly where bytes go missing.
3. **Ritual placement.** Archiving a release's section is a **post-release**
   step: the section must be in CHANGELOG.md at tag time (`changelog-notes`
   extracts it there), so the archive lands in the next docs PR after the tag
   run — normally the same archiving PR that closes the release's plan.
   `notes -version X` for an already-archived version fails with a clear
   error naming the archive file; re-runs of a release workflow read the
   tag's own checkout, where the section is still inline.
4. **The stub preserves anchors — no citation sweep at archive time.** A
   `CHANGELOG.md § [X.Y.Z]` citation keeps resolving after the move: the stub
   carries the same heading and names the archive file, so the reader is one
   explicit hop from the narrative (unlike the release-time `[Unreleased]`
   move, whose pointer paragraph does not name where a specific section
   went — those citations do re-point, per RELEASING.md step 6). New
   citations may target the archive file directly. GitHub Release trailers
   are tag-pinned (`blob/vX.Y.Z/CHANGELOG.md`) and never re-pointed.
   Rejected: re-pointing every citation at every archive — a repo-wide churn
   per release for anchors that were never broken.
5. **HISTORY splits by period.** docs/HISTORY.md keeps its intro, the
   Delivery-slices table, and the sections of the current month; older
   sections move to `docs/history/<YYYY-MM>.md` by the date each section's
   heading carries — decision 1's lesson applies here too: relative links
   re-based for the new directory (`](../` up one more, then `](./` to
   `](../`), byte-reversibly — with a pointer paragraph in HISTORY.md naming
   the archive files. Section-level citations from other docs re-point in
   the same PR, except inside archives themselves — `docs/changelog/`
   (bytes frozen by the inversion guard) and prior `docs/history/` files —
   whose text records its era as written; readers there resolve through
   HISTORY.md's pointer.
   (Manual move with a byte-identity check in the PR — HISTORY has no
   parsing tool to extend, and building one for a two-file split is exactly
   the speculative tooling CLAUDE.md's simplicity rule forbids.)

## Slices

1. **The `archive` subcommand + the CHANGELOG split.** TDD: extraction/stub/
   round-trip contract tests first (mutation-check the lossless guard — a
   mutant that drops the round-trip comparison must go red). Make target,
   RELEASING.md post-release step, CLAUDE.md/AGENTS.md wording where they
   describe CHANGELOG.md's shape. Run it for 0.2.0 and 0.1.0; changelog
   fragment.
2. **The HISTORY split.** Move pre-current-month sections to
   `docs/history/<YYYY-MM>.md`; pointer paragraph; citation sweep; byte-count
   check recorded in the PR; changelog fragment. Archive this plan.

## Acceptance

`make verify` green; `go run ./tools/changelog latest` still answers from the
slimmed CHANGELOG.md; the real move re-composes to the pre-move document
byte-for-byte under the inverse link rewrite; every `CHANGELOG.md § [X.Y.Z]`
citation still resolves through the stub's onward link; CHANGELOG.md lands
under ~60 lines and HISTORY.md under ~1,000.
