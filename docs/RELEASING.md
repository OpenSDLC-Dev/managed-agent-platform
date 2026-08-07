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

> **Not yet built.** The tag-triggered publishing workflow (`release.yml`:
> GHCR images for server components and the gate, the Helm chart as an OCI
> artifact, worker binaries, and a GitHub Release whose notes come from
> `make changelog-notes VERSION=X.Y.Z [OUT=file]` — clamped to GitHub's
> 125,000-character body cap with a link to the full CHANGELOG.md section
> when the section is bigger, as the first cut's absorbed backlog will be)
> arrives with plan 27 slice 3. Until it lands, a tag publishes nothing and
> a release is the tag plus the assembled changelog.
