- **Changelog fragments have a size** — `changelog.d/README.md` now sets one: 60–120 words,
  hard cap 1,500 bytes. A fragment carries what a release-notes reader needs; the longer forms
  have homes already — the `docs/plan/` file for a decision, [docs/HISTORY.md](./docs/HISTORY.md)
  for an acceptance record, [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) for a wire claim, the
  comment beside the code for a mechanism, and the PR itself for a bug's forensics. Every
  unreleased fragment over the cap was recut to it, which is the last moment they can be: at the
  next `make changelog` they fold into CHANGELOG.md and are frozen. First slice of
  [plan 34](./docs/plan/34_doc-trim.md) (#413).
