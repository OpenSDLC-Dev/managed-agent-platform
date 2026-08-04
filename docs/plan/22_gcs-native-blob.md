---
status: in-progress
issue: "#240"
---

# A GCS-native blob backend, and the end of the HMAC key pair (plan 22)

`internal/blob/s3` reaches Google Cloud Storage through its S3-interop XML API. That works,
and it costs a static credential: an HMAC key pair, created by `deploy/gcp/bootstrap.sh`,
stored in two Secret Manager containers, and copied by hand into the deployment's
Kubernetes Secret. It is the deployment's **last Google-issued key material** — the only
credential a human downloads out of Google and carries to the cluster. Cloud KMS beside it
already authenticates as the workload itself through Workload Identity, with no material
anywhere (docs/HISTORY.md, the mode-2 acceptance run). Cloud SQL is a third case and not a
Workload Identity one: the as-built run reaches it over private IP with the platform's own
non-superuser database role and its password — an operator-chosen secret, not key material
Google issued — and the Auth Proxy path that would give it a Google identity was never
exercised. So this plan removes the one credential Workload Identity can remove today, and
leaves the database password where the deployment guide already puts it.

Tracking issue: [#240](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/240),
deferred out of plan 20 at its "What this plan does not do". The sibling issue
[#241](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/241) — the
pre-created-bucket mode for the S3 backend — landed first and is not repeated here; it
narrowed the *S3* path's permissions, this plan removes the *credential*.

## What this changes, and what it must not

The platform stays backend-agnostic. S3 remains the default and the portable path — the
bundled MinIO, Ceph RGW, AWS S3, any endpoint speaking the S3 wire — because self-hosting
is this project's premise and a GCS-only object store would be wrong for every deployment
that is not on Google Cloud. GCS-native is an **additional** backend, selected by
configuration, and the GCP deployment is its first consumer.

## Ground truth

Measured, not assumed. Everything in this section was run.

- **The Go client reaches a `fake-gcs-server` container** with
  `option.WithEndpoint("http://host:port/storage/v1/")` + `option.WithoutAuthentication()`
  + `storage.WithJSONReads()`. The last one is load-bearing: without it the client's
  *reads* go to the XML media host derived from the bucket name rather than to the
  configured endpoint, and every `NewReader` 404s against a container published on a
  random port. `STORAGE_EMULATOR_HOST` fixes reads too, but only when the fake's
  `-public-host` matches the published address — which a random host port cannot promise.
  It is also a gap: production takes the XML read path the fake cannot serve, so the
  hermetic suite exercises read *semantics* on a transport production does not use. The
  live tier is that gap's cover, which is one of the reasons it exists.
- **A missing bucket is indistinguishable from a missing object on the read path** — on the
  fake *and against real Cloud Storage*. A probe run under this project's own credentials
  against a throwaway bucket answered `NewReader` for a missing object and `NewReader` in a
  bucket that does not exist with the same bare `storage.ErrObjectNotExist`, carrying no
  `googleapi.Error` to inspect; only `Delete` distinguishes them, and only by the message
  text of its 404 (`"The specified bucket does not exist."` versus
  `"No such object: <bucket>/<key>"`). `ErrBucketNotExist` is never returned on either path.
- **A one-object listing distinguishes them, and costs no bucket-level privilege.**
  `bucket.Objects(ctx, …).Next()` returns `iterator.Done` for a bucket that exists and is
  empty, and `storage.ErrBucketNotExist` for one that does not — the listing path produces
  the sentinel the read path never does. It needs `storage.objects.list`, which
  `roles/storage.objectAdmin` already grants; `storage.buckets.get` is not involved. So the
  ambiguity above is answerable after all, on the miss path only, without the privilege
  this backend exists to do without.
- **An empty object name is rejected by the client**, before any request, with
  `"storage: object name is empty"` on `NewReader` and `Delete` — not the absence sentinel.
  So `blob.Store`'s "a caller bug is not a miss" rule holds without a guard of our own.
- **`cloud.google.com/go/storage@v1.56.0` is already the module graph's floor** (pulled in
  by `cloud.google.com/go@v0.123.0`), and pinning that version adds 11 indirect requires
  while upgrading nothing. Taking the latest instead would drag `otelgrpc` and `genproto`
  forward, which this repo's telemetry rests on. Both variants build, and both cross-build
  for `linux/arm` — the `crossbuild` gate is not a blocker.

## Decisions

1. **A selectable backend, not a replacement.** `BLOB_BACKEND` chooses; unset means `s3`,
   which keeps every existing deployment working unchanged and mirrors
   `internal/sandbox/backend`'s empty-means-docker arm. `gcs` is opt-in. Naming a backend
   is asking for it, so an explicit `BLOB_BACKEND=s3` with no endpoint is an error where an
   *unset* environment still means "deploy without object storage" — a deployment that
   asked for storage must not silently run without it.
2. **Selection lives in a sibling package, `internal/blob/backend`.** The seam package
   holds `blob.Store` *and* `blob.ErrNotFound`, which every backend wraps, so a selector
   inside `internal/blob` importing the backends would be an import cycle — the same
   argument `internal/secrets/backend`'s package doc already records. The selector is also
   the single place `blob.WithMetrics` is applied, so no backend can forget it.
3. **The GCS backend carries no credential and makes no bucket call.** Its config is a
   bucket name; authentication is Application Default Credentials, which on GKE is
   Workload Identity. There is no `BucketExists` analogue — the permission-minimal shape
   #241 gave the S3 backend is the *only* shape this one has — so a wrong bucket name or a
   missing IAM binding surfaces on first use rather than at startup, and the deployment
   identity needs object permissions only.
4. **A vanished bucket stays an error, answered by a listing rather than a bucket read.**
   The S3 backend keeps a removed bucket an *error* rather than absence
   (`TestOpsAgainstRemovedBucketAreErrorsNotAbsence`), because a mistyped bucket name that
   reads as an empty store says "the platform has no skills" instead of naming the
   incident. This backend holds the same line: the read path's one ambiguous sentinel is
   disambiguated by the one-object listing in Ground truth, on the miss path only, and only
   an *affirmative* answer buys absence — a probe that fails for any other reason (a
   denial, a transport error) leaves the question open, and an open question is an error
   rather than a report that data is gone. The cost is one extra request per miss and no
   privilege at all.
5. **Contract-tested against a pinned `fake-gcs-server` container, plus an opt-in live
   tier.** The container is symmetric with `blobtest`'s pinned MinIO and keeps the shared
   suite running in CI with no credentials and no spend. The live tier
   (`RUN_LIVE_GCS_TESTS=1`, bucket from `.env`) runs the *same* `blobtest.Run` against real
   GCS, following `gcpkmstest` exactly: configuration may come from `.env`, consent may
   only come from the environment, and once opted in a missing bucket fails rather than
   skips. It exists because the fake is a fake: every semantic in Ground truth was checked
   against real Cloud Storage before being designed around, it is the only thing that
   exercises the production read transport, and it is what keeps both true as the client
   version moves. It writes under a per-run key prefix and deletes what it wrote.
6. **The deployment moves wholesale.** GCP mode 2 selects the GCS backend, and the HMAC
   pair, its two Secret Manager containers, the `map-storage` service account and
   `bootstrap.sh`'s entire key-materialization dance (roughly half the script, with its
   rollback trap) go away. Both bucket IAM grants re-point to the workloads' own Google
   service accounts.
