- **The answer-style eval trials stop saying "secret"** — the five trials that
  plant a passphrase behind a mount, a skill, an MCP tool or a memory store
  now ask for "this task's passphrase" instead of "this task's secret
  passphrase", in the prompts and in the planted fixture text alike. Measured
  before landing, against the same endpoint on the same day: the "secret"
  wording failed 10 of 31 attempts across the three locally-runnable trials —
  refusals, denials that the tool exists, fabricated answers — and the
  identical prompts without it failed 0 of 24. This is the other axis from the
  2026-08-12 measurement that kept the plain ask (recorded beside
  `repoAnswer`): the ask stays plain, only the trigger word goes. The
  repo-answer fixture repository's own file text is operator-owned and
  unchanged; graders are wording-independent and untouched.
