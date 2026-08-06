---
status: draft
---

# Verification hardening: checkers proven by what they refuse, verdicts that travel with their evidence (plan 25)

This plan hardens the verification machinery itself — no product behavior changes. It
comes out of a comparative study of Anthropic's `how-we-claude-code` workshop sample
(its `phase-3-verify` app demonstrates a "verifiable component architecture": runtime
observation at the surface, machine-readable contracts, mandatory adversarial fixtures,
a four-value verdict taxonomy) against this repo's existing regime. Most of the
workshop's ideas are already here in stronger form — the wire schema pinned by reference
checkout beats a self-declared contract, the implementer/certifier split beats a shared
pipeline, fail-not-skip beats a benign SKIP. Five gaps survived the adversarial
comparison; this plan closes them. Four distilled principles drive the slices:

1. **A guard is only worth what it refuses.** The workshop pins a fixture designed to
   fail and asserts the framework fails it. This repo already writes that doctrine down —
   `deploy/gcp/check_split_test.py`'s docstring, and mutant-style tests at
   `internal/api/sessions_test.go:916`, `internal/executor/files_test.go:207`,
   `internal/blob/gcs/gcs_test.go:258` — but no Go contract suite or eval grader pack has
   a suite-level known-bad subject.
2. **"Couldn't observe" and "observed and wrong" are different facts.** Conflating them
   costs triage time; today the distinction lives only in session folklore ("two
   consecutive CI failures in different suites = contention, not a regression").
3. **A verdict travels with its evidence and the address of its own reproduction.**
4. **A verification step that silently doesn't run reads as green.** Structure makes the
   missing step visible; prose does not.

Each slice is one small PR, independently landable, in any order. The suggested order
below is by value.

## What this plan does not do

Restraint is part of the design; each rejection is deliberate:

- **No self-declared introspection contract** (a `data-verify-*` analogue, debug
  self-description endpoints). The platform's machine-readable surface is the wire,
  pinned externally by the SDK reference checkout; a self-report layer would be a second,
  drift-prone source of truth beside the event log — the exact failure the same-source
  instrumentation principle exists to preclude.
- **No unification of the implementer's and certifier's pipelines.** The workshop's "one
  code path, three consumers" concentrates authorship; this repo deliberately splits it
  (the verifier derives scope itself; reviewers run foreign models).
- **No BLOCKED as a non-red state.** Slice 2's marker labels a red run; it never becomes
  a skip. The consent contract (missing config fails, missing Docker fails) is untouched.
- **No replay theater or video evidence.** For a headless multi-process platform the
  transcript is the replay; evals already flushes per-trial artifacts.
- **No registry/manifest layer over `go test`.** The toolchain already enumerates every
  suite; the Makefile stays the single executable source of the gate.

## Ground truth

Measured at 56f81f9, not assumed:

- `internal/provider/providertest` contains only `contract.go` — no `_test.go`; nothing
  feeds a non-conforming backend to `Run` and asserts refusal. `blobtest` has
  `mem_test.go` (a **conforming** self-check) and `ready_test.go` (a fixture meta-test) —
  neither proves the suite rejects a violating store. `sandboxtest` has zero `_test.go`.
- `evals/grade_unit_test.go` (1220 lines) is per-grader and per-helper over hand-written
  mini-transcripts; there is no pinned known-bad whole transcript asserted to grade as a
  failed trial through the full grader pack.
- Environment provisioning failures carry no machine-readable marker, and the shared
  harnesses fail from `TestMain`, not `t.Fatalf`: `pgtest.Main`
  (`internal/pgtest/pgtest.go:48-59`, its `startReady` helper through `:96`),
  `blobtest.Main` (`internal/blob/blobtest/blobtest.go:60-71`, `startReady` through
  `:109`), `gcstest.Main` (`internal/blob/gcs/gcstest/gcstest.go:49-79`), and
  `secretstest.Main` (`internal/secrets/secretstest/secretstest.go:47-58`, consumed by
  `internal/secrets/openbao/openbao_test.go:20`) print to stderr and return 1. Per-test
  provisioning failures are plain `t.Fatalf` — `FreshDB` (`pgtest.go:193,198`),
  `FreshBucket` (`gcstest.go:165,170`), sandbox `provision:`
  (`internal/sandbox/sandboxtest/contract.go:78,103` — where a dead daemon actually
  surfaces, since `docker.New`/`k8s.New` never ping), the gate fixtures
  (`sandboxtest/gatefixture.go:52,56,92,96,131,165,175`; `k8s_test.go:212-281`), and the
  toolset harness (`internal/toolset/toolset_test.go:29,37`). This survey is by seam and
  is **not** claimed exhaustive by file — slice 2 defines its scope by sweep. One site
  deliberately mixes environment and product: `pgtest.NewPool`'s `open store` failure
  (`pgtest.go:209`) calls `store.Open`, which runs the repo's migrations
  (`internal/store/store.go:37`) — a failure there can be a broken migration, not
  blockage.
- The bare word `BLOCKED` already appears in `internal/sandbox/sandboxtest/egress.go:125,131`
  as an egress-probe **behavior** token; a marker must not collide with a bare-word grep.
- The verifier's report format is an exact prose template
  (`.claude/agents/verifier.md:32-39`); rung 2's scratch-copy mutation proof is
  discretionary — "When you doubt a specific test can fail, you may prove it"
  (`verifier.md:23`, reinforced at `:3` and `:12`). Practice is already stricter than the
  rule.
- `evals` artifacts carry no rerun command: the `record` struct
  (`evals/report_test.go:41-54`) and `renderSummary`'s failure detail
  (`report_test.go:319-328`) point to the transcript only. A single task **is** already
  selectable — `TestEvals` runs one subtest per task ID (`evals/evals_test.go:25`) — but
  that knowledge exists only in the code structure. Contract-suite subtest names are
  already stable literals (`ExecTimeoutKillsAndSurvives`, `PutGetRoundTrip`,
  `TextTurnTerminatesWithStopAndUsage`), promised nowhere.
- `acceptance`'s `runDCF` (`acceptance/dcf_test.go:72-238`) reports failures through
  plain `t.Fatalf`/`t.Errorf` calls and writes no artifact on any failure path; its
  stream watcher already buffers every SSE frame with the raw wire JSON retained (`streamWatch.frames`, `dcf_test.go:285-320`; the SDK union's
  `RawJSON()`), and `dcfRun` holds the downloaded deliverable bytes — a byte-faithful
  dump is a serialization of state already in memory. `.gitignore` has `/evals/artifacts/`
  and no acceptance sibling.

## Slice 1 — known-bad subjects: the contract suite and the graders must be seen to refuse

**providertest.** Add a self-test in `internal/provider/providertest` that feeds `Run` a
deliberately non-conforming backend and asserts the suite goes red. Mechanism: the
re-exec pattern (the stdlib's helper-process idiom) — an env-gated known-bad `Backend`
whose fake provider violates pinned invariants (a second `done` chunk, `end_turn` on a
tool turn, zeroed usage when the upstream reported none), run by re-invoking the test
binary and asserting a non-zero exit plus the expected failure text. Refactoring the
suite's rows to return errors was considered and rejected: invasive, and it would change
the suite every backend already passes.

**evals.** Add pinned known-bad whole transcript fixtures — **one per failure cause** (a
wrong final answer; a forbidden tool call), each asserted to grade `Pass: false` with the
expected *named* grader failure, through the full core+task grader pack — one level above
`grade_unit_test.go`'s per-grader coverage. One fault per fixture keeps the mutation
criterion honest: a two-fault fixture stays green when either fault alone is repaired.
Grading-only, so it needs no recorder; if it ever touches `recordTrial`, it must use the
scratch recorder (`evals/report_unit_test.go:12-38` documents why).

Known limitation, recorded not built: a bad `Backend` *renderer* (e.g. streaming tool
input in a single frame) would silently weaken the accumulation row — the multi-frame
requirement at `contract.go:68-70` is enforced only by comment. Same doctrine; out of
scope, as are `sandboxtest`/`blobtest` known-bad subjects. Whether to extend the pattern
to any of them is a backlog decision made in GitHub issues; this plan records no future
work.

Acceptance: reverting any one of the violated invariants' assertions in `contract.go`
turns the self-test red; repairing any known-bad fixture's single fault turns exactly
that fixture's test red. (The meta-tests are themselves mutation-tested — the rule they
exist to structuralize.)

## Slice 2 — BLOCKED(environment): label the provisioning failure, keep it red

Prefix environment-provisioning failure messages with the marker
`BLOCKED(environment): `. Scope is defined by seam and found by sweep at implementation
time, not by the ground-truth survey above: every Docker/K8s-backed shared harness
`Main` (`pgtest`, `blobtest`, `gcstest`, `secretstest`) and every per-test provisioning
failure that fires **before any repo logic runs** (`FreshDB`, `FreshBucket`, sandboxtest
`provision:`, the gate fixtures, the docker/k8s/toolset harness entry points, and
whatever else the sweep finds). Behavior-assertion failures stay unmarked. Both remain
red: the marker classifies the failure, it never softens it.

The *before any repo logic runs* rule is load-bearing: `pgtest.NewPool`'s `open store`
failure stays **unmarked**, because `store.Open` runs the repo's own migrations
(`internal/store/store.go:37`) — a failure there can be a broken migration, which is
"observed and wrong", not "couldn't observe". Where one call mixes both (a connect error
and a migration error surface identically), the site stays unmarked: a missing marker
costs one extra look, a wrong marker hides a bug — the same asymmetry that makes the
runner fail when in doubt.

The grep token is the full `BLOCKED(environment)` — the bare word collides with the
egress probe's behavior token (`egress.go:125,131`). `pgtest`/`blobtest` retry once
(#265); the interim `; retrying` line keeps its wording and gains no marker — only the
final give-up line does. One sentence lands in docs/ARCHITECTURE.md's testing section
naming the convention, so triage ("rerun or investigate?") stops depending on session
folklore.

Acceptance: with Docker stopped, every provisioning-failure site the sweep identified
carries the marker (checked against the sweep's own list, not the survey above); a
seeded assertion failure (scratch copy) does not. The marker-bearing paths are
mutation-checked per slice 1's rule where a cheap seeding exists; harness stderr lines
are verified by running the binary against a stopped daemon, not unit-tested.

## Slice 3 — the verifier reports structure, and every new guard shows its red run

Two changes to `.claude/agents/verifier.md`, landing as one PR (behavior-steering
markdown: full dual-review ritual).

**Structured verdict.** The report keeps its prose bullets and additionally requires one
fenced JSON block: `rungs: [{rung: 1..5, name, verdict: PASS|FAIL|BLOCKED|SKIP,
evidence}]`, `findings: [{n, severity: blocker|concern|note, file, line, summary}]`, and
an overall `verdict`. Semantics: `BLOCKED` = wanted to observe, couldn't (environment,
access) — with the reason; `SKIP` = rung not applicable to this diff — with the reason;
neither is a pass, and any rung `FAIL`, or a `BLOCKED` on a rung the change touches,
keeps the work not-done (unchanged from today's rule). A rung that silently didn't run
becomes a visible missing key, not an absent paragraph — the same failure mode the
rate-limited-CodeRabbit incident taught. CLAUDE.md step 5 gains half a line: the PR
description embeds the JSON block verbatim.

**Red-run evidence by default.** Rung 2's scratch-copy mutation proof goes from
discretionary to default for the diff's own guards: for every test the diff adds or
modifies that guards changed behavior, rung 2 requires evidence the test fails against
the reverted/broken behavior in the throwaway scratch copy; a missing red run is itself
a finding. Tests the diff does not touch stay under the existing doubt-triggered rule.
This codifies what PR #276's review pass caught — three new regression tests that had
never run against the broken code (that PR's review threads are the record) — and what
the mutation-test-every-new-guard practice already does by hand.

Acceptance: a verifier dispatch on a trivial doc change returns the JSON block with rung
verdicts including honest `SKIP`s; a dispatch on a change whose new test never saw the
broken code returns a finding saying so.

## Slice 4 — the failure artifact carries its own rerun

**evals.** The `record` struct and `renderSummary`'s failure detail gain a `rerun` field
/ line: `RUN_EVALS=1 go test -count=1 -v -timeout 120m -run 'TestEvals/<task-id>$'
./evals/`. The command is cheap to print and honest about cost: a filtered run still
pays the fixed setup (image pull, Postgres, stack) — the artifact says what to type, not
that it is fast. `make eval` stays unparameterized on purpose; the `go test` line is the
scoped path.

**Contract suites.** `providertest`, `sandboxtest`, `blobtest` package docs gain one
sentence promising subtest-name stability — names are part of the suite's contract, so a
failure line like `TestDockerProviderContract/ExecTimeoutKillsAndSurvives` always implies
a runnable `-run` pattern. Renaming a row is a contract change, reviewed as one.

Acceptance: a forced-fail eval run's `report.json` and `summary.md` carry a rerun line
that, pasted verbatim, reruns exactly the failed task.

## Slice 5 — acceptance failures leave the wire transcript

`runDCF` registers a `t.Cleanup` that, on `t.Failed()`, dumps to gitignored
`acceptance/artifacts/` (a `.gitignore` sibling of `/evals/artifacts/`): the buffered SSE
frames' raw wire JSON in order, the `dcfRun` summary (session ID, outcome ID, terminal
state, grader explanation), and the deliverable bytes themselves — `dcfRun.Contents`
already holds them, they are doc-example-sized, and the rehearsal leg's in-memory blob
store and test-scoped Postgres die with the test, so a name-and-hash record would strand
any byte-content diagnosis. Output passes a scrub mirroring evals' policy before
writing. Both legs inherit from the one harness change;
the rehearsal leg gives CI failures their evidence, the live leg saves a full-stack
re-run whose whole point was being expensive.

Acceptance: a seeded rehearsal failure (scratch copy) leaves a transcript whose frame
sequence replays what the typed assertions saw; a passing run writes nothing.

## Delivery

Five PRs, one per slice, any order; suggested order as numbered. Slices 1, 2, 4, 5 are
ordinary code PRs under the full gate; slice 3 is `.claude/`-steering markdown and takes
the full ritual explicitly. STATE.md picks the plan up when its first slice starts;
this plan file flips to `in-progress` in that same PR.
