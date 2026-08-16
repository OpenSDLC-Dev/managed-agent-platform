- **Release notes link into the repository, not off the release page** — `make changelog-notes`
  copied the changelog section verbatim, so every repo-root-relative target in it resolved
  against the GitHub Release page and 404'd: 29 links across 12 documents in the v0.3.0 body,
  the first published without clamping. The archive path has re-based links for their new
  directory since plan 28, and the truncation trailer was already absolute for precisely this
  reason — the notes path alone had no equivalent. Both relative forms
  [changelog.d/README.md](./changelog.d/README.md) permits are now rewritten to
  `…/blob/vX.Y.Z/…` at the tag, anchors preserved and fenced examples left as written, while a
  relative form with no mapping fails the notes rather than publishing a dead link — the same
  bargain the archive re-basing already made. (#423)
