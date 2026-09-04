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
	changelog changelog-notes changelog-archive \
	release-tag-check release-images release-chart-check release-chart release-binaries \
	openbao-init-test cd-outcome-test parked-test retry-test identifiers-test pins-test registry-check \
	gcp-fmt gcp-validate gcp-split-check gcp-lint gcp-bootstrap-test gcp-dbinit-test gcp-split-check-test gcp-power-test gcp-tfvars-test gcp-env-targets-test gcp-foundation-apply gcp-bootstrap gcp-env-apply gcp-db-init gcp-env-destroy gcp-env-rebuild \
	gcp-require-project gcp-env-tfvars gcp-env-migrate-state gcp-env-init gcp-env-vars-match \
	gcp-env-stop gcp-env-start gcp-env-status

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

# Coverage denominator: logic packages only. internal/pgtest, internal/dockertest,
# internal/sandbox/sandboxtest, internal/modeltest, internal/blob/blobtest,
# internal/blob/gcs/gcstest, internal/provider/providertest, internal/secrets/secretstest,
# internal/secrets/gcpkms/gcpkmstest, internal/webtool/webtooltest and
# internal/identity/identitytest and internal/mcp/mcptest are test support —
# packages solely because a test in another package must import them. What is
# uncovered in them are the branches no unit test can reach: the ones that fire
# when a suite fails, when a live tier is misconfigured, or only under the
# opt-in tiers themselves. Counting those measures nothing and dilutes the
# gate, exactly as cmd/ main glue would.
#
# -timeout 30m, not go test's 10m default (#490): the two largest
# Postgres-backed suites, internal/api and internal/executor, now run about ten
# minutes each on a loaded 8-CPU box — the executor measured past the default at
# 613 s, the api just under at 591 s — and a package that dies at the ceiling
# reports `panic: test timed out` with a sub-second test "running", which reads
# like a hang rather than the budget it is. The default is sized for unit tests;
# these run a fixture container per binary and migrate a fresh database per
# test, which is where the minutes go. The per-test
# guards are the real limits — this is only the outer backstop, and a suite that
# genuinely wedges still fails, three times slower to say so. Raising it costs
# nothing when tests pass, and a timed-out binary skips the `defer` that removes
# its pgtest fixture, so the ceiling also fed the next run's contention. This
# buys room rather than fixing the cause: the per-test database creation behind
# the growth, and reaping a fixture whose owner died, are #499.
test:
	@set -euo pipefail; \
	coverpkg="$$(go list ./internal/... | grep -vE '/(pgtest|dockertest|sandboxtest|modeltest|blobtest|gcstest|providertest|secretstest|gcpkmstest|webtooltest|identitytest|mcptest)$$' | paste -sd, -)"; \
	set -x; \
	go test -count=1 -timeout 30m -coverpkg="$$coverpkg" -coverprofile=coverage.out ./...

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
#   - 120m because a trial waits on a real model and real containers; the
#     per-turn timeout inside the harness is the real guard, this is the outer
#     backstop — sized so the worst case of every task's own budget (5m per
#     ordinary turn, more for an outcome loop) still fits without the test
#     binary panicking mid-trial and losing the report, with headroom for the
#     one retry a model-classed failure earns (at most double, never a loop).
#
# Artifacts land in evals/artifacts/ (gitignored): report.json, summary.md, and
# one transcript per failed attempt (a retried-then-failed task leaves two).
eval:
	RUN_EVALS=1 go test -count=1 -v -timeout 120m ./evals/...

# Release-time changelog tooling (docs/RELEASING.md; the fragment format is
# changelog.d/README.md). NOT part of `verify`: `changelog` rewrites
# CHANGELOG.md, which only a release PR does, `changelog-notes` exists for
# the release workflow to extract a section as GitHub Release notes — with
# its relative links rewritten absolute at the tag, since that body is also
# served raw, where nothing supplies the repository base github.com's own
# renderer applies on the release page — and
# `changelog-archive` moves a released section to docs/changelog/ in the
# post-release PR (RELEASING.md step 9). The tool's own tests DO run under
# `make test` (./... includes ./tools/...); only the invocations are
# release-scoped.
changelog:
	go run ./tools/changelog assemble -version "$(VERSION)"

