# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**[Plan 22](./docs/plan/22_gcs-native-blob.md) — issue
[#240](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/240)**, the second half
of narrowing what object storage costs a deployment identity: a GCS-native backend that
removes the HMAC key pair entirely. Its sibling
[#241](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/241) landed first.

## Tasks

- [x] #241 — `BLOB_BUCKET_PRECREATED` skips `BucketExists`/`MakeBucket` and requires a
  region (a region-less client resolves the bucket's location instead); chart value,
  render guard, CI assertions
- [x] #240 slice 1 — `internal/blob/gcs` on `cloud.google.com/go/storage` behind the
  `internal/blob/backend` selector, passing `blobtest` unchanged against a pinned
  `fake-gcs-server` and, opted in, against real Cloud Storage
- [ ] #240 slice 2 — chart wiring for the keyless mode; `deploy/gcp` drops the HMAC pair
  and the bucket-read grant, binding the ServiceAccounts #269 finished giving all three
