---
status: archived
---

# Bump anthropic-sdk-go v1.58.0 → v1.59.0

> Archived on landing: the bump and its verification are one PR. The measurements are in
> [docs/HISTORY.md](../HISTORY.md) § "anthropic-sdk-go v1.59.0 bump — wire-schema verification
> record", the narrative in CHANGELOG.md. **Everything below describes the state of the
> repository *before* that PR** — read it as the argument for doing the verification, not as a
> description of the result.

Direct request rather than a tracked issue, unlike [05](./05_sdk-bump-1.58.0.md)'s
[#120](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/120).

## Why this needs a plan file

For the reason plan 05 gives, which is a property of the pin and not of the issue that
prompted it: CLAUDE.md's wire-compatibility rules make the pinned `anthropic-sdk-go` this
project's **authoritative typed wire schema**, ahead of the `ant` CLI source and behind only
the public docs. Changing that pin changes what the repo is measured against. If a bump
silently moved a mirrored field, an enum value, or an ID prefix, every downstream check — the
verifier's wire-compat rung, external reviewers, docs/DIVERGENCES.md's registry — would start
grading against a contract nobody read. The bump is a schema event first and a dependency
update second, and the diff size is explicitly not the test.

## What has to be resolved

Answerable only by diffing the two versions, and to be recorded whether or not they turn out
to be no-ops — a null result is the point, since it is what lets the next bump skip the work.

1. **Do the changed types alter any shape `internal/domain` or `internal/api` mirrors?**
   Field-by-field, not file-by-file. v1.59.0's release note names Managed Agents surface
   directly — "model effort, initial session events, and threads delta streaming" — so unlike
   the v1.58.0 bump a null result should not be assumed.
2. **Does `shared/constant/constants.go` add stop reasons or event types the
   `{domain}.{action}` taxonomy should carry?** It is a diff hunk this time, not an untouched
   file: `EnvironmentDeleted`'s literal moves. Decide whether that reaches a response shape
   this platform emits.
3. **Which new surface is a divergence to record, and which is a code change to make?** The
   registry's remit is deliberate divergences and unconfirmed inferences; CLAUDE.md's
   "simplicity first" forbids building ahead of the backlog. Every new field therefore
   resolves to exactly one of: mirrored now (because the pin makes our current behavior
   *wrong*, not merely incomplete), or recorded with an issue. Decide explicitly rather than
   by omission.

## Method, and its traps

Diff the two module-cache trees and judge each change against this repo's mirrored types.

The first trap is proving too little and claiming too much: showing that the obvious session
files are unchanged does **not** establish that no mirrored shape moved, because a session's
shape reaches into `betasessionresource.go`, `betasessionthreadevent.go`, `betaagentversion.go`,
`betaenvironment.go` and `betaenvironmentwork.go`. Enumerate every file that defines a mirrored
shape and show each is unchanged, or the conclusion is not supported.

The second trap is that the pinned version is quoted as the live contract standard in prose —
the verifier's wire-compat rung, docs/REFERENCE_PROJECTS.md's caveat, `internal/toolset`'s
accepted-key comment, and two docs/DIVERGENCES.md entries that cite the SDK by `file:line`.
Moving the pin without re-reading those citations at the new version leaves docs asserting
evidence that may no longer be where they say it is. This trap did not fire at v1.58.0; a
diff that touches `betaagent.go` and `api.md` makes it likelier this time.

A third, specific to this bump: a constant whose *Go identifier* is unchanged but whose
*literal* moved is invisible to the compiler and to every test that does not assert the literal.
`make verify` passing is not evidence that a wire value held.

## Acceptance

- The three questions answered with file:line evidence, recorded in docs/HISTORY.md as a
  verification record.
- `make verify` green, and no code change required — or, if one is, it lands in the same PR
  with a test that fails before it.
- Every live pinned-version label updated, and every `file:line` the registry cites re-read at
  the new version rather than assumed.
- A CHANGELOG.md entry carrying the narrative once, delegating the measurements to the record.