7. **The brain gets a ServiceAccount.** It builds a blob store
   (`cmd/brain/main.go`) but the chart gives it no ServiceAccount to bind, a gap
   `deploy/gcp/environment/iam.tf` already records for Cloud SQL. Workload-Identity-only
   object storage turns that gap into a missing feature, so this plan closes it.
8. **One leg of the foundation/environment split retires with the credential.** "Deleting
   a service account deletes its HMAC keys" is one of three stated reasons the durable half
   exists; it stops being true. The split still stands on the KMS key ring, which cannot be
   deleted at all, and on secrets as the reconciliation source. Four passages say the old
   thing and are rewritten; `check_split.py`'s `MIN_PROTECTED` drops deliberately, which is
   exactly what its own failure message asks for.

## Slices

1. **The backend and its selector.** `internal/blob/gcs`, `internal/blob/backend`, the
   fake-gcs test support package and the live tier, the four construction call sites, and
   the `BLOB_BACKEND` documentation. `blobtest.Run` passes unchanged, bare and through the
   metrics decorator.
2. **The deployment.** Chart wiring for a keyless object-storage mode plus the brain
   ServiceAccount; `deploy/gcp` drops the HMAC pair, re-points IAM to Workload Identity,
   and shrinks `bootstrap.sh`; the guides and the split rationale follow. Credential-free
   Terraform checks are the gate here — a real apply and a mode-2 acceptance re-run are an
   operator action, and the PR says so rather than implying otherwise.