changelog-notes:
	@go run ./tools/changelog notes -version "$(VERSION)" -out "$(or $(OUT),-)" -cap "$(or $(CAP),0)"

changelog-archive:
	@go run ./tools/changelog archive -version "$(VERSION)"

# The registry's pointer guard (#452). Its shape rules already run inside
# `verify` — they are offline, and `go test ./...` runs tools/registrycheck's
# own test, which calls Check on the real docs/DIVERGENCES.md. This target is
# the other half: whether each live `Tracked: #N` still names an OPEN issue,
# which only GitHub can answer. NOT part of `verify`, for the one reason the
# gcp-* and eval groups are not either — the gate is offline and
# credential-free by design, and a check that reaches the network cannot be
# made to fail honestly inside it. .github/workflows/registry.yml runs this
# daily and on every PR that touches the registry; GITHUB_TOKEN is optional
# (the repository is public) and only raises the API rate limit.
registry-check:
	go run ./tools/registrycheck -issues

# ---------------------------------------------------------------------------
# Release publishing (docs/RELEASING.md; plan 27). Like the gcp-* group,
# NEVER part of `verify`: these build and push release artifacts. Each target
# is exactly what .github/workflows/release.yml invokes — the workflow only
# sequences them — and each runs locally: without PUSH=1 the image target
# builds linux/amd64 into the local daemon for a smoke run and nothing leaves
# the machine.
# ---------------------------------------------------------------------------

RELEASE_REGISTRY ?= ghcr.io/opensdlc-dev
RELEASE_IMAGE_NS  = $(RELEASE_REGISTRY)/managed-agent-platform
RELEASE_CHART_OCI ?= oci://$(RELEASE_REGISTRY)/charts
RELEASE_PLATFORMS ?= linux/amd64,linux/arm64
RELEASE_LDFLAGS   = -ldflags "-X github.com/OpenSDLC-Dev/managed-agent-platform/internal/version.Version=$(VERSION)"

# Tag sanity, run before anything publishes: the tag's version must be the
# changelog's newest released section (i.e. the release PR merged first), and
# the tagged commit must sit on origin/main.
release-tag-check:
	@set -euo pipefail; \
	test -n "$(VERSION)" || { echo "VERSION is required" >&2; exit 1; }; \
	latest="$$(go run ./tools/changelog latest)"; \
	if [ "$$latest" != "$(VERSION)" ]; then \
		echo "tag says $(VERSION) but the changelog's newest released section is $$latest — merge the release PR first" >&2; \
		exit 1; \
	fi; \
	git merge-base --is-ancestor HEAD origin/main || { echo "the tagged commit is not on origin/main" >&2; exit 1; }

# One server build pushed under the three component names the Helm chart
# composes ({registry}/{repository}/{component}:{tag} — same digest, three
# names), plus the gate from its own Dockerfile target. Deliberately no
# `latest` tag: the chart derives its default tag from appVersion, and a
# mutable alias only invites drift.
release-images:
	@set -euo pipefail; \
	test -n "$(VERSION)" || { echo "VERSION is required" >&2; exit 1; }; \
	mode="--load --platform linux/amd64"; \
	if [ "$(PUSH)" = "1" ]; then mode="--push --platform $(RELEASE_PLATFORMS)"; fi; \
	docker buildx build $$mode --build-arg VERSION="$(VERSION)" --target server \
		-t "$(RELEASE_IMAGE_NS)/controlplane:$(VERSION)" \
		-t "$(RELEASE_IMAGE_NS)/brain:$(VERSION)" \
		-t "$(RELEASE_IMAGE_NS)/executor:$(VERSION)" .; \
	docker buildx build $$mode --build-arg VERSION="$(VERSION)" --target gate \
		-t "$(RELEASE_IMAGE_NS)/gate:$(VERSION)" .

