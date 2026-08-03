# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Issues [#241](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/241) and
[#240](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/240)** — narrowing what
object storage costs a deployment identity: a pre-created-bucket mode for the S3 backend,
then a GCS-native backend that removes the HMAC key pair entirely.

## Tasks

- [x] #241 — `BLOB_BUCKET_PRECREATED` skips `BucketExists`/`MakeBucket` and requires a
  region (the location lookup is the other bucket-level call); chart value, render guard,
  CI assertions
- [ ] #240 — `internal/blob/gcs` on `cloud.google.com/go/storage` behind a backend
  selector, passing `blobtest` unchanged
- [ ] #240 — `deploy/gcp` drops the HMAC pair and the bucket-read grant
