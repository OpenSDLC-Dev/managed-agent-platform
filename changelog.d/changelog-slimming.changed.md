- **CHANGELOG.md slims to an index; released sections archive per release**
  (plan 28 slice 1). The new `archive` subcommand of `tools/changelog`
  (`make changelog-archive VERSION=X.Y.Z`) moves a released section to
  `docs/changelog/X.Y.Z.md` — re-basing its relative links for the new
  directory, byte-reversibly — and leaves an index stub — the exact dated
  heading over one line naming the section's Keep-a-Changelog groups and
  linking the archive file — so `latest`, the release tag-sanity check, and
  existing `CHANGELOG.md § [X.Y.Z]` citations keep resolving unchanged. The
  move is guarded: the archive must be exactly one section, the document
  must round-trip byte-for-byte, the written archive must invert to the
  moved section, an existing archive file is never clobbered (an interrupted
  run's byte-identical leftover converges the retry), and `notes` refuses to
  ship a stub as a release body (re-runs of a release workflow read the
  tag's checkout, where the section is still inline). Applied to 0.2.0 and
  0.1.0: CHANGELOG.md drops from 6,448 lines to a 35-line index at the cut,
  and the archiving step joins docs/RELEASING.md's ritual as its
  post-release step.