# The chart version must already equal VERSION — the release PR bumps
# Chart.yaml's version and appVersion in lockstep with the platform. A
# target of its own so the workflow can run it next to release-tag-check,
# before anything publishes: a half-bumped chart must fail the run while it
# is still free to fail, not after the images are already public.
release-chart-check:
	@set -euo pipefail; \
	test -n "$(VERSION)" || { echo "VERSION is required" >&2; exit 1; }; \
	grep -qxF 'version: $(VERSION)' deploy/helm/managed-agent-platform/Chart.yaml || { \
		echo "Chart.yaml version is not $(VERSION) — the release PR bumps it" >&2; exit 1; }; \
	grep -qxF 'appVersion: "$(VERSION)"' deploy/helm/managed-agent-platform/Chart.yaml || { \
		echo "Chart.yaml appVersion is not $(VERSION) — version and appVersion move in lockstep" >&2; exit 1; }

release-chart: release-chart-check
	@set -euo pipefail; \
	test -n "$(VERSION)" || { echo "VERSION is required" >&2; exit 1; }; \
	mkdir -p dist; \
	helm package deploy/helm/managed-agent-platform -d dist; \
	if [ "$(PUSH)" = "1" ]; then helm push "dist/managed-agent-platform-$(VERSION).tgz" "$(RELEASE_CHART_OCI)"; fi

# Worker binaries for the platforms BYOC users run. No Windows: the worker
# drives Docker sandboxes and has no Windows user story (plan 27 decision 4).
release-binaries:
	@set -euo pipefail; \
	test -n "$(VERSION)" || { echo "VERSION is required" >&2; exit 1; }; \
	mkdir -p dist; \
	for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		os="$${target%/*}"; arch="$${target#*/}"; \
		dir="dist/worker_$(VERSION)_$${os}_$${arch}"; \
		CGO_ENABLED=0 GOOS="$$os" GOARCH="$$arch" go build -trimpath $(RELEASE_LDFLAGS) -o "$$dir/worker" ./cmd/worker; \
		tar -czf "$$dir.tar.gz" -C "$$dir" worker; \
		rm -r "$$dir"; \
	done; \
	(cd dist && shasum -a 256 worker_$(VERSION)_*.tar.gz > "worker_$(VERSION)_sha256sums.txt"); \
	ls -l dist/worker_$(VERSION)_*

# ---------------------------------------------------------------------------
# The bundled OpenBao's init scripts. Outside `verify` for the same reason the
# GCP targets below are: deployment tooling, never a dependency of the platform
# or its build. CI runs it in the compose job.
# ---------------------------------------------------------------------------

# shellcheck has no opinion on the ORDER in which a redirect and the command
# filling it take effect. Nor does anything else here: the compose job starts
# postgres, minio, the control plane and the brain and never openbao, and the
# helm job renders the chart's init script without executing a line of it — so
# the two scripts that decide whether a self-hosted stack can encrypt anything
# were the part of this repo nothing ran. #439 was the bill: a failed init left
# a 0-byte init.json that the recovery branch read as a good one, leaving a
# vault whose root token existed nowhere and a stack repairable only by
# destroying its volumes. This RUNS both scripts against a fake `bao` — no
# Docker, no credentials — and reverts each half of that fix to require the
# checks guarding it to go red.
openbao-init-test:
	python3 deploy/compose/openbao_init_test.py

# The classifier deploy-alert.yml keys on, run rather than read — it is the one
# piece of shell here that the PR changing it cannot execute (see the script's
# own header). shellcheck rides along because this is the repo's fourth
# checked-in shell script and a lint target of its own, for one file, would be
# more machinery than the line it replaces.
cd-outcome-test:
	shellcheck .github/scripts/cd-outcome.sh
	python3 .github/scripts/cd_outcome_test.py

