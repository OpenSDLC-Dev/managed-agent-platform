- **Four false statements in the delivery record, two of them written by this plan** (#413) —
  `docs/history/2026-07.md`'s header and the unreleased `history-split` changelog fragment both
  promised the archive still recomposes with `docs/HISTORY.md` byte-for-byte. Slice 3 of this
  plan had edited that archive, so neither was true; both now say byte-reversibility was a
  property of the split, not a standing invariant, and name `docs/changelog/` as the frozen kind.
  A quoted sentence about the docker wrapper's `/tmp` marker had drifted across three copies —
  `internal/sandbox/docker/api_test.go` cited the archive while quoting HISTORY.md's re-punctuated
  version, which had itself merged two separate archive sentences inside one set of quotation
  marks; both now quote the source exactly. Two archived decisions that plan 29 has since
  reversed — confirmation gating scoped to `agent.tool_use`, and `SandboxProvider` having no
  `Attach` — carry dated notes in the existing "Since closed" style rather than being edited: an
  overturned decision is what an archive is for, and `Attach` returned on the session id, so the
  reason recorded against it survives its own reversal.
