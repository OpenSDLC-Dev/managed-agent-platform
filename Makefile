# The single executable source of the merge gate. CI invokes these targets and
# the verifier runs `make verify`; prose (CLAUDE.md, AGENTS.md, README.md)
# names targets instead of duplicating commands. Mirrors
# .github/workflows/ci.yml — a new check lands in both, in the same PR.

# Multi-command recipes open with `set -euo pipefail`, matching (and slightly
# hardening) the `bash -e` GitHub Actions ran the old inline steps with: a
# failing `gofmt -l` or `go list` aborts the step instead of passing an empty
# result downstream. Deliberately NOT via .SHELLFLAGS — macOS ships GNU Make
# 3.81, which silently ignores it (introduced in 3.82); the inline `set` works
# on every make. bash (not sh) is required for pipefail.
SHELL := /usr/bin/env bash

# Serial always: cover-gate consumes the coverage.out that test writes, and
# that ordering lives in the `verify` prerequisite list — which only serial
# make honors. Nothing here benefits from -j (go build/test parallelize
# internally), so refuse it rather than gate on a stale profile.
.NOTPARALLEL:

.PHONY: build crossbuild vet fmt-check test cover-gate verify eval

build:
	go build ./...

# The BYOC worker is meant to cross-compile; 32-bit-only breakage
# (e.g. an untyped 64-bit const) is invisible to the host build.
crossbuild:
	GOOS=linux GOARCH=arm go build ./internal/...

vet:
	go vet ./...

# gofmt walks the filesystem rather than the module, so unlike `go vet ./...` it
# does not skip dot-directories — and .claude/worktrees holds whole checkouts of
# this same repo. Without this, a parallel session's half-typed file fails THIS
# checkout's gate, which is precisely the interference worktrees exist to
# prevent. Each worktree's own `make verify` covers its own files.
#
# -prune, not a -path filter: a filter still descends and only withholds the
# match, so an unreadable directory in a sibling worktree makes find itself
# error out and takes the gate down with it under `set -e` — the same failure
# through a different door. -prune never enters.
fmt-check:
	@set -euo pipefail; \
	unformatted="$$(find . -path ./.claude -prune -o -name '*.go' -exec gofmt -l {} +)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:" >&2; \
		echo "$$unformatted" >&2; \
		exit 1; \
	fi

# Coverage denominator: logic packages only. internal/pgtest,
# internal/sandbox/sandboxtest, internal/modeltest, internal/blob/blobtest and
# internal/provider/providertest are test support —
# packages solely because a test in another package must import them. What is
# uncovered in them are the branches no unit test can reach: the ones that fire
# when a suite fails, when a live tier is misconfigured, or only under the
# opt-in tiers themselves. Counting those measures nothing and dilutes the
# gate, exactly as cmd/ main glue would.
test:
	@set -euo pipefail; \
	coverpkg="$$(go list ./internal/... | grep -vE '/(pgtest|sandboxtest|modeltest|blobtest|providertest)$$' | paste -sd, -)"; \
	set -x; \
	go test -count=1 -coverpkg="$$coverpkg" -coverprofile=coverage.out ./...

# Gates the coverage.out that `make test` (or the CI test step) just wrote —
# it deliberately does NOT depend on `test`, so CI can run the two as separate
# named checks without re-running the suite. Standalone `make cover-gate`
# therefore judges whatever profile is on disk: run it via `make verify` or
# right after `make test`.
# Exact total from the profile: `go tool cover -func` rounds to 0.1%, which
# would let ~89.95% pass as "90.0". Duplicate blocks (same package covered by
# several test binaries) merge with OR semantics; an empty profile fails closed.
cover-gate:
	@awk 'NR>1 { stmts[$$1]=$$2; if ($$3>0) covered[$$1]=1 } \
	     END { \
	       for (k in stmts) { t+=stmts[k]; if (k in covered) c+=stmts[k] } \
	       if (t==0) { print "no statements in coverage profile"; exit 1 } \
	       pct=100*c/t; \
	       printf "total statement coverage: %.2f%%\n", pct; \
	       exit !(pct >= 90.0) \
	     }' coverage.out

verify: build crossbuild vet fmt-check test cover-gate

# The live end-to-end eval suite: whole sessions through the public API against
# the .env endpoint and real Docker sandboxes. NOT part of `verify` — it spends
# money and takes minutes — and deliberately not a coverage run:
#
#   - RUN_EVALS is command-scoped, never exported. An exported opt-in would make
#     a later `make verify` in the same shell both call the model and count the
#     eval packages toward the gate. modeltest reads it as consent (any non-empty
#     value); the endpoint itself still comes from .env.
#   - No -coverprofile: it would overwrite the coverage.out that cover-gate
#     grades, and the eval packages are test-only besides.
#   - 60m because a trial waits on a real model and real containers; the per-turn
#     timeout inside the harness is the real guard, this is the outer backstop.
#
# Artifacts land in evals/artifacts/ (gitignored): report.json, summary.md, and
# one transcript per failed trial.
eval:
	RUN_EVALS=1 go test -count=1 -v -timeout 60m ./evals/...
