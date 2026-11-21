---
status: archived
---

# Verification and testing hardening (plan 75)

Strengthens the platform's verification loop across test speed, property and fuzz coverage, race detection, diff-triggered review rules, full-topology offline end-to-end tests, wire differential checks, commit verification gates, and reviewer evaluation.

## Scope

1. **Slice 1 — Test speed and dead-owner reaping (#499).** Clone pre-migrated template databases in `internal/pgtest`, reap dead-owner fixtures safely in `internal/dockertest`, and add `make test-fast` for container-free local feedback.
2. **Slice 2 — Local `verify` skill, repair cap, and branch protection.** Add `.claude/skills/verify/SKILL.md` for repeatable local end-to-end smoke checks, cap autonomous repair loops at three failed rounds before human escalation, and align `main` branch protection required checks with CI.
3. **Slice 3 — Property tests and fuzzing.** Add zero-dependency property tests and `Fuzz*` targets for model gateway conversion, sandbox path handling, event/queue invariants, and API pagination/domain round-trips, plus a scheduled fuzz workflow.
4. **Slice 4 — Race detection and test hygiene.** Add `make test-race` and a CI `race` job for high-concurrency packages, plus a test-hygiene guard baselining existing `time.Sleep` and `t.Skip` uses.
5. **Slice 5 — `REVIEW.md`, diff-triggered reliability checklist, and rule isolation.** Add a root `REVIEW.md` with diff-triggered checks backed by past incidents, load review baselines from `origin/main`, chain review context across rounds, and audit PRs that modify verification gates.
6. **Slice 6 — Full-topology offline E2E and wire differential suite.** Add a scripted-model multi-process E2E covering `controlplane`, `brain`, `modelgateway`, `executor`/`worker`, and `gate`, plus expanded SDK/recorded wire differential checks filtered by `docs/DIVERGENCES.md`.
7. **Slice 7 — Verification stamping and commit gate hook.** Add `.claude/hooks/verify-commit-gate.sh` with `off`/`observe`/`enforce` modes (defaulting to `observe` with an escape valve after two consecutive blocks) and `Verified-By:` commit trailer support.
8. **Slice 8 — Reviewer meta-evaluation and pruning policy.** Add `tools/revieweval` with historical incident/revert PRs and clean control PRs, stage-by-stage recall scoring, optional config-driven LLM execution, and a routine rule-pruning policy.

## Decisions

1. **Zero new external test dependencies.** Property tests use Go's built-in `testing.F` and lightweight deterministic generators so `go.mod` stays minimal and `internal/domain` remains stdlib-only.
2. **Template cloning preserves `FreshDB`.** `pgtest.NewPool` clones a once-migrated `pgtest_template` database (`CREATE DATABASE ... TEMPLATE`) serialized by a mutex; `pgtest.FreshDB` still returns an empty un-migrated database so migration tests exercise every migration from scratch.
3. **Kernel-verified dead-owner reaping on Linux only.** `dockertest` records `boot_id`, PID namespace inode, PID, and `/proc/<pid>/stat` start ticks in one label and reaps an underage container only when both `boot_id` and PID namespace match the current host and the process is proven gone or recycled. Different boots, foreign PID namespaces, non-Linux hosts, and unlabelled containers fall back conservatively to `strayAge`.
4. **Gate isolation.** Review guidelines in `REVIEW.md` are read from `origin/main` so a PR cannot weaken the rules judging it.

## Pull requests

One focused PR per slice, merged sequentially through CI before the next slice starts.
