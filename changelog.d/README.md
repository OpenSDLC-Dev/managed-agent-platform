# changelog.d/ — unreleased changelog fragments

Every PR that lands a notable change adds **one file per Keep-a-Changelog
group** here instead of editing CHANGELOG.md (which only a release PR touches,
via `make changelog` — see [docs/RELEASING.md](../docs/RELEASING.md)). Fragments
remove the top-of-file insertion race between parallel PRs: each PR adds its
own new file. (Two in-flight branches picking the same slug and section can
still collide add/add — rare, and resolved by renaming one.)

**Naming:** `<slug>.<section>.md` — `slug` is the branch short-name (letters,
digits, `_`, `-`; the PR number is unknown while the branch is under review, so
it goes in the entry text, not the filename), and `section` is one of `added` |
`changed` | `deprecated` | `removed` | `fixed` | `security`. A PR touching two
groups writes two files. Any other name fails the assembler loudly — a typo'd
section must not silently drop an entry from a release — with two exceptions:
this README, and dot-prefixed OS/editor droppings (`.DS_Store` and kin), which
are skipped since no slug can start with a dot.

**Content:** the file body is the final CHANGELOG.md entry **verbatim** — the
same `- **title** — narrative…` long-form style the changelog already uses,
starting with a `-` marker followed by a space and containing no `#` headings
(grouping is the assembler's job). Assembly is pure concatenation, so what you write here is byte-for-byte
what the release section will hold.

At release time `make changelog VERSION=X.Y.Z` folds every fragment into a
dated CHANGELOG.md section (groups in Keep-a-Changelog order, entries
newest-first by the commit that added them) and deletes the fragment files.
"What is unreleased right now" is simply `ls changelog.d/`.
