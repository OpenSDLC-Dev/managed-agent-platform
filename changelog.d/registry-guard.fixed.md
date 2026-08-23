- **Every shared pointer in the registry says what its own entry leaves open** — 33 pointers in
  [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) named a tracker that several entries share — #78,
  the recording tracker, above all — and so told a reader nothing about the entry citing them.
  #453 had given 51 of their siblings a parenthetical and left these on no principle but the
  accident of which trackers happened to close; each now carries one, written from its own entry.
  Five turned out to be worse than bare. #56 was "Console SSO and RBAC" when they were written
  and is now "Multi-tenant activation (post-v1)", which settles none of them: three record
  shipped divergences with nothing outstanding at all and become provenance, and two are waiting
  on a recording and re-point at #78. An issue whose scope moves out from under a pointer is a
  second kind of rot, and no issue-state check can see it — only writing the parenthetical does.
  (#452)
