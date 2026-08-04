# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**None.** [Plan 22](./docs/plan/22_gcs-native-blob.md) is archived — its delivery record is
in [docs/HISTORY.md](./docs/HISTORY.md), its narrative in [CHANGELOG.md](./CHANGELOG.md).
The backlog is [GitHub issues](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues).

## Tasks

_None in flight._ Last completed, both closing
[#240](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/240) and its sibling
[#241](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/241):

- [x] #241 — `BLOB_BUCKET_PRECREATED` skips `BucketExists`/`MakeBucket`
  ([#274](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/274))
- [x] #240 slice 1 — `internal/blob/gcs` behind the `internal/blob/backend` selector
  ([#276](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/276))
- [x] #240 slice 2 — the chart's keyless `gcsObjectStorage` mode; `deploy/gcp` drops the
  HMAC pair and binds the bucket to the three workload identities
