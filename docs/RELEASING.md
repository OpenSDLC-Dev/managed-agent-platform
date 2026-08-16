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
   dated CHANGELOG.md section and advances the link references. Assembly is
   pure concatenation, and the section it writes is **frozen on arrival**: it
   must reproduce byte-for-byte from its source fragments, so no entry is
   edited after folding. The v0.2.0 cut refreshed one stale figure in the
   assembled text and reverted it under review. A defect in an entry is fixed
   in the fragment before this step runs, never in the section after.
4. Bump `deploy/helm/managed-agent-platform/Chart.yaml`: `version` and
   `appVersion`, both to `X.Y.Z`.
5. Update README.md's status line if the release changes what it says.
6. Re-point in-repo citations of the section that just moved: search the
   docs with a **fixed-string** match — `grep -rF '§ [Unreleased]' docs/`
   (without `-F`, the brackets are a regex character class and the literal
   never matches) — and retarget each hit to the new dated section.
   docs/DIVERGENCES.md and docs/HISTORY.md cite it as an evidence anchor
   under more than one phrasing (`CHANGELOG.md § […]`, `CHANGELOG § […]`);
   the v0.2.0 cut missed seven variant-phrased citations with a narrower
   pattern. Step 3 also *deletes* every fragment it folds in, so a citation of
   one is left pointing at nothing — and it may spell the fragment three ways:
   full path, bare filename, or bare slug. Grep the **slug**, which is a
   substring of all three, over the whole tree from the repo root:
   `for f in $(git diff main...HEAD --diff-filter=D --name-only -- changelog.d); do n=${f##*/}; git grep -nF "${n%.*.md}" -- . ':!CHANGELOG.md'; done`
   The deleted-file list is the enumeration rather than `main:changelog.d`: it
   names exactly what this cut consumed, and cannot drift with a fragment that
   lands on main mid-review. Retarget each hit. CHANGELOG.md is excluded
   because a just-folded entry may name a fragment in past-tense narrative, and
   step 3 froze that text — a dead path there records what was fixed; it is not
   a live citation. The v0.3.0 cut found three, all in one archived plan, and a
   filename-only grep saw only two of them.
7. Normal PR flow — verifier plus **full dual review** (the PR touches
   `Chart.yaml`, and a release PR changes the deploy surface; it is not
   LIGHT-tier docs), CI green, threads settled, squash merge.
8. Tag the squash-merge commit and push the tag:
   `git tag -a vX.Y.Z -m "vX.Y.Z" <merge-sha> && git push origin vX.Y.Z`.
   **If anything the tag runs was itself fixed after that merge, tag the later
   commit instead** — every step below executes the tooling as of the tagged
   commit, so a fix landing after the release PR is simply not in the release
   unless the tag reaches it. `release-tag-check` allows this: it asks that
   the changelog's newest released section is X.Y.Z and that the commit is on
   `origin/main`, not that the commit is the release PR's. This rule exists
   because v0.3.0's notes-link fix landed after its own release PR.
9. After the release run is green: `make changelog-archive VERSION=X.Y.Z` in
   the next docs PR (normally the one archiving the plan that drove the
   release) — the section moves to `docs/changelog/X.Y.Z.md` behind an index
   stub, byte-reversibly (the tool re-bases relative links for the new
   directory and refuses any move it cannot invert). The section must still
   be inline at tag time (step 2 of the trigger list renders the notes from
   it), and the stub keeps the dated heading, so existing
   `CHANGELOG.md § [X.Y.Z]` citations resolve through it — no citation
   sweep.

## What the tag triggers

`release.yml` (`.github/workflows/release.yml`) fires on any `v*` tag (the
version is validated as strict SemVer before it touches a shell) and
publishes everything with `GITHUB_TOKEN` alone. Every build/publish step is
a root-Makefile target the workflow merely sequences — registry logins and
the final `gh release` calls are the workflow's own glue — and each target
also runs locally, where without `PUSH=1` nothing leaves the machine:

1. Every pure check, before anything publishes — a bad tag must fail while
   the run is still free to fail: `make release-tag-check VERSION=X.Y.Z`
   (the version is the changelog's newest released section, i.e. the
   release PR merged first, and the tagged commit sits on `origin/main`)
   and `make release-chart-check VERSION=X.Y.Z` (Chart.yaml's `version`
   and `appVersion` both already bumped).
2. `make changelog-notes VERSION=X.Y.Z OUT=notes.md CAP=120000` — the
   release notes, rendered up front for the same reason: whole leading
   Keep-a-Changelog groups under GitHub's 125,000-character body cap, then
   a link to the full CHANGELOG.md section (the first cut's absorbed
   backlog exceeds the cap). The section's relative links are rewritten
   absolute at the tag on the way out: a release body is read off the
   release page, where a repo-root-relative target resolves against that
   page and 404s. This renders with the tooling **at the tagged commit**,
   so a fix to it must be tagged to take effect.
3. `make release-images PUSH=1 VERSION=X.Y.Z` — one server build
   (linux/amd64 + arm64; the build stage cross-compiles rather than
   emulating the Go toolchain) pushed as
   `ghcr.io/opensdlc-dev/managed-agent-platform/{controlplane,brain,executor}:X.Y.Z`
   (same digest, three names — the coordinates the Helm chart composes) plus
   `…/gate:X.Y.Z` from the gate target. Deliberately no `latest` tag.
4. `make release-chart PUSH=1 VERSION=X.Y.Z` — the chart to
   `oci://ghcr.io/opensdlc-dev/charts` (its guards re-run as a
   prerequisite).
5. `make release-binaries VERSION=X.Y.Z` — version-stamped worker tarballs
   for linux/darwin × amd64/arm64, plus a sha256sums file.
6. The GitHub Release, from the notes rendered in step 2: created if
   missing, then reconciled (`gh release edit` republishes a stuck draft
   and refreshes title/notes), then assets uploaded with `--clobber`.

Re-running the workflow on the same tag rebuilds equivalent artifacts from
the same commit and converges the release, so partial-failure recovery is a
re-run. (Equivalent, not byte-identical: the base images float and archives
carry build timestamps.) One first-publish note: packages created by
`GITHUB_TOKEN` start **private** — flip the four image packages and the chart
to public once, in the org's package settings, so anonymous pulls work.
