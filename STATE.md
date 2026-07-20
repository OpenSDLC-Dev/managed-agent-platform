# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#128](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/128) — the anthropic adapter's
`message_start` branch copied three of the four usage counters and dropped `output_tokens`, so an
endpoint that reports its whole reading up front and closes with a stop-reason-only `message_delta`
was recorded as having produced zero output tokens. No plan file: single-PR scope.

## Tasks

- [x] Contract test in both adapters for a reading that arrives before the closing frame. The
      anthropic one red on `main` with `output_tokens: 0`; the openai one passed unchanged and
      stays as a regression guard.
- [x] `message_start` seeds `OutputTokens` alongside the other three; `message_delta` still
      overrides, and its `> 0` guard keeps a sparse closing frame from zeroing the seed.
- [x] `make verify` green (exit 0, coverage 91.93%). The pinned `verifier` subagent did **not** run
      — the branch was developed from outside the repo, so its `.claude/` agents never loaded.
- [x] Reviews: two independent Claude reviewers, both PASS. Both new tests mutation-checked (each
      fails iff its invariant is broken); the change matches the SDK's own accumulator and records
      identical numbers for official-API streams. Both raised one `docs/ARCHITECTURE.md`
      imprecision, now fixed.
- [ ] Codex reviewer — **stalled, not run**: account usage limit, resets 2026-07-25.
- [x] PR [#133](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/133) green on CI (ci,
      coverage, helm, compose, CodeQL). Draft until the Codex pass runs.

**Coordination:** [#130](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/130) (#90) is
open and wraps the same `message_start` block in a `reportedUsage` check. Whichever lands second
puts the `OutputTokens` line inside it; this file and CHANGELOG.md keep both entries.
