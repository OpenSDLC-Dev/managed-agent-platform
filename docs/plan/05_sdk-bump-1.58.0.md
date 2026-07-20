---
status: in-progress
issue: "#120"
---

# Bump anthropic-sdk-go v1.56.0 → v1.58.0

The plan for [#120](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/120).

## Why this needs a plan file

Because the work is **wire-schema verification**, and
[`.claude/agents/issue-triage.md`](../../.claude/agents/issue-triage.md)'s judgment criteria make
that trigger fire "unconditionally, however well-scoped the issue already looks: the resolution
itself belongs in a plan, never improvised mid-implementation." The size of the diff is explicitly
not the test — a two-line `go.mod` change qualifies exactly as a large one would.

The substantive reason behind the rule: CLAUDE.md's wire-compatibility rules make the pinned
`anthropic-sdk-go` this project's **authoritative typed wire schema**, ahead of the `ant` CLI source
and behind only the public docs. Changing that pin changes what the repo is measured against. If a
bump silently moved a mirrored field, an enum value, or an ID prefix, every downstream check —
the verifier's wire-compat rung, external reviewers, docs/DIVERGENCES.md's registry — would start
grading against a contract nobody read. So the bump is a schema event first and a dependency update
second.

## What has to be resolved

Three questions, each answerable only by diffing the two versions in the module cache. They must be
answered *before* the pin moves, and their answers recorded whether or not they turn out to be
no-ops — a null result is the point, since it is what lets the next bump skip this work.

1. **Do the changed `betaagent.go` / `betamessage.go` / `betasession*.go` types alter any shape
   `internal/domain` or `internal/api` mirrors?** Answer field-by-field, not file-by-file.
2. **Does `shared/constant/constants.go` add stop reasons or event types the `{domain}.{action}`
   event taxonomy should carry?**
3. **Do the new `betatunnel*` / `betadream` surfaces imply managed-agents behavior worth recording
   in docs/DIVERGENCES.md?** The registry's remit is deliberate divergences and unconfirmed
   inferences; decide explicitly rather than by omission.

## Method, and its trap

Diff the two module-cache trees and judge each change against this repo's mirrored types. The trap
to avoid is proving too little and claiming too much: showing that the three obvious session files
are unchanged does **not** establish that no mirrored session field moved, because a session's shape
reaches into `betasessionresource.go`, `betasessionthreadevent.go`, `betaagentversion.go`,
`betaenvironment.go` and `betaenvironmentwork.go`. Enumerate every file that defines a mirrored
shape and show each is unchanged, or the conclusion is not supported.

A second trap: the pinned version is quoted as the live contract standard in prose — the verifier's
wire-compat rung, docs/REFERENCE_PROJECTS.md's caveat, and docs/DIVERGENCES.md's Stop Work entry,
which cites the SDK by `file:line`. Moving the pin without re-reading those citations at the new
version leaves docs asserting evidence that may no longer be where they say it is.

## Acceptance

- The three questions above answered with file:line evidence, recorded in docs/HISTORY.md as a
  verification record (that file's remit covers exactly this, plus any decisions rejected).
- `make verify` green, and no code change required — or, if one is, it lands in the same PR.
- Every live pinned-version label updated, and every `file:line` the Stop Work divergence cites
  re-read at the new version rather than assumed.
- A CHANGELOG.md entry carrying the narrative once, delegating the measurements to the record.
