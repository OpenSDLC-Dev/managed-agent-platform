# REVIEW.md — Diff-Triggered Review & Reliability Guide

Reviewers and the independent verifier MUST read this file from the base branch (`git show origin/main:REVIEW.md`) so a pull request cannot weaken the rules judging itself. If `REVIEW.md` does not yet exist on `origin/main`, read the repository root copy.

To list only the items triggered by the current diff, run:

```bash
python3 .github/scripts/review_checklist_test.py --diff origin/main
```

## Severity rubric

Tag every review finding with one severity so authors and maintainers can prioritize cleanly:

- **Important** — must fix before merge (correctness, security, wire compatibility, data integrity, concurrency/race bug, migration hazard, or weakened verification gate).
- **Nit** — optional polish (naming, phrasing, minor readability). Never blocks merge on its own.
- **Pre-existing** — issue already present on `origin/main` outside this PR's diff. Do not hold the PR author responsible; file or reference a GitHub issue instead.

## Iterative review context

Review and verification rounds build on each other rather than restarting blind. Pass the verifier's evidence, prior review findings, and any fix commits into follow-up passes so reviewers verify fixes and hunt deeper regressions instead of repeating settled checks.

## Diff-triggered reliability checklist

Each item has a diff-readable trigger, a concrete check, and the incident or PR behind it. Only check the items whose trigger matches the diff (typically 2 to 6 items per PR).

### R01 — Migration immutability and rollback safety
- **Trigger:** `internal/store/migrations/`
- **Check:** Confirm no previously merged migration file was edited or renamed; new DDL lives in a new higher-numbered file; no statement forbidden inside a single transaction (`CREATE INDEX CONCURRENTLY`) is present; verify older binaries still run cleanly or document the deploy ordering.
- **Incident:** Plan 69 slice 7b (`docs/HISTORY.md`), `CLAUDE.md` migration invariant.

### R02 — Lock-vs-timestamp semantics (`now()` vs `clock_timestamp()`)
- **Trigger:** `internal/store/`, `internal/api/`, `internal/events/`, `internal/queue/`
- **Check:** Whenever a transaction acquires a blocking row or advisory lock before stamping an ordering or expiry timestamp, verify whether `clock_timestamp()` (statement time after lock) is required instead of `now()` (transaction start).
- **Incident:** `#552`/`#969`, `#975`/`#990`, `#989`/`#1037` (three separate regressions from stamping before lock acquisition).

### R03 — Object-delete transaction isolation
- **Trigger:** `internal/store/`, `internal/api/`, `internal/executor/`
- **Check:** Any code path that enqueues pending object deletions must begin its transaction with `store.BeginObjectDelete` (`READ COMMITTED`) and write through `store.EnqueueObjectDeletes`, never raw `INSERT INTO pending_object_deletes`.
- **Incident:** `#578`, `#703`, migration `0046_files_object_key.sql`.

### R04 — Wire schema compatibility and `docs/DIVERGENCES.md`
- **Trigger:** `internal/api/`, `internal/domain/`, `docs/DIVERGENCES.md`
- **Check:** Diff wire-visible JSON fields, enums, paths, error envelopes, and ID prefixes against the pinned `anthropic-sdk-go` version in `go.mod`. Reject silently-ignored request fields. Every deliberate divergence or unconfirmed inference must be recorded in `docs/DIVERGENCES.md`.
- **Incident:** `#827`, `#861`, `#1004`, Plan 51 (`tools/sdkref`).

### R05 — Pagination cursor list binding
- **Trigger:** `internal/api/page`, `internal/api/*list*`, `internal/api/sessionresources.go`
- **Check:** Confirm every paginated list binds issued cursors to its `cursorList`, rejects foreign-list and non-matching-kind cursors via `foreignToTime` or equivalent, and rejects unstorable bytes (`NUL`, invalid UTF-8) before SQL binding.
- **Incident:** `#135`, `#534`, `#983`, `#1004`/`#1064`.

### R06 — `internal/domain` stdlib-only containment
- **Trigger:** `internal/domain/`
- **Check:** Confirm `internal/domain` imports only the Go standard library — no `adk-go`, `genai`, MCP SDK, `go-jose`, or provider SDK types.
- **Incident:** `CLAUDE.md` non-negotiable design principles 1 & 2.

### R07 — Egress policy withdrawal, destination binding, and host validation
- **Trigger:** `internal/egress/`, `internal/gate/`, `internal/gaterun/`, `internal/vaultresolve/`, `internal/dialguard/`
- **Check:** Verify outbound admission re-checks the current policy snapshot right before dialing; policy withdrawal severs active plain-HTTP requests and CONNECT tunnels; vault credentials cannot expand destinations without resupplying secret material; non-ASCII/Unicode hostnames are rejected rather than punycode-folded into aliases.
- **Incident:** `#601`/`#618`, `#614`/`#1032`, Plan 70 (`#1054`, `#1056`, `#1060`).

### R08 — Sandbox path escaping and shell quoting
- **Trigger:** `internal/sandbox/`, `internal/toolset/`, `internal/executor/`
- **Check:** Verify paths containing single quotes, spaces, Unicode, symlinks, or lengths past 200 bytes round-trip safely through POSIX quoting without shell injection, temp-file escape, or container root escape.
- **Incident:** `#860`/`#868`, `#862`/`#1057`, `#1058`, `#930`/`#936`.