# The other one neither PR can execute: deploy.yml and staging-parked.yml both
# decide from it whether staging is parked, and the rule is "a label KEY
# beginning with `power-saved-`" — one `;` away from also matching a label
# VALUE. Kept a separate target from cd-outcome-test rather than folded into it
# so a red run names which of the two signals moved.
parked-test:
	shellcheck .github/scripts/parked.sh
	python3 .github/scripts/parked_test.py

# And the third: the `retry` wrapper around both notifiers' READ calls. It is
# NOT a checked-in script — the two copies were kept separate deliberately (see
# the test's header) — so this lifts each definition out of its workflow and
# runs it, which is why there is no shellcheck line above it. #507 is what it
# would have caught: a failed attempt's partial stdout landing in the caller's
# capture, joined to the attempt that worked.
retry-test:
	python3 .github/scripts/retry_test.py

# The one that checks a rule rather than a script: #355/#356 parameterised the
# operator's coordinates out of the repository by hand, the sweep stopped at
# `deploy/` and `.github/`, and #514 found what it had left in docs/. A rule kept
# by memory lapses, so this searches the documentation for the four shapes those
# coordinates take. It catches shapes, not the rule entire — the script's header
# says exactly what it cannot see — and it self-tests before it scans, because a
# broken pattern and a clean repository print the same thing otherwise.
identifiers-test:
	python3 .github/scripts/identifiers_test.py

# And the second rule kept only by memory, which lapsed the same way:
# `.github/dependabot.yml` says every action is pinned to a commit SHA, #472
# added two workflows that were not, and nothing in the repository could notice
# — so four mutable tags sat in jobs holding an OAuth token until Dependabot
# offered to swap one of them for another (#518). Shape only: whether a SHA IS
# the release its comment names needs the network, which a gate has not got.
# It holds the companion clause too — every `actions/checkout` drops the job's
# credential — which lapsed further still: three call sites never carried it
# while a released changelog said every one did (#558). That clause's own prose
# lives in the script's docstring, since Dependabot has no half in it.
pins-test:
	python3 .github/scripts/pins_test.py

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

# environment/'s state lives in the bucket foundation/ owns (#478), and a backend
# block cannot interpolate a variable — so the bucket arrives at init time. The
# name is DERIVED here exactly as foundation/main.tf composes it, rather than
# recorded as another coordinate, so there is nothing for an operator to copy
# between two places and get wrong.
#
# There is deliberately NO override variable. One existed and was removed in
# review: an override chooses the bucket while terraform.tfvars still chooses the
# resources, and gcp-env-vars-match — which compares the tfvars against PROJECT
# and NAME_PREFIX — cannot see it. That is a way to apply one environment's
# configuration to another's state with every guard green, which is the exact
# failure the guard exists to prevent, offered as a convenience.
#
# What an override was for: GCS bucket names are one global namespace and a
# project id reserves nothing in it, so `<project>-<prefix>-tfstate` could in
# principle already belong to somebody else. That fails the FIRST foundation
# apply, loudly, before any environment exists — and the remedy at that point is
# a different NAME_PREFIX, which both sides honour and which costs nothing
# because nothing has been named yet. An operator who somehow hits it later can
# change the two expressions together; that is a deliberate two-line edit, which
# is the right price for a divergence this dangerous.
#
# `override`, not `=`: a plain assignment still loses to `make TF_STATE_BUCKET=…`
# on the command line, which is the same bypass by another spelling.
NAME_PREFIX_OR_DEFAULT = $(or $(NAME_PREFIX),map)
# BOTH are `override`, and the second is not redundant. TF_BACKEND is what the
# recipes actually pass; overriding it on the command line replaces the whole
# definition, so `make TF_BACKEND='-backend-config=bucket=other' gcp-env-destroy`
# reaches an arbitrary bucket no matter how TF_STATE_BUCKET is declared — the
# same hazard, one variable further out. Locking only the inner one looked
# sufficient and was not.
#
# The rule for which variables need locking, since this moved outward twice
# before it stopped: everything DOWNSTREAM of the guard's inputs must be locked,
# and everything upstream must not be. gcp-env-vars-match compares the tfvars
# against PROJECT and NAME_PREFIX_OR_DEFAULT, so those two — and NAME_PREFIX
# behind them — move the bucket AND what the guard expects, together; overriding
# one is just choosing another environment, and a tfvars that disagrees refuses.
# TF_STATE_BUCKET and TF_BACKEND are past that comparison: they move the bucket
# alone, leaving the guard checking something it no longer decides. That is the
# whole difference, and it is why locking them is enough.
override TF_STATE_BUCKET = $(PROJECT)-$(NAME_PREFIX_OR_DEFAULT)-tfstate
override TF_BACKEND = -backend-config="bucket=$(TF_STATE_BUCKET)"

