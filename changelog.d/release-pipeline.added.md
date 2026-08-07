- **The tag-triggered release pipeline lands** (plan 27 slice 3).
  `.github/workflows/release.yml` fires on a `v*` tag and publishes with
  `GITHUB_TOKEN` alone, invoking only root-Makefile targets so the release's
  executable source stays the Makefile: `release-tag-check` (the version must
  be the changelog's newest released section — served by the tool's new
  `latest` subcommand — and the commit must sit on `origin/main`; nothing
  publishes otherwise), `release-images` (one multi-arch server build pushed
  as `ghcr.io/opensdlc-dev/managed-agent-platform/{controlplane,brain,executor}:X.Y.Z`
  — same digest, three names, the coordinates the Helm chart composes — plus
  `…/gate:X.Y.Z`; deliberately no `latest` tag; without `PUSH=1` it builds
  linux/amd64 into the local daemon and nothing leaves the machine),
  `release-chart` (the chart, its `version` and `appVersion` both checked
  against the release PR's bump before anything publishes, to
  `oci://ghcr.io/opensdlc-dev/charts`), and `release-binaries`
  (version-stamped worker tarballs for linux/darwin × amd64/arm64 with
  sha256sums). The GitHub Release's notes come from `make changelog-notes
  CAP=120000`: the `notes` subcommand now clamps an over-cap section to whole
  leading Keep-a-Changelog groups plus a link to the full CHANGELOG.md
  section — GitHub rejects a body over 125,000 characters, and the first
  cut's absorbed legacy backlog (measured 443,570) exceeds it. Deploy docs go
  live accordingly: the Helm values/README image guidance now points at the
  published coordinates (from v0.2.0 onward) instead of "build and push your
  own", README gains the `helm install oci://…` path, and RELEASING.md's
  "What the tag triggers" section replaces its "Not yet built" placeholder —
  including the one-time note that `GITHUB_TOKEN`-created GHCR packages
  start private and need a public flip for anonymous pulls.
