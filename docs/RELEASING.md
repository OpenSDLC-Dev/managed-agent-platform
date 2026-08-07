# RELEASING.md — how a release is cut

The release scheme was designed in [docs/plan/27_release-management.md](./plan/27_release-management.md).
This file is the operative ritual; day-to-day changelog mechanics live in
[changelog.d/README.md](../changelog.d/README.md).

## Versioning policy

- **SemVer, 0.x for now.** The bump is mechanical from what the release
  contains: any group other than `Fixed`/`Security` — `Added`, `Changed`,
  `Deprecated`, `Removed`, i.e. new capability or changed behavior —
  → **minor**; only `Fixed`/`Security` → **patch**.
- **1.0 has no timetable.** It is reserved for an explicit stability promise
  (wire-compat surface freeze plus config/deploy surface freeze) and is not
  managed by this process.
- **One release train, one number:** the platform version, the Helm chart
  `version`, and its `appVersion` move in lockstep. A chart-only fix ships as
  a normal (patch) release.
- Tags are **annotated**, `vX.Y.Z`, on the release PR's squash-merge commit.
  (`v0.1.0` predates this scheme and is a lightweight tag; it stays as is.)

## When to release

Releases are **plan-archive driven**: cut one when an archived plan (or a batch
of them) completes a capability worth shipping — a human judgment, not a
schedule. Preconditions: `main` is green and STATE.md's active work is not
mid-slice in something the release would half-ship.

## The ritual

1. Decide the version from the pending content (`ls changelog.d/`; the policy
   above makes the bump mechanical).
2. Branch: `git checkout -b release/vX.Y.Z` off a fresh `main`.
3. `make changelog VERSION=X.Y.Z` — folds every `changelog.d/` fragment (and,
   for the first cut under this scheme, the legacy `[Unreleased]` body) into a
   dated CHANGELOG.md section and advances the link references.
4. Bump `deploy/helm/managed-agent-platform/Chart.yaml`: `version` and
   `appVersion`, both to `X.Y.Z`.
5. Update README.md's status line if the release changes what it says.
6. Re-point in-repo citations of the section that just moved: grep the docs
   for `CHANGELOG.md § [Unreleased]` (docs/DIVERGENCES.md and docs/HISTORY.md
   cite it as an evidence anchor) and retarget each to the new dated section.
7. Normal PR flow — verifier plus **full dual review** (the PR touches
   `Chart.yaml`, and a release PR changes the deploy surface; it is not
   LIGHT-tier docs), CI green, threads settled, squash merge.
8. Tag the squash-merge commit and push the tag:
   `git tag -a vX.Y.Z -m "vX.Y.Z" <merge-sha> && git push origin vX.Y.Z`.

## What the tag triggers

`release.yml` (`.github/workflows/release.yml`) fires on any `v*` tag and
publishes everything with `GITHUB_TOKEN` alone, invoking only root-Makefile
targets — each also runs locally, and without `PUSH=1` nothing leaves the
machine:

1. `make release-tag-check VERSION=X.Y.Z` — the version must be the
   changelog's newest released section (the release PR merged first) and the
   tagged commit must sit on `origin/main`; nothing publishes otherwise.
2. `make release-images PUSH=1` — one server build (linux/amd64 + arm64)
   pushed as
   `ghcr.io/opensdlc-dev/managed-agent-platform/{controlplane,brain,executor}:X.Y.Z`
   (same digest, three names — the coordinates the Helm chart composes) plus
   `…/gate:X.Y.Z` from the gate target. Deliberately no `latest` tag.
3. `make release-chart PUSH=1` — the chart (whose `version`/`appVersion` the
   release PR already bumped) to `oci://ghcr.io/opensdlc-dev/charts`.
4. `make release-binaries` — version-stamped worker tarballs for
   linux/darwin × amd64/arm64, plus a sha256sums file.
5. `gh release create vX.Y.Z` with the binaries attached and notes from
   `make changelog-notes VERSION=X.Y.Z OUT=notes.md CAP=120000` — whole
   leading Keep-a-Changelog groups under GitHub's 125,000-character body
   cap, then a link to the full CHANGELOG.md section (the first cut's
   absorbed backlog exceeds the cap).

Re-running the workflow on the same tag re-publishes identical artifacts, so
partial-failure recovery is a re-run (an existing release gets its assets
re-uploaded with `--clobber`). One first-publish note: packages created by
`GITHUB_TOKEN` start **private** — flip the four image packages and the chart
to public once, in the org's package settings, so anonymous pulls work.