# An empty PROJECT would compose `-map-tfstate`, which is not a legal bucket name
# and fails several steps later with a message about the bucket rather than about
# the variable. Every target that needs the backend depends on this.
gcp-require-project:
	@if [ -z "$(PROJECT)" ]; then \
		echo "PROJECT is required — it is half of the state bucket's name." >&2; \
		echo "Run: PROJECT=your-project make <target>" >&2; \
		exit 2; \
	fi

# fmt and validate need no credentials and no state, which is what lets CI run
# them on every PR: the configuration cannot rot silently between the rare runs
# that actually touch GCP. `-backend=false` is what keeps that true now that
# environment/ declares a `backend "gcs"` block: it stops init from reaching for
# the bucket, so no GCS call and no credential are involved. Not the same as
# offline — init still installs providers from the registry, which is a network
# call CI makes and which the lock file pins.
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

# The second half is a portability guard shellcheck does not offer: it checks
# syntax and quoting, not which bash a construct needs. These scripts are run BY
# THE OPERATOR, on a laptop, and macOS has shipped bash 3.2.57 as /bin/bash for
# years (GPLv3 licensing, not inertia — Apple moved to zsh instead) — so a
# bash-4 builtin here is not a nicety, it is `make gcp-db-init` dying on the
# machine it was written for while CI (Linux, bash 5) stays green. `mapfile` got
# in exactly that way. A list of constructs, not an analysis, and honest about
# it: it catches a recurrence of this class, not every possible one.
gcp-lint:
	shellcheck deploy/gcp/bootstrap.sh deploy/gcp/dbinit.sh deploy/gcp/env-power.sh deploy/gcp/tfvars.sh
	@set -euo pipefail; \
	found=0; \
	for f in deploy/gcp/bootstrap.sh deploy/gcp/dbinit.sh deploy/gcp/env-power.sh deploy/gcp/tfvars.sh; do \
		if grep -nE '(^|[^[:alnum:]_-])(mapfile|readarray)[[:space:]]|declare[[:space:]]+-[A-Za-z]*A|\$${[A-Za-z_][A-Za-z0-9_]*(\^\^|,,)' "$$f"; then \
			echo "  ^ in $$f: needs bash 4; macOS ships 3.2" >&2; \
			found=1; \
		fi; \
	done; \
	if [ "$$found" -ne 0 ]; then exit 1; fi

# shellcheck cannot know that `gcloud secrets versions describe` rejects
# `--filter`, and it exited 0 on a bootstrap.sh that aborted on its first call in
# every project. This RUNS the script against a fake gcloud — credential-free,
# free of charge, and the only check in this group that would have caught that.
gcp-bootstrap-test:
	python3 deploy/gcp/bootstrap_test.py

# terraform validate cannot execute SQL and shellcheck cannot execute psql, so
# without this the only thing that ever runs dbinit.sql is a billable cluster
# talking to a billable database. This runs it against a real PostgreSQL 16 with
# TLS on, sets up every state its assertions guard and the DDL does not repair,
# and requires the run to go red for that reason — an assertion that cannot fail
# is a comment. Needs Docker.
gcp-dbinit-test:
	python3 deploy/gcp/dbinit_test.py

