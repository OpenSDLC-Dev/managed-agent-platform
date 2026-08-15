---
status: in-progress
issue: "#413"
---

# Plan 34 — trimming the documentation to what code cannot say

## The problem

Tracked markdown is **2,665,735 bytes** against **2,364,778 bytes** of non-test Go: the
documentation is larger than the system it documents. Line counts hid this — `docs/DIVERGENCES.md`
is 196 lines and 312 KB, single table rows in `docs/ARCHITECTURE.md` exceed 25 KB, and one
"1-line" changelog fragment is 17 KB — so measure this work in bytes, not lines.

Size is the symptom. The defect is **restatement**: prose that repeats what a Go doc comment,
a Terraform `description`, a `values.yaml` comment or another markdown file already says. A
fact with two homes drifts, and this repo has already paid for that — see the corrections in
slice 1.

## Ground truth

Code and its comments are the source of truth. A doc earns its place only by holding what code
cannot: why a decision was made, what was rejected, a constraint imposed from outside, an
obligation on the operator. Those are never cut here. Restatement, duplication and staleness are.

## Design decisions

1. **Comments come before deletions.** A pointer to `go doc ./internal/<pkg>` is only as good as
   the comment behind it, and three package comments currently describe shipped code as unbuilt.
   Slice 2 fixes the comments; no package's prose is deleted before `go doc` answers what the
   prose answered. Rejected: deleting first and backfilling comments after — that ships a window
   in which the terse answer is a falsehood, which is worse than the verbose truth.
2. **Pin the lists that drift, don't just correct them.** The ID-prefix set has drifted twice and
   the `RUN_LIVE_*` set three ways. Correcting them once repeats the failure; slice 2 adds an
   offline test in the shape of the existing `evals.TestDocsSpellTheTrialCount`.
3. **Archived plans keep their cited headings.** 291 references point into `docs/plan/`, many by
   section anchor. Compression keeps every cited heading as a stub line and re-greps after each
   file. Rejected: rewriting the 291 citations, which spends the budget on churn.
4. **Deletion must name its recovery.** Nothing is deleted without a verified `git show` command
   in the commit message. For the released changelog that is
   `git show 816d8e4^:CHANGELOG.md`, confirmed to hold all 174 entries of the 0.2.0 section.
5. **Review tier drives PR boundaries.** A markdown-only diff (excluding CLAUDE.md, AGENTS.md and
   `.claude/`) takes a single reviewer; anything else takes the full dual pass. Slices are cut so
   that exactly one of them touches non-markdown.

## Slices

1. **Fragments and facts** — the `changelog.d/` size convention (1,500-byte cap, 60–120 words
   the aim) and the recut of every unreleased fragment over it; the release blocker in
   `cd-build-on-runner.fixed.md`; the sandbox-pool sizing correction in `docs/deploy-gcp.md`.
   Markdown only.
2. **Comments worth pointing at** — the seven package-comment repairs and additions, the
   drift-pinning test, and the matching `variables.tf` description. Touches `.go`; comments only,
   no logic, no new statements — with one exception it also carries: a byte-length check in
   `loadFragments`, so the 1,500-byte cap slice 1 introduced is enforced rather than advisory.
   Without it the cap regresses silently, which is exactly how `changelog.d/` reached 196 KB.
3. **`docs/ARCHITECTURE.md`** — delete the 155 per-file table rows and 27 package preambles,
   package by package, each replaced by a stub pointing at `go doc`. Cross-package claims found
   inside a row move to the execution-flow section rather than being dropped.
4. **Deployment, security and steering** — the four deployment documents against
   `values.yaml`/`*.tf`/`docker-compose.yml`; `docs/self-hosted-security.md` (including the
   missing MCP-egress account); `README.md`, `AGENTS.md` and `CLAUDE.md`, which gains a *Writing
   docs* section stating this plan's ground truth as a standing rule.
5. **Registry and plans** — reshape `docs/DIVERGENCES.md` into the registry rows its own header
   promises, re-verifying each entry's evidence against the checkout; compress the 32 archived
   plans to their decisions.
6. **The record** — `docs/HISTORY.md`, `docs/history/` and `docs/changelog/`.

## Targets

| cluster | now | target |
| --- | ---: | ---: |
| archived plans (32) | 545 KB | 80 KB |
| `docs/changelog/` | 518 KB | 40 KB |
| `HISTORY.md` + `history/` | 414 KB | 120 KB |
| `ARCHITECTURE.md` | 325 KB | 110 KB |
| `DIVERGENCES.md` | 313 KB | 120 KB |
| `changelog.d/` | 196 KB | 60 KB |
| deployment docs (4) | 174 KB | 95 KB |
| `self-hosted-security.md` | 77 KB | 62 KB |
| `CLAUDE.md`/`README.md`/`AGENTS.md`/`STATE.md` | 56 KB | 32 KB |

## Recording checklist

- **Slice 2 refuted one of its own preconditions.** `internal/vaultresolve/mcprefresh.go` was listed
  as holding its retryable-versus-unusable taxonomy only in prose. It does not: `mcp.go:13-22`
  defines the split and the caller's differing response, `mcprefresh.go:119-123` gives the
  refresh-specific version, and `exchange` carries per-arm rationale. Nothing was added, and slice 3
  may cut ARCHITECTURE's vaultresolve paragraph without a precondition. A survey finding is a
  hypothesis; the code is the answer.
- **Native Windows is not a supported place to run the gate.**
  `tools/changelog.TestAssembleStagingFailureLeavesEverythingUntouched` fails there because
  `os.Chmod(dir, 0o555)` does not deny a rename on Windows and `os.Geteuid()` returns -1, so its
  root-skip never fires. Confirmed identical at `main` with main's own sources, so it is the
  environment, not a regression. Run the gate in WSL or read CI.

- **Config-doc gap for slice 4.** The rule that decides how a role-claim *name* is read — a
  URI-shaped name is one flat key, any other dotted name is a path, fixed at configuration time
  so a token cannot choose the reading — is stated only in `internal/identity/claims.go:10-22`.
  An operator spelling `IDENTITY_CLAIM_*` reads the chart and compose READMEs, and neither
  carries it. Slice 4 puts it there.
- **Never run the assembler to check a fragment.** `go run ./tools/changelog assemble` deletes
  every file in `changelog.d/` after folding it into CHANGELOG.md; a verification attempt during
  slice 1 destroyed the whole recut and it had to be rebuilt from the agent transcripts. Check
  fragments against `loadFragments`' rules by reading, or run `TestTheShippingFragmentsLoad`,
  which the gate now runs for you — never by executing `assemble`.
- **`internal/executor/mcpwork.go`'s two "later slice" mentions are still true.** Slice 2 swept
  eight stale ones and checked these; the `reason` column exists, but no code under `internal/api`
  or `internal/brain` reads it, so "later slices surface it to the model and the API" remains
  future work. Leave them until something does. Not every forward-looking comment is stale.

- `docs/DIVERGENCES.md` cites "the v1.62.0 checkout" three times as evidence, but the local
  `anthropic-sdk-go` reference checkout's newest tag is v1.61.0, which is also the `go.mod` pin.
  The substance verifies (`betaagent.go` does carry the four resolved-config types at v1.61.0),
  so this is a citation a reviewer cannot reproduce rather than a false claim. Slice 5 settles it
  by re-verifying against the checkout and restating the version, not by swapping the string.
