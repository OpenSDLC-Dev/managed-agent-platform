---
status: in-progress
---

# Plan 27 — release management

## Why

One release exists (v0.1.0, 2026-07-17) and its process lived only in PR #82's
description: a hand-moved changelog section and a lightweight tag, publishing
nothing. Three weeks later CHANGELOG.md held 5,300+ lines (85% of the file)
under a single `[Unreleased]` heading with 28 repeated `###` groups — the
Keep-a-Changelog shape had stopped carrying information — and every PR
inserted at the same top-of-file position, a standing merge-conflict surface
for this repo's parallel-worktree workflow. Meanwhile nothing was published:
no images (the Helm values said "build and push your own"), no chart artifact,
no worker binary, no GitHub Release, and no version embedded in any binary.

Approved decisions (user, 2026-08-07): a release produces **all four artifact
groups** (GitHub Release + notes, GHCR images, OCI Helm chart, worker
binaries); releases are **plan-archive driven**; versioning stays **SemVer
0.x** (minor for new capability/wire surface, patch for fix-only; 1.0 reserved
for an explicit stability promise, no timetable); the changelog moves to a
**fragment directory**. Publishing is CI-built from a tag — artifacts come
from a public, reproducible workflow run, not a maintainer's laptop.

## Decisions

1. **Versioning / cadence** — as approved above; platform version, chart
   `version`, and chart `appVersion` move in lockstep (one release train).
   Annotated tags `vX.Y.Z` on the release PR's squash-merge commit.
2. **Changelog fragments** — `changelog.d/<slug>.<section>.md`, body = the
   final entry verbatim (see changelog.d/README.md). `tools/changelog`
   (outside `internal/`, so outside the coverage denominator; its tests run
   under `make test`) assembles fragments into a dated section — KaC group
   order, entries newest-first by adding commit — then appends the legacy
   `[Unreleased]` body byte-identical (a one-time affordance for the first
   cut), leaves a fixed pointer paragraph as the new `[Unreleased]` body, and
   advances the link references. `notes` extracts a section for release
   notes. Make entry points: `changelog`, `changelog-notes`. No CI
   enforcement of fragment presence — the verifier's docs-consistency rung
   polices it, as it policed direct entries.
3. **Version embedding** — `internal/version.Version` (a bare var, `"dev"`
   default) injected via `-ldflags -X` from the Makefile and a Dockerfile
   `ARG`; every binary logs it at startup; the worker — the one binary users
   run standalone — also gets `--version`. **No version API endpoint**: that
   would be net-new wire surface.
4. **`release.yml`** — triggered by `v*` tag push; `GITHUB_TOKEN` only
   (`contents: write`, `packages: write`). Steps are Make targets the
   workflow merely invokes (the Makefile stays the one executable source;
   local runs build without pushing): a tag-sanity gate (tag on `main`,
   version matches the top released changelog section), multi-arch
   (amd64+arm64) image build — the one server image pushed under the three
   component names the chart already composes,
   `ghcr.io/opensdlc-dev/managed-agent-platform/{controlplane,brain,executor}`,
   plus `…/gate` from the gate target; **no `latest` tag** (the chart derives
   its default tag from `appVersion`; a mutable alias only invites drift);
   `helm package` + push to `oci://ghcr.io/opensdlc-dev/charts` (a separate
   namespace so the chart is not mistaken for a fourth component); worker
   binaries for linux/darwin × amd64/arm64 (`-trimpath`, version injected,
   tar.gz + sha256sums; no Windows — the worker drives Docker sandboxes and
   has no Windows user story); `gh release create` with notes from
   `changelog-notes`, binaries attached — **clamped**: GitHub caps a Release
   body at 125,000 characters, and the v0.2.0 first cut (which absorbs the
   5,300-line legacy backlog) exceeds it, so the workflow publishes the
   section body when it fits and otherwise the fragment-group prefix plus a
   link to the full CHANGELOG.md section. Re-running the workflow on the same
   tag re-publishes identical artifacts, so partial-failure recovery is a
   re-run. These targets are NOT part of `make verify`, like the `gcp-*`
   group.
5. **Ritual and governance** — the operative ritual is docs/RELEASING.md. A
   release PR takes the **full dual-review tier** (it touches `Chart.yaml`
   and the deploy surface; LIGHT tier stays docs-only). CLAUDE.md/AGENTS.md
   step-2 wording and the verifier's docs rung move from "a CHANGELOG.md
   entry" to "a `changelog.d/` fragment"; only a release PR edits
   CHANGELOG.md, via `make changelog`.

## Slices

1. **Fragment mechanism** — `changelog.d/` + README, `tools/changelog` with
   its test suite (mutation-checked), Make targets, docs/RELEASING.md, the
   governance rewording (CLAUDE.md, AGENTS.md, `.claude/agents/verifier.md`).
   From this PR on, new PRs write fragments.
2. **Version embedding** — `internal/version`, Makefile LDFLAGS, Dockerfile
   ARG, startup logs, worker `--version`.
3. **Publishing pipeline** — `release.yml` + the release Make targets +
   deploy-doc updates (Helm values comment, README install pointers,
   RELEASING.md's "what the tag triggers" section goes live).
4. **Cut v0.2.0** — the acceptance run of the whole scheme: release PR
   (assembles the legacy backlog plus accumulated fragments, bumps the chart,
   README status line), annotated tag, `release.yml` publishes everything;
   verified by a kind `helm install` from the published OCI chart and GHCR
   images and a `--version` check on a downloaded worker binary. Acceptance
   record to docs/HISTORY.md; the plan archives.

## Acceptance

- A PR's changelog obligation is one added file; parallel PRs no longer
  contend for CHANGELOG.md's insertion point (a same-slug add/add collision
  stays possible and resolves by renaming one file).
- `make changelog VERSION=0.2.0` moves the legacy body byte-identically
  (diff shows only the seams) and consumes the fragments.
- After slice 4: `helm install` from `oci://ghcr.io/opensdlc-dev/charts`
  with default values pulls the published GHCR images and the platform runs
  on kind; the GitHub Release for v0.2.0 carries the clamped notes (decision
  4) and the worker tarballs; the released worker binary prints `0.2.0`.