# The split guard run against the real tree proves only that the real tree is
# compliant — a checker that had silently stopped reading would pass that too,
# and twice did. This plants violations in a scratch copy and requires each one
# to come back red, plus decoys that must stay green.
gcp-split-check-test:
	python3 deploy/gcp/check_split_test.py

# Every claim env-power.sh makes is about a SEQUENCE of gcloud calls, and neither
# shellcheck nor a careful read checks a sequence. The two that cost real money:
# `start` restoring the sizes `stop` saved rather than a plausible constant, and
# the two orderings — nodes before the database going down, the database before
# the nodes coming up. This runs the script against a fake gcloud instead.
gcp-power-test:
	python3 deploy/gcp/env_power_test.py

# The tfvars generator turns one gcloud answer into a value Terraform accepts,
# and every way that goes wrong — a `projects/…/serviceAccounts/` prefix, a
# Windows CR — writes a file that looks right and fails variable validation. That
# happens at PLAN, so nothing is half-created; the cost is that it fails a
# `destroy` the same way, and a destroy is what you are running when the
# environment is already up and billing. Shellcheck cannot see any of it, so this
# RUNS the script against a fake gcloud, including the refusals: a disabled API,
# junk that is not an account, and an existing file never overwritten.
gcp-tfvars-test:
	python3 deploy/gcp/tfvars_test.py

# And the recipes below, which were the last unrun thing in this directory and
# the most dangerous: the guards deciding whether a destroy proceeds. Every one
# of them survived mutation while nothing ran them — deleting a
# `gcp-env-vars-match` prerequisite, deleting the empty-state refusal, changing
# the derived bucket. Runs `make` itself against a fake `terraform`, so what is
# checked is the recipe and its prerequisite ORDER, not a restatement of them.
gcp-env-targets-test:
	python3 deploy/gcp/env_targets_test.py

# Adds the first version of each Secret Manager secret — the two database
# passwords, since #240 retired the GCS HMAC key that was the third value here.
# Runs BETWEEN the two applies: the foundation creates the secrets empty,
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

# The two variables environment/ has no default for, written to the gitignored
# environment/terraform.tfvars. Run it after gcp-foundation-apply: the Cloud
# Build account it looks up does not exist until that API is on. Refuses to
# overwrite an existing file, which may carry settings it does not generate.
#
# OUT is pinned here for the same reason gcp-env-vars-match pins it, and the
# consequence is nastier on this side: generation READS OUT too, so a stray one
# writes the file somewhere Terraform will never look while reporting success —
# and the next `make gcp-env-apply` then fails telling you to run
# `make gcp-env-tfvars`, the command that just appeared to work.
#
# Of the three assignments below only NAME_PREFIX does work — it applies the
# `map` default. Make already exports a variable that came from the command line
# or the environment into every recipe, so PROJECT and KMS_LOCATION would reach
# the script without being named here; they are named to keep the script's whole
# input in one place. Deleting either is a no-op, which is why no test pins it.
gcp-env-tfvars: gcp-require-project
	PROJECT=$(PROJECT) NAME_PREFIX=$(NAME_PREFIX_OR_DEFAULT) KMS_LOCATION=$(KMS_LOCATION) \
		OUT=deploy/gcp/environment/terraform.tfvars \
		bash deploy/gcp/tfvars.sh

# Point a checkout at the state and stop, which is the capability the move to a
# bucket is FOR and the only one the expensive targets used to hide. Reading an
# output — which `dbinit.sh` does seven times, and which README quotes for the
# cluster name, the addresses and the registry — needs an initialized backend,
# and a partial backend cannot be initialized by the bare `terraform init` that
# Terraform's own error message suggests: it has no bucket. Without this the
# only way to get there is a full apply or a destroy, which is an absurd price
# for `terraform output -raw cluster_name`.
#
# It runs the coordinate guard like the others, despite changing nothing itself:
# what it leaves behind is a `.terraform` remembering a bucket, and every later
# `terraform output` reads from it — including the seven `gcp-db-init` reads
# before it writes to a database. A read-only target that selects the wrong
# state is a mutating one at a remove.
gcp-env-init: gcp-require-project gcp-env-vars-match
	$(GCP_TF) -chdir=deploy/gcp/environment init -input=false $(TF_BACKEND)

