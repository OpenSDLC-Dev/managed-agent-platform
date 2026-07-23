---
status: archived
issue: "#155"
---

# Skill archive integrity — a sha256 stored at upload, verified at materialization

> Archived 2026-07-23: completed. Delivered in one PR
> ([#162](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/162)); the delivery record is in
> [docs/HISTORY.md](../HISTORY.md) § "Skill archive integrity (plan 10)", the narrative in
> CHANGELOG.md. **Everything below describes the state of the repository *before* that PR** — read it
> as the argument for the change, not a description of the result — "The change" and "Acceptance
> criteria" are what was *planned*, not a report of what shipped (that is CHANGELOG.md). The one
> exception is decision D5 and the acceptance criterion belonging to it, added mid-PR in response to
> a review finding; D5 says so itself.

The plan for [#155](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/155).

## The gap

Nothing on the skill-archive path carries a content digest, so no component can tell a correct
archive from a corrupted or substituted one:

- **Upload computes none.** `skills.FromFiles` (`internal/skills/skills.go:134`) and
  `skills.FromZip` (`:206`) return a `Bundle` of `{Name, Description, Directory, Zip}`. The
  object-store `Put` discards what the backend reports — `internal/blob/s3/s3.go:74` drops
  `minio-go`'s returned `UploadInfo` (and with it the ETag) — and `blob.Store`
  (`internal/blob/blob.go`) exposes no checksum concept at all.
- **The registry stores none.** Migration `0007_skills.sql`'s `skill_versions` is
  `id / skill_id / version / name / description / directory / created_at`.
- **The download serves none.** `downloadSkillVersion` (`internal/api/skills.go:613-662`) sets
  `Content-Type` and `Content-Length` and streams the object verbatim.
- **Both materialization halves verify none.** The executor
  (`internal/executor/skills.go:187-192`) and the BYOC worker
  (`internal/worker/skills.go:194-199`) each call `skills.ReadArchive` and hand the bytes
  straight to `skills.Extract`.

The only integrity check anywhere is Go stdlib zip's **per-member CRC-32** — non-cryptographic,
scoped to a single member, and (for the verbatim-stored single-zip upload form, where `FromZip`
opens only `SKILL.md`) first exercised at materialization rather than at upload.

**Threat model.** Storage bit-rot, truncation, or whole-object substitution between the upload that
validated the bytes and the materialization that extracts them into a sandbox. The registry's
metadata (Postgres) and the archive bytes (object storage) are *different stores* with different
operators and failure modes, which is exactly what makes a digest held in the first and checked
against the second meaningful.

**Explicitly not in scope.** (a) In-sandbox tampering after extraction — already recorded and
accepted in `docs/DIVERGENCES.md` (the sentinel-idempotence entry). (b) A hostile control plane:
a digest served by the same process that serves the bytes proves storage integrity, not
provenance. (c) The Files API path (`blob.FilesKey`, plan 08), which has the same gap and is not
this issue's subject — a separate follow-up. (d) Backend-native checksums (S3 ETag / `x-amz-checksum-*`):
an ETag is not a content hash under multipart upload and is backend-specific, so it cannot be the
cross-backend contract `blob.Store` needs; the digest stays in the registry where it is already
trusted.

## Why this needs a plan file

The read half that runs in a **customer's** BYOC worker is wire-only: it never touches the
database, so the expected digest has to reach it over the wire. The pinned SDK
(`anthropic-sdk-go` v1.58.0, `betaskillversion.go`) gives `BetaSkillVersionGetResponse` /
`…ListResponse` / `…NewResponse` **no checksum field**, and the reference's own response headers on
`GET …/versions/{version}/content` are unrecorded (`docs/DIVERGENCES.md` INFERRED, "response
headers; skill rendering with zero versions"). So the change cannot be made without settling a wire
question against the reference checkouts, and the alternative — shipping executor-only — changes
the scope materially. Per CLAUDE.md's issue-triage rule that resolution belongs here, not
improvised mid-implementation.

## Decisions

**D1 — Both halves ship together.** The threat is storage-layer, and it is *stronger* at the BYOC
deployment point, whose object store is operated by the customer and never touched by the platform.
Shipping executor-only would protect the deployment the platform controls and leave unprotected the
one it does not. The vehicle below is additive and precedented, so there is no cost to including
the worker.

**D2 — The wire surface is one additive response header on the `/content` download:**
`x-skill-archive-sha256: <64 lowercase hex characters>`, sent only when the version row records a
digest. Direct in-repo precedent: `traceparent` / `tracestate` on `GET …/work/poll` are additive
response headers our worker reads and the reference worker ignores (`docs/DIVERGENCES.md`,
CONFIRMED). Reference clients — the SDK's `Download`, which sends `Accept: application/binary` and
treats the body as opaque — ignore unknown headers, so wire compatibility is unaffected in both
directions. Recorded by extending the existing INFERRED entry for this endpoint's headers rather
than opening a second one.

RFC 9530 `Repr-Digest: sha-256=:<base64>:` was considered and rejected: nothing between our two
processes consumes the standard field, and implementing one algorithm with no `Want-Repr-Digest`
and no `Content-Digest` distinction advertises a contract we do not honor. An honest
platform-specific header is the smaller claim.

**D3 — The column is nullable.** Migration `0010_skill_archive_sha256.sql` adds
`skill_versions.sha256 text` with a `CHECK (sha256 IS NULL OR sha256 ~ '^[0-9a-f]{64}$')`. It
cannot be `NOT NULL`: the bytes a pre-existing row's digest would have to be computed from live in
object storage, which a SQL migration cannot read, so `NOT NULL` fails on any populated table.
Every row written from this migration onward carries a digest — all three insert sites set it — so
`NULL` means exactly "written before this change". Once no such rows remain, a later migration can
tighten the column; that is not this change.

**D4 — Verification lives inside `skills.ReadArchive`**, whose signature becomes
`ReadArchive(r io.Reader, wantSHA256 string) ([]byte, error)`. It is the one function both halves
already call between "get the bytes" and "extract them", so folding the check in makes it
impossible for a future third caller to read an archive and forget to verify it, and keeps the
single rule for an absent digest in one place. An empty `wantSHA256` means no digest was recorded
and the archive is read unverified (D3's legacy rows, or a control plane older than D2's header);
each caller logs that it happened. A mismatch — including a present-but-malformed expected digest,
which simply never equals a real one — returns the sentinel `skills.ErrDigestMismatch`. The
comparison is case-insensitive: our own digests are lowercase by construction, but rejecting
another implementation's uppercase hex would be a gratuitous failure.

**D5 — The `.materialized` sentinel carries an integrity generation.** *(Added in review — the
Codex pass found the hole this closes.)* Both halves return early, without downloading anything,
when the marker matches the freshly resolved set. So a sandbox that a pre-verification binary
populated during a rolling upgrade — control plane and migration deployed first, execution binaries
still catching up — would keep matching afterwards and suppress the new verification for the rest of
that session, which on a long-horizon session is a long time. The marker therefore records what a
materialization was *guaranteed* to have done (`skills.SentinelVersion`; generation 2 = "verified
against the digest the registry holds for it, where one was recorded" — D3's legacy rows still have
none), and a marker of any other generation never matches. Cost: one
re-materialization per live sandbox at upgrade, nothing at steady state. Recording the digests in
the marker instead — the reviewer's first suggestion — was rejected: the BYOC worker learns a digest
only from the download response, i.e. *after* the skip decision, so it would have to spend a wire
round trip per skill per pass to answer what a constant answers for free. What this does not do is
heal bytes already on disk: the pass detects and refuses the bad archive (counting `corrupt`) and
declines to vouch for it in the new marker, but the sandbox seam has no delete primitive, so a file
an older binary wrote stays until something overwrites it — the residual already recorded for
in-place tampering.

**D6 — A mismatch folds into the existing per-skill tolerance, under its own outcome label.**
Materialization's contract on both halves is that per-skill failure is logged and skipped, never
fatal to the tool run (the reference's semantics, restated in `docs/DIVERGENCES.md`). Making a
corrupt archive fatal would turn one bad object into a total outage of every session referencing
it, which is strictly worse than that session running without the skill. So the corrupt archive is
skipped — but it is *not* a dangling reference and must not be counted as one: both halves gain a
`corrupt` value for the `skills.materialized` outcome attribute, so an operator can alert on
integrity failures separately from ordinary misses.

## The change

**Write half.** `skills.Digest(data []byte) string` (lowercase-hex sha256) is called by both
`Bundle` constructors, adding `Bundle.SHA256`; because `FromFiles` builds a canonical zip
(sorted entries, no timestamps) the digest is stable for identical content. The three inserts —
`insertSkill` (`internal/api/skills.go:184`), `insertSkillVersion` (`:413`), and the operator
importer (`internal/api/skillsimport.go:115`) — write it alongside the metadata they already store,
inside the same transaction, before the blob `Put`.

**Read half, executor.** `(*Executor).skillName` already reads the trusted per-version row for the
materialization directory; it also selects `sha256` and fills a new `skills.Resolved.SHA256`, which
`materializeSkill` passes to `ReadArchive`. No extra query.

**Read half, worker.** `materializeSkill` reads D2's header off the `Download` response and passes
it to `ReadArchive`. The worker's `Resolved.SHA256` stays empty — the SDK's version GET has no
checksum field to fill it from, which is the whole reason for the header.

**Download endpoint.** The existence probe (`SELECT EXISTS …`) becomes a `SELECT sha256 …` — same
round trip, same 404 on no rows — and sets the header when the value is non-NULL.

## Acceptance criteria → coverage

- `Digest` matches a known sha256 vector; both `Bundle` constructors set `SHA256` to the digest of
  the `Zip` they return — new `internal/skills` unit tests.
- `ReadArchive` returns the bytes on a matching digest, `ErrDigestMismatch` on a mismatched or
  malformed one, and reads unverified on an empty one; the byte cap still fires first for an
  oversized stream — new `internal/skills` unit tests.
- An upload (both forms) and an operator import persist `skill_versions.sha256` equal to the
  sha256 of the stored archive — new `internal/api` tests.
- The `/content` download carries `x-skill-archive-sha256` equal to the sha256 of the body it
  streamed, and omits the header for a row whose digest is NULL — new `internal/api` tests.
- The executor materializes a skill whose stored object was substituted after upload: refuses it,
  counts `corrupt`, and the tool run still completes; a row with a NULL digest still materializes
  — new `internal/executor` tests.
- The worker does the same end-to-end over the wire against the real API server — new
  `internal/worker` tests.
- A sentinel written under the pre-verification generation does not match: the pass
  re-materializes and verifies (healing a healthy archive, refusing a substituted one) and
  rewrites the marker in the current generation — new `internal/skills` and
  `internal/executor` tests.
- `make verify` green (build, crossbuild, vet, fmt, test, ≥90% coverage).
