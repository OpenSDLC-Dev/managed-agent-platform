# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#128](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/128) — the anthropic adapter's
`message_start` branch copied three of the four usage counters and dropped `output_tokens`, so an
endpoint that reports its whole reading up front and closes with a stop-reason-only `message_delta`
was recorded as having produced zero output tokens. No plan file: single-PR scope, one behavior
change plus its contract tests, no wire-schema question.

## Tasks

- [x] Contract test in both adapters for a reading that arrives before the closing frame —
      `message_start`-only usage (anthropic) and an early usage frame (openai). The anthropic one
      red on `main` with `output_tokens: 0`; the openai one passed unchanged and stays as a
      regression guard.
- [x] `message_start` seeds `OutputTokens` alongside the other three counters; `message_delta` still
      overrides, and its `> 0` guard keeps a sparse closing frame from zeroing the seed.
- [ ] Verifier PASS.
- [ ] Reviews: Codex reviewer and the Claude-side reviewer.
- [ ] PR green on CI.

**Coordination:** [#130](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/130) (#90) is
open and edits the same `message_start` block, wrapping it in a `reportedUsage` check. Whichever
lands second puts the `OutputTokens` line inside that check; this file and CHANGELOG.md reconcile
by keeping both entries.
