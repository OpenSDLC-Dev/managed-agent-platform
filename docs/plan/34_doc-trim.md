---
status: archived
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
   the aim) and the recut of every unreleased fragment over it; the release blocker in the
   `cd-build-on-runner` fragment (`docs/changelog/0.3.0.md`); the sandbox-pool sizing
   correction in `docs/deploy-gcp.md`.
   Markdown only.
2. **Comments worth pointing at** — the seven package-comment repairs and additions, the
   drift-pinning test, and the matching `variables.tf` description. Touches `.go`; comments only,
   no logic, no new statements — with one exception it also carries: a byte-length check in
   `loadFragments`, so the 1,500-byte cap slice 1 introduced is enforced rather than advisory.
   Without it the cap regresses silently, which is exactly how `changelog.d/` reached 196 KB.
3. **`docs/ARCHITECTURE.md`** — delete the per-file tables (measured: 176 rows across 30
   subsections, 92% of the file), replaced by a map. `go doc` is the pointer only where the
   exported surface is the story; for `api`, `brain` and `executor` the substance is unexported
   and the pointer is the files. Cross-package claims found inside a row move to the map or the
   execution-flow section rather than being dropped.
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
| archived plans (32) | 545 KB | ~~80 KB~~ retired — nothing is cuttable; see below |
| `docs/changelog/` | 518 KB | ~~40 KB~~ retired — the files are byte-frozen |
| `HISTORY.md` + `history/` | 414 KB | ~~120 KB~~ retired — the record is the evidence; see below |
| `ARCHITECTURE.md` | 325 KB | ~~110 KB~~ 39 KB |
| `DIVERGENCES.md` | 313 KB | ~~120 KB~~ landed at 228 KB — the floor is entry count, not prose |
| `changelog.d/` | 196 KB | 60 KB — met at slice 1 (50 KB), but a staging directory a release empties, so the figure is transient |
| deployment docs (4) | 174 KB | ~~95 KB~~ landed at 163 KB — the GCP pair was kept |
| `self-hosted-security.md` | 77 KB | ~~62 KB~~ retired — slice 4 grew it to 79 KB |
| `CLAUDE.md`/`README.md`/`AGENTS.md`/`STATE.md` | 56 KB | ~~32 KB~~ landed at 50 KB |

A struck target is one a slice deliberately did not meet; the checklist entry below says
why in each case. They are amended rather than deleted so the plan stays a record of what
was aimed at, not only of what happened.

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
- **`internal/executor/mcpwork.go`'s two "later slice" mentions are still true, with one
  qualifier.** Slice 2 swept eight stale ones and checked these. The `mcp_catalogs.error`
  column really is unread: the brain selects `server_name, url, status, tools` and not it
  (mcptools.go), and `internal/api` only deletes those rows. But the same text is already on
  the wire by another road — the settlement passes `message: r.reason` to `mcpFailureEvent`
  (mcpwork.go:966), which appends a `session.error` the events endpoints serve. So "to the
  model" is fully future and "to the API" is not. A later slice revisiting these comments
  should add that qualifier rather than delete them. Not every forward-looking comment is
  stale — and not every one is wholly true either.

- **Slice 4's deployment cluster was two documents, not four.** The 174 KB → 95 KB target
  assumed all four deployment docs restated their configuration the way `ARCHITECTURE.md`
  restated the packages. Measured, that held for exactly two: the chart README's
  "Notable values" table restated `values.yaml` (whose 53 keys already carry `# --`
  helm-docs annotations, richer per key than the table), and the compose README's variable
  table was a *third* home behind `.env.example` and `docker-compose.yml`'s own inline
  comments. It did not hold for the two GCP documents, which are procedure, decisions and
  recovery — state adoption, the gVisor/gate incompatibility with its exact error strings,
  required node settings — none of which the `.tf` files can hold. They do name Terraform
  variables, but never as a reference: the mentions sit inside shell procedures, a
  `terraform.tfvars` example, cross-references tying `foundation/` to `environment/`, and
  table rows about an operation (rotate this password, pick this cluster shape) rather than
  about the variable. Nothing there reproduces a `variables.tf` description, so both were
  kept.
- **Measure the shape, not the token — and this entry had to learn it twice.** The first
  pass counted `var.` occurrences, found one, and reported that the GCP documents name a
  variable once; that instrument matches Terraform *expression* syntax and misses every
  bare `name_prefix`. The correction then counted bare names, undercounted again by
  dropping the common-word variables (`region`, `zone`, `namespace`), and asserted "one
  table" past two table rows that contain variables. Both counts were wrong; the shape
  claim they were meant to support was right each time. The numbers are gone rather than
  fixed a third time, because they were never the argument — the same lesson as slice 3's
  `go doc` measurement, which counted only exported symbols and nearly condemned seven
  well-documented packages. `docs/self-hosted-security.md` is the same: a platform-enforces/you-own
  synthesis that no single file carries, and it names no Terraform variable at all. The two
  GCP documents were therefore left untouched, and cutting them would have traded truth for
  brevity, which the Ground truth above forbids. **`self-hosted-security.md` went the other
  way** — slice 4 grew it from 77 KB to 79 KB with the MCP-egress account it was missing and
  the identity consequence of a grandfathered environment key, so its 62 KB target is
  retired rather than missed. A byte target derived from one document's failure mode does
  not transfer to documents with a different one.
- **Carried into a later slice: `docs/ARCHITECTURE.md` is a second, un-pinned home for the
  test-support package set.** Its list is currently accurate against the Makefile's
  exclusion expression — all twelve match — but slice 4 removed CLAUDE.md's copy precisely
  because a hand-maintained enumeration of that set drifts, and this one is subject to the
  same failure with nothing to catch it. Slice 4's diff does not touch ARCHITECTURE.md, so
  it was left alone rather than widened; whichever later slice edits that file replaces the
  list with the naming convention, as CLAUDE.md now does.
