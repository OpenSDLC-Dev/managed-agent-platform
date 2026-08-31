- **The answer-style eval trials drop the word "secret" from their prompts** —
  the five trials that plant a passphrase behind a mount, a skill, an MCP tool
  or a memory store now ask for "this task's passphrase" rather than "this
  task's secret passphrase" (`repo-answer`, which never used that phrase,
  instead stops describing its fixture file as holding a "secret passphrase"),
  and the four fixtures this repo plants lose the word from their planted text
  too; `repo-answer`'s fixture repository is operator-owned and unchanged, and
  the skill trial's `eval-secret` identifier stays deliberately — the measured
  variant kept it. Measured before landing, against the same endpoint on the
  same day: the "secret" wording failed 10 of 31 attempts across
  `file-answer`, `skill-answer` and `mcp-answer` — refusals, denials that the
  tool exists, fabricated answers — while the identical prompts without it
  failed 0 of 24; `memory-recall` and `repo-answer` take the change by
  analogy, unmeasured. The 2026-08-12 measurement recorded beside `repoAnswer`
  moved the ask's shape as well as the word; this change moves the word alone.
  Graders are wording-independent and untouched. (#530)
