- **A falsified provenance claim, in all three places it was asserted** (#413) — The archive
  header of `docs/history/2026-07.md`, the unreleased `history-split` changelog fragment, and
  `docs/HISTORY.md`'s own plan-28 record each promised that the archive and `HISTORY.md` still
  recompose to the pre-split file byte-for-byte. Slice 3 of this plan had edited that archive,
  so none of the three was true; all now scope byte-reversibility to the move rather than
  asserting it as a standing invariant. The third copy was found only by review, after the
  first two were fixed — grep for the claim, not for the copies you remember. Separately, a
  quoted sentence about the docker wrapper's marker file had drifted:
  `internal/sandbox/docker/api_test.go` cited the archive while quoting `HISTORY.md`'s
  version, which had merged two different archive sentences inside one set of quotation
  marks. Both now quote one archive clause exactly and attribute the `/tmp` detail to the
  separate sentence it comes from. Two archived decisions that plan 29 has since reversed —
  confirmation gating scoped to `agent.tool_use`, and `SandboxProvider` having no `Attach` —
  carry dated notes rather than edits: an overturned decision is what an archive is for.
