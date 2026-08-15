# changelog.d/ — unreleased changelog fragments

Every PR that lands a notable change adds **one file per Keep-a-Changelog
group** here instead of editing CHANGELOG.md (which only a release PR — plus
the post-release `make changelog-archive` move to docs/changelog/ — touches,
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
same `- **title** — narrative…` style the changelog already uses, starting with
a `-` marker followed by a space and containing no `#` headings (grouping is
the assembler's job). Assembly is pure concatenation, so what you write here is
byte-for-byte what the release section will hold.

**Size: hard cap 1,500 bytes** — one paragraph, around 200 words. That is the
ceiling, not the target: most changes say what they need in 60–120 words, and a
fragment near the cap should be carrying several operator contracts rather than
one long explanation. A fragment holds what a reader of the release notes needs
— what changed, what it means for them, and the issue or PR number. It is not
where a change is explained in full. Every longer form already has a mandated
home, and writing it here too means maintaining it in two places:

| the long form | its home |
| --- | --- |
| why the design is this way, what was rejected | the `docs/plan/` file |
| an acceptance run, a review-hardening record | [docs/HISTORY.md](../docs/HISTORY.md) |
| a wire-shape claim or an inference about the reference | [docs/DIVERGENCES.md](../docs/DIVERGENCES.md) |
| how the mechanism works | the comment beside the code |
| the forensics of a bug — the failing run, the false leads | the PR that landed it |

Keeping it short loses nothing: the PR body and `git log` hold the full account
permanently, and a released entry is frozen the moment `make changelog` folds
it in — so an over-long fragment is a paragraph the project maintains forever.

**Links:** a relative target must be written `](./…)` from the repo root, or
`](docs/…)`. Those are the only two forms the post-release
`make changelog-archive` can re-base when the section moves down to
docs/changelog/; every other relative form — `](../…)` and bare `](deploy/…)`
alike — fails the archive rather than being guessed at (`rebaseLinks` in
tools/changelog). Absolute URLs and `#anchor` targets are always fine. The
failure lands a release later than the mistake, so it is worth a glance now.

At release time `make changelog VERSION=X.Y.Z` folds every fragment into a
dated CHANGELOG.md section (groups in Keep-a-Changelog order, entries
newest-first by the commit that added them) and deletes the fragment files.
"What is unreleased right now" is simply `ls changelog.d/`.