# PROJECT and NAME_PREFIX choose the state BUCKET; terraform.tfvars chooses the
# RESOURCES. Two independent inputs to one operation, and a disagreement between
# them is not a failure — it is a success against the wrong state. So assert they
# agree before init, every time. tfvars.sh --check argues it at length.
# OUT is PINNED, not inherited. tfvars.sh takes it from the environment so the
# test can redirect it, and make exports nothing but passes the environment
# through — so a stray OUT left over from anything would point the check at a
# file Terraform will not read, and it would pass.
gcp-env-vars-match: gcp-require-project
	@PROJECT=$(PROJECT) NAME_PREFIX=$(NAME_PREFIX_OR_DEFAULT) \
		OUT=deploy/gcp/environment/terraform.tfvars \
		bash deploy/gcp/tfvars.sh --check

# `-input=false` here is doing more than suppressing prompts, and is the reason
# running this BEFORE gcp-env-migrate-state is safe: on a machine that still
# holds local state, Terraform detects the backend change and refuses with
# "Can't ask approval for state migration when interactive input is disabled" —
# it does not quietly adopt an empty remote state. Verified, not assumed.
gcp-env-apply: gcp-require-project gcp-env-vars-match
	$(GCP_TF) -chdir=deploy/gcp/environment init -input=false $(TF_BACKEND)
	$(GCP_TF) -chdir=deploy/gcp/environment apply

# The one-time move of an existing LOCAL state into the bucket (#478), and it has
# to run from the machine that still holds that file — CI cannot do it, and
# neither can a fresh checkout. Deliberately without `-input=false`: the
# confirmation Terraform asks for before copying state is the point, not an
# obstacle. Afterwards every machine with credentials shares one state, and the
# `terraform.tfstate` left behind locally is a backup to delete once a remote
# `terraform plan` has come back clean.
gcp-env-migrate-state: gcp-require-project gcp-env-vars-match
	$(GCP_TF) -chdir=deploy/gcp/environment init -migrate-state $(TF_BACKEND)

# Creates the platform's database role OUTSIDE cloudsqlsuperuser and asserts it.
# Runs as a Job because environment/ gives Cloud SQL a private address only:
# the instance is reachable from the VPC and not from here. Needs kubectl
# pointed at the cluster — see the get-credentials line in the `zone` output.
#
# Guarded like the gcp-env-* targets, and for the reason the comment above
# gcp-env-init gives: dbinit.sh reads seven `terraform output` values, so it is a
# backend READER, and a reader picks up whichever bucket the last `init` in
# deploy/gcp/environment/ selected. Without this, `PROJECT=proj-a make
# gcp-env-init` followed by `PROJECT=proj-b make gcp-db-init` takes the database
# host, name and role from proj-a's outputs while dbinit.sh passes proj-b to
# gcloud for the cluster and the secrets — one half of the operation in each
# environment, and nothing failing to say so.
#
# gcp-env-init is a prerequisite rather than an assumption because the guard
# alone cannot see what `.terraform` was initialized against; re-selecting the
# backend is what makes the outputs come from the bucket PROJECT names. It is
# idempotent, and if the two disagree its -input=false refuses rather than
# silently migrating.
#
# The first two are SUBSUMED by the third — gcp-env-init requires both — so only
# deleting gcp-env-init changes behavior, and that is the one the suite pins.
# They are named anyway, to read like the four gcp-env-* targets above rather
# than leave this one looking unguarded.
gcp-db-init: gcp-require-project gcp-env-vars-match gcp-env-init
	PROJECT=$(PROJECT) NAME_PREFIX=$(NAME_PREFIX_OR_DEFAULT) NAMESPACE=$(or $(NAMESPACE),map) \
		bash deploy/gcp/dbinit.sh