### R09 — Lossy model conversion and tool-name round-trips
- **Trigger:** `internal/modelgateway/convert/`, `internal/provider/`
- **Check:** Verify tool-name shortening respects upstream byte limits (OpenAI 64, Gemini 128) and lead-byte rules (Gemini `[A-Za-z_]`), preserves valid UTF-8, rejects collisions, and round-trips back to the original tool name in answers and streams.
- **Incident:** `#980`/`#984`, `#981`/`#986`, `#985`/`#991`.

### R10 — Concurrency readiness ordering and database clocks
- **Trigger:** `internal/events/`, `internal/brain/`, `internal/queue/`, `internal/worker/`
- **Check:** Verify readiness signals never open before initial wake/state setup completes (no `setReady` before `wakeAll`); expire leases using the database clock rather than local wall clock; run `make test-race` on touched concurrency packages.
- **Incident:** `#537`/`#556`, `#793`, Plan 75 slice 4 (`Broker.markReadyAndWake`).

### R11 — Principal authority, initiator binding, and credential isolation
- **Trigger:** `internal/login/`, `internal/identity/`, `internal/clioauth/`, `internal/api/`, `internal/modelgateway/`
- **Check:** Verify browser credentials and delegated claim tokens never leak to direct or upstream model routes; model turns check current org membership of the immutable initiator; trusted client IP throttling only trusts authenticated edge headers.
- **Incident:** Plan 69 (`#1041`–`#1049`), `#1061`, `#1065`, `#1066`.

### R12 — Helm template numeric precision and optional Secret keys
- **Trigger:** `deploy/helm/`
- **Check:** Test values through a YAML values file (not only `--set`) so large integers do not render in scientific notation and fractional invalid inputs are not silently rounded; assert both the env var name AND its `secretKeyRef.key` match `secret.yaml`.
- **Incident:** `ci.yml` Helm hardening regressions (`memoryBytes`, `runAsUser`, `GCPKMS_KEY_NAME`, `BLOB_BUCKET`).

### R13 — GCP foundation/environment split, KMS IAM roles, and bash 3.2 portability
- **Trigger:** `deploy/gcp/`, `tools/kmsrole/`, `tools/tfcorpus/`
- **Check:** Confirm `environment/` owns no unrecoverable resource kind, each GCP identity's KMS role matches `Encrypt`/`Decrypt` callers (`make gcp-kms-role-check`), and operator shell scripts avoid bash-4 constructs (`make gcp-lint`).
- **Incident:** `#748`/`#750`, `#754`, `#758`/`#761`/`#762`.

### R14 — GitHub Actions pin hygiene and `pipefail` safety
- **Trigger:** `.github/workflows/`, `.github/scripts/`
- **Check:** Every `uses:` is pinned to a commit SHA, every `actions/checkout` sets `persist-credentials: false`, setup-go uses `go-version-file: go.mod`, and no pipeline stage exits early before EOF (`grep -q`, `head`) under `pipefail` (`make pins-test pipes-test`).
- **Incident:** `#472`/`#518`, `#558`, `#763`.

### R15 — Test honesty and hygiene
- **Trigger:** `_test.go`
- **Check:** Confirm tests fail when the behavior under test is broken (never mirror the implementation or mock away the invariant), add no unannotated `time.Sleep` or silent `t.Skip` (`make test-hygiene-test`), and leave no leaked processes or Docker containers.
- **Incident:** `#346`, `#486`, `#843`, Plan 75 slices 1 & 4.

### R16 — Verification gate and review rule self-modification audit
- **Trigger:** `.claude/`, `REVIEW.md`, `Makefile`, `.github/workflows/ci.yml`, `.github/scripts/test_hygiene_baseline.json`
- **Check:** When a PR modifies verification rules, coverage exclusions, hygiene baselines, or CI gates, confirm no check was silently weakened or bypassed without explicit rationale and maintainer sign-off.
- **Incident:** Plan 75 slice 5 (guarding the verifiers).

## Rule lifecycle (addition and pruning)

- **Add a rule** when a missed defect recurs across at least two PRs (or represents a high-severity security/data-loss class) and can be spotted from a diff, or when a `tools/revieweval` incident replay shows reviewers missed it on a small diff.
- **Evaluate before editing rules or prompts:** run `make revieweval-dry-run` (offline structural and trigger validation) and `go run ./tools/revieweval -live -subset tuning` (followed by `-subset eval` once frozen) to verify recall improves without raising the control false-positive rate or hiding drops between `raw` and `post_verify`.
- **Quarterly / release-cadence pruning policy:** once per minor release (or quarterly, whichever comes first), audit every `R01`–`R16` entry and prune or promote it when any of these holds:
  1. An automated compile-time check, unit/property test, or CI guard (`make verify`, `test-hygiene-test`, `pins-test`, `pipes-test`) now makes the failure mode impossible or strictly enforced — remove the redundant review bullet.
  2. A rule has triggered across recent PRs while producing only `Nit` or false-positive comments without catching a real `Important` defect — tighten its trigger glob or retire it.
  3. The checklist grows beyond ~20 rules — consolidate overlapping items so a typical PR still triggers only 2 to 6 focused checks.