- ~~`docs/DIVERGENCES.md` cites "the v1.62.0 checkout" three times~~ **Settled in slice 5.**
  Re-verified at the checkout, whose newest tag, `git describe` and `internal/version.go`
  all say v1.61.0 — the `go.mod` pin. Each claim was re-read there and restated to what a
  reviewer can open: the resolved-config types and the eight-value `Name` enum are read at
  the pin, and the sentence about no `AlwaysAsk` outside the generated schema now names the
  two files that carry the boilerplate (`betaagent.go`, `betasession.go`). The
  "byte-identical in the v1.62.0 checkout" comparison is gone rather than restated — there
  is no second version here to compare against, so the claim was unreproducible by
  construction, not merely mis-versioned.
- **Three byte targets in the table above rest on the same mistaken premise, and slice 5
  measured the third.** Each was derived from a cluster's total size without asking what
  the cluster must still hold. For `DIVERGENCES.md`, 120 KB over 142 entries implies 845
  bytes an entry, against a 1,256-byte mean for the 118 entries this slice never touched —
  the target is smaller than what an untrimmed row already costs. The file's floor is entry
  count times what a row must carry: a surface, what the platform does, whether it is
  CONFIRMED or INFERRED, and an evidence pointer. So the reachable win was the 24 entries
  that had grown into essays, and it was taken: 313 KB → 228 KB, every entry, line, header
  and citation intact.
- **Archived plans are not trimmable at all, and the attempt is the evidence.** 33 spent-
  looking sections were removed and then restored in the same PR, because review found the
  premise false in two ways the cut-candidate check had never tested. The check compared
  candidates against *quoted section titles* and found zero collisions — but plans are also
  addressed **by slice number** (`plan 32 slice 2`, `plan 29 slice 1`, in DIVERGENCES
  entries, code comments, tests, a migration and the released changelog — over a hundred
  such references, the exact figure depending on how you count, which is why the shape and
  not the number is the argument), and **by role** (`docs/changelog/0.2.0.md` says of plan
  21 "whose DCF-rubric example the plan's acceptance replays end-to-end"). Deleting a
  Slices section orphans every by-number citation; deleting an Acceptance section falsifies
  a byte-frozen released file. Rewriting those citations is the churn design decision 3
  above already rejected. Most of the corpus is cited by `DIVERGENCES.md` or is the problem
  statement and design rationale Ground truth protects; what looked like the remaining
  scaffolding turned out to be the reference targets themselves. An archived plan is a
  citation target, not prose — its size is not the measure of it.
- **`docs/changelog/` cannot be trimmed at all without breaking a stated invariant.** Its
  files are called "byte-frozen" by `docs/changelog/0.3.0.md`'s history-split entry, and
  `make changelog-archive` is recorded as byte-reversible — which holds only while an
  archived file equals the section it was moved from. Deleting or rewriting one also breaks
  the index stubs in `CHANGELOG.md`, which only a release PR or the archive tool may edit.
  The 40 KB target is retired rather than missed.
- **The record is the evidence, so slice 6 cut nothing from it — the fourth target retired,
  and the fourth to fail on measurement** (the three above, plus this one).
  414 KB → 120 KB assumed HISTORY was padded with narrative the changelog
  already carried. What it holds instead is what a changelog structurally cannot: measurements,
  attacks that were tried, alternatives evaluated and rejected. Two probes proposed deletions
  and both premises broke the same way every earlier one did — on the reference class nobody
  measured. Here it was the *unit of citation*: the checkers matched section headings, but more
  citations quote body prose than name a heading, so preserving every heading would still have
  orphaned them. The precedent was already on disk — plan 03 had trimmed this file's largest
  section by exactly the proposed method in 2026-07, and what it left behind is what survives
  a search of the changelog. That leaves nothing to re-cut.
- **The four retirements share one root cause, and it is worth stating once.** Each was derived
  from a cluster's *size* without asking what the cluster must still hold, then defended by an
  instrument narrower than the thing it tested — quoted titles only, `var.` occurrences only,
  heading granularity. The lesson is not "documents are incompressible": of the nine targets,
  four were retired, three were missed after real cuts (`DIVERGENCES.md`, the deployment pair,
  the steering cluster), and two were met or beaten — `ARCHITECTURE.md` by 71 KB, and
  `changelog.d/` at slice 1. It is that a byte target is a hypothesis about content, and only
  the content can settle it.
- **What slice 6 did instead: made the record true.** Three copies of one provenance claim had
  been falsified by this plan's own slice 3, which edited `docs/history/2026-07.md` — the
  archive header, the `history-split` entry now in `docs/changelog/0.3.0.md`, and `HISTORY.md`'s own plan-28
  record, which review caught after the first two were fixed and which is the reason to grep
  for a claim rather than fix the copies you happen to remember. A fourth statement, in
  archived plan 28's Acceptance list, is deliberately left: it records a check made at
  delivery, which is true of that moment, not a property claimed to still hold. All three now say
  byte-reversibility was a property of the *move*, not a standing invariant; the fragment was
  the urgent one, since a release would have frozen the false claim into
  `CHANGELOG.md`. Two decisions the archive records as final have since been reversed by plan
  29 (MCP confirmation gating; `Attach` back on `SandboxProvider`) and now carry dated notes in
  the existing "Since closed" style — the record is not edited to match the present, because a
  decision that was later overturned is exactly the thing an archive exists to hold. One quoted
  sentence had drifted across three copies and was re-pointed at the source it actually came
  from. `HISTORY.md`'s own provenance paragraph now names the citation unit, so the next
  trimmer starts where this slice finished rather than re-deriving it.
