# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); released sections
group entries newest-first by the PR that landed them.

A change and its changelog entry land in the **same PR** — the entry as a
fragment in [changelog.d/](./changelog.d/), its body the final entry verbatim.
A release PR assembles the fragments into a dated section here (`make
changelog`; see [docs/RELEASING.md](./docs/RELEASING.md)); post-release, the
section moves to [docs/changelog/](./docs/changelog/) behind the index stub
below (`make changelog-archive`, relative links re-based, byte-reversibly);
no other PR edits this file. The fragment is the **one place a change's
narrative is written**: [docs/HISTORY.md](./docs/HISTORY.md) holds only what
a changelog structurally cannot (acceptance-run and review-hardening records,
decisions evaluated and rejected, archived plans' progress summaries), never
a second copy of an entry here.

## [Unreleased]

Unreleased changes accumulate as one fragment file per PR in
[changelog.d/](./changelog.d/) and are assembled into a dated section here
by `make changelog` at release time (see [docs/RELEASING.md](./docs/RELEASING.md)).

## [0.2.0] - 2026-08-07

Added · Changed · Fixed · Security — the full section lives in [docs/changelog/0.2.0.md](./docs/changelog/0.2.0.md).

## [0.1.0] - 2026-07-17

Added · Changed · Fixed — the full section lives in [docs/changelog/0.1.0.md](./docs/changelog/0.1.0.md).

[Unreleased]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/OpenSDLC-Dev/managed-agent-platform/releases/tag/v0.1.0
