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

.PHONY: build crossbuild vet fmt-check test cover-gate verify eval \
	gcp-fmt gcp-validate gcp-split-check gcp-lint gcp-bootstrap-test gcp-dbinit-test gcp-split-check-test gcp-foundation-apply gcp-bootstrap gcp-env-apply gcp-db-init gcp-env-destroy gcp-env-rebuild

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
# internal/sandbox/sandboxtest, internal/modeltest, internal/blob/blobtest,
# internal/provider/providertest, internal/secrets/secretstest,
# internal/secrets/gcpkms/gcpkmstest and internal/webtool/webtooltest are test support —
# packages solely because a test in another package must import them. What is
# uncovered in them are the branches no unit test can reach: the ones that fire
# when a suite fails, when a live tier is misconfigured, or only under the
# opt-in tiers themselves. Counting those measures nothing and dilutes the
# gate, exactly as cmd/ main glue would.
test:
	@set -euo pipefail; \
	coverpkg="$$(go list ./internal/... | grep -vE '/(pgtest|sandboxtest|modeltest|blobtest|providertest|secretstest|gcpkmstest|webtooltest)$$' | paste -sd, -)"; \
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

# ---------------------------------------------------------------------------
# GCP staging environment (docs/plan/20, Decision 9). Developer tooling for GCP
# deployment only: NEVER a prerequisite of `verify`, never a dependency of the
# platform or its build, and the Helm chart stays the portable install path.
# Terraform is not installed by this repo — `brew install hashicorp/tap/terraform`
# or the equivalent.
#
# The two configurations are not symmetric and the targets say so. `foundation/`
# has an apply and no destroy, because KMS key rings and crypto keys cannot be
# deleted and the vault ciphertext in Postgres is decryptable by that key and
# nothing else. `environment/` has both.
# ---------------------------------------------------------------------------

GCP_TF ?= terraform

# fmt and validate need no credentials and no state, which is what lets CI run
# them on every PR: the configuration cannot rot silently between the rare runs
# that actually touch GCP.
gcp-fmt:
	$(GCP_TF) fmt -check -recursive deploy/gcp

gcp-validate:
	set -euo pipefail; \
	for d in deploy/gcp/foundation deploy/gcp/environment; do \
		echo "==> $$d"; \
		$(GCP_TF) -chdir=$$d init -backend=false -input=false >/dev/null; \
		$(GCP_TF) -chdir=$$d validate; \
	done

# The structural half of Decision 9: environment/ may not OWN an unrecoverable
# resource kind, and foundation/ must protect every one it declares. Expressed
# over kinds rather than a list of addresses, so adding a resource cannot slip
# past it and moving one between files cannot false-fail it.
gcp-split-check:
	python3 deploy/gcp/check_split.py

gcp-lint:
	shellcheck deploy/gcp/bootstrap.sh deploy/gcp/dbinit.sh

# shellcheck cannot know that `gcloud secrets versions describe` rejects
# `--filter`, and it exited 0 on a bootstrap.sh that aborted on its first call in
# every project. This RUNS the script against a fake gcloud — credential-free,
# free of charge, and the only check in this group that would have caught that.
gcp-bootstrap-test:
	python3 deploy/gcp/bootstrap_test.py

# terraform validate cannot execute SQL and shellcheck cannot execute psql, so
# without this the only thing that ever runs dbinit.sql is a billable cluster
# talking to a billable database. This runs it against a real PostgreSQL 16 with
# TLS on, and drives three of its assertions red on purpose — an assertion that
# cannot fail is a comment. Needs Docker.
gcp-dbinit-test:
	python3 deploy/gcp/dbinit_test.py

# The split guard run against the real tree proves only that the real tree is
# compliant — a checker that had silently stopped reading would pass that too,
# and twice did. This plants violations in a scratch copy and requires each one
# to come back red, plus decoys that must stay green.
gcp-split-check-test:
	python3 deploy/gcp/check_split_test.py

# Adds the first version of each Secret Manager secret and creates the GCS HMAC
# key. Runs BETWEEN the two applies: the foundation creates the secrets empty,
# this fills them, and environment/ reads them ephemerally. Idempotent by
# skipping — a secret that already has a version is left alone, because that
# version may be the only thing that can decrypt something.
gcp-bootstrap:
	PROJECT=$(PROJECT) NAME_PREFIX=$(or $(NAME_PREFIX),map) bash deploy/gcp/bootstrap.sh

# Applies below cost money and are interactive on purpose — no -auto-approve.
# Read the plan before the first one.
gcp-foundation-apply:
	$(GCP_TF) -chdir=deploy/gcp/foundation init -input=false
	$(GCP_TF) -chdir=deploy/gcp/foundation apply

gcp-env-apply:
	$(GCP_TF) -chdir=deploy/gcp/environment init -input=false
	$(GCP_TF) -chdir=deploy/gcp/environment apply

# Creates the platform's database role OUTSIDE cloudsqlsuperuser and asserts it.
# Runs as a Job because environment/ gives Cloud SQL a private address only:
# the instance is reachable from the VPC and not from here. Needs kubectl
# pointed at the cluster — see the get-credentials line in the `zone` output.
gcp-db-init:
	PROJECT=$(PROJECT) NAME_PREFIX=$(or $(NAME_PREFIX),map) NAMESPACE=$(or $(NAMESPACE),map) \
		bash deploy/gcp/dbinit.sh

# Destroys the staging DATABASE along with everything else, and vault ciphertext
# lives only in Postgres — retaining the KMS key does not bring a deleted row
# back. Correct for staging, stated here rather than discovered.
gcp-env-destroy:
	$(GCP_TF) -chdir=deploy/gcp/environment destroy

# The teardown proof of Decision 9, as one target: create -> destroy -> create.
# What it proves is that the second apply succeeds with no KMS name collision
# and that the foundation's surviving secrets still reconcile against freshly
# created resources.
gcp-env-rebuild: gcp-env-destroy gcp-env-apply
