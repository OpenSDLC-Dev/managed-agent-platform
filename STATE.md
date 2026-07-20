# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#99](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/99) — eval grader rigor: the
four P/M/E precision and coverage-depth gaps PR #98 left out of scope. No plan file (triaged as
single-PR work, confined to `evals/`).

## Tasks

- [x] Tasks 1–3: reclass the model-dependent Platform checks (`fib-quickstart`'s file and
      tool-result checks → Either; `shell-state`'s nonce round trip split into a Model
      prerequisite plus a vacuous-unless-called Platform check).
- [x] `journal-multiturn`: a replay-only code word (`{{RECALL}}`, kept off disk by
      `NotInToolTraffic`) and a seeded file the model is never told about as the container-reuse
      witness.
- [x] `glob` output graded: `GlobPathList` (Platform, pattern-independent) plus the seeded path
      (Either).
- [x] `ConfirmedResult` joins the expected tool/input → its confirmation → its result, and reds a
      confirmation naming no `agent.tool_use`. Any-match like `CallResult`, so a retry after an
      error is not a Platform red.
- [x] Live `make eval` (`MiniMax-M3`): 10/10, over five runs in which **no Platform grader ever
      fired** — the two reds were a Model and an Either finding, which is the invariant working.
      An early run caught a real defect — a token substituted on the prompt side but not in the
      grader — closed by folding every substitution into `(*Trial).fill`.
- [x] Reviews settled: Codex, `/code-review` and the verifier all landed findings; the confirmed
      ones share a shape — a grader no mutation of itself could catch. Re-verified at the final
      state: `make verify` green, live eval 10/10, 17 mutation probes with 16 killed — the last by
      a live run. The survivor is an equivalent mutant, not a gap; the PR says which and why.