# Destroys the staging DATABASE along with everything else, and vault ciphertext
# lives only in Postgres — retaining the KMS key does not bring a deleted row
# back. Correct for staging, stated here rather than discovered.
#
# It also needs the state and the tfvars, and before #478 that meant it needed
# THIS machine: on a checkout without the state file, destroy found nothing to
# destroy and reported success while the environment kept billing. The state is
# remote now and `make gcp-env-tfvars` regenerates the inputs, so this runs from
# anywhere with credentials.
#
# The empty-state check is what stops #478 surviving its own fix. A remote
# backend does not make an EMPTY state impossible — it makes it quiet. Init a
# bucket that has no state object yet, from a checkout that never held the local
# one, and Terraform starts a fresh empty state with no complaint; destroy then
# reports success over a running environment, which is the original bug wearing
# the new backend. `terraform state list` prints nothing and exits 0 on an empty
# state, so emptiness is the signal — but its exit STATUS has to be read too, or
# an expired credential and a genuinely empty state produce the same empty stdout
# and the same confidently wrong diagnosis.
#
# The cost is that destroy is deliberately no longer idempotent: an environment
# already destroyed refuses rather than succeeding over nothing. That is the
# trade — Terraform cannot tell "already gone" from "pointed at the wrong empty
# bucket", and between a needless refusal and a silent teardown of nothing, only
# one of them can bill you for a month.
gcp-env-destroy: gcp-require-project gcp-env-vars-match
	$(GCP_TF) -chdir=deploy/gcp/environment init -input=false $(TF_BACKEND)
	@resources="$$($(GCP_TF) -chdir=deploy/gcp/environment state list)" || { \
		echo "refusing: could not READ the state in $(TF_STATE_BUCKET) — the error is above." >&2; \
		echo "That is not the same as an empty state, and destroy must not guess which it is." >&2; \
		exit 1; \
	}; \
	if [ -z "$$resources" ]; then \
		echo "refusing: $(TF_STATE_BUCKET) holds no state for environment/." >&2; \
		echo "" >&2; \
		echo "Destroying an empty state would report success and tear down nothing," >&2; \
		echo "which is the failure #478 exists to remove. It means one of:" >&2; \
		echo "  - the environment is already destroyed — then there is nothing to do;" >&2; \
		echo "  - the state was never migrated, and still sits on the machine that" >&2; \
		echo "    applied it: run \`make gcp-env-migrate-state\` THERE first;" >&2; \
		echo "  - PROJECT/NAME_PREFIX name the wrong bucket." >&2; \
		exit 1; \
	fi
	$(GCP_TF) -chdir=deploy/gcp/environment destroy

# The teardown proof of Decision 9, as one target: create -> destroy -> create.
# What it proves is that the second apply succeeds with no KMS name collision
# and that the foundation's surviving secrets still reconcile against freshly
# created resources.
gcp-env-rebuild: gcp-env-destroy gcp-env-apply

# Parking, the cheap counterpart to destroying: the hourly charges stop and every
# resource — the database above all — stays. Unlike everything else in this
# group these need neither Terraform nor state nor tfvars, only credentials, so
# they run from any machine the operator happens to be at. env-power.sh's header
# says what they park and what keeps billing regardless.
gcp-env-stop:
	PROJECT=$(PROJECT) NAME_PREFIX=$(or $(NAME_PREFIX),map) bash deploy/gcp/env-power.sh stop

# NODES is forwarded explicitly rather than left to make's export rules, because
# the refusal this script prints when the saved sizes are gone tells the operator
# to set it, and that instruction has to work whichever way they pass it.
gcp-env-start:
	PROJECT=$(PROJECT) NAME_PREFIX=$(or $(NAME_PREFIX),map) NODES=$(NODES) \
		bash deploy/gcp/env-power.sh start

gcp-env-status:
	PROJECT=$(PROJECT) NAME_PREFIX=$(or $(NAME_PREFIX),map) bash deploy/gcp/env-power.sh status
