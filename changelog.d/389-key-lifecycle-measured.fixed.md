- **The management-key lifecycle now matches the measured reference** (#389). Probed against the
  live console, three of five recorded inferences went against us and two more divergences
  nobody had suspected surfaced. A repeated archive is refused with 400 instead
  of succeeding as a no-op: nothing may be patched onto an archived row, an empty body included.
  A key disabled and then left to lapse renders `expired`, not `inactive`; the precedence is
  `archived` > `expired` > the stored status. A lapsed key admits archiving alone — disabling and
  renaming it are now refused. A past `expires_at` is accepted at issue, minting a key born
  `expired`, previously rejected. An empty patch on a live key answers 200 with the unchanged
  resource. Operator rules: docs/self-hosted-security.md §10; the differing refusal wording is
  registered in docs/DIVERGENCES.md.
