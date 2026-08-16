- **Release notes link into the repository, not off the release page** — `make changelog-notes`
  copied the changelog section verbatim, so every repo-root-relative target in it would have
  resolved against the GitHub Release page and 404'd. The v0.3.0 body carries 30 such links
  across 12 documents and is the first rendered without clamping; it is also the first tagged
  past this fix, so it publishes them absolute and never exhibited the defect. The archive path
  had re-based links for their new directory since plan 28, and the truncation trailer was
  already absolute for precisely this reason — the notes path alone had no equivalent. Both
  relative forms [changelog.d/README.md](./changelog.d/README.md) permits are now rewritten to
  `…/blob/vX.Y.Z/…` at the tag, link-reference definitions included, anchors preserved and
  fenced examples left as written, while a form with no mapping — a `..` segment among them —
  fails the notes rather than publishing a dead link. That failure lands *after* the tag is
  pushed, so link forms are worth a glance while the fragment is still in review. (#423)
