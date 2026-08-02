# The bindings that attach the foundation's identities to this environment's
# resources. They live here, not in foundation/, because they name resources a
# destroy takes with it.

# ---------------------------------------------------------------------------
# Workload Identity. Two halves, and the annotation alone is not enough: the
# Kubernetes ServiceAccount must be annotated (chart:
# controlplane.serviceAccount.annotations / executor.serviceAccount.annotations)
# AND the Google service account must permit that KSA to impersonate it. This is
# the second half.
#
# The member string names one exact KSA in one exact namespace. It is bound by
# name rather than by any label or suffix match deliberately — a suffix filter
# would grant impersonation, and with it this identity's key permissions, to any
# unrelated ServiceAccount that happened to match.
# ---------------------------------------------------------------------------

locals {
  # The chart's map.fullname: the release name alone when it already contains
  # the chart name, RELEASE-managed-agent-platform otherwise. Mirrored here so a
  # release named "map" produces map-managed-agent-platform-controlplane and a
  # release named "managed-agent-platform" produces
  # managed-agent-platform-controlplane, exactly as helm renders them.
  chart_name = "managed-agent-platform"
  fullname = (
    strcontains(var.release_name, local.chart_name)
    ? var.release_name
    : "${var.release_name}-${local.chart_name}"
  )

  controlplane_ksa = "${local.fullname}-controlplane"
  executor_ksa     = "${local.fullname}-executor"
}

resource "google_service_account_iam_member" "controlplane_workload_identity" {
  service_account_id = data.google_service_account.controlplane.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.namespace}/${local.controlplane_ksa}]"
}

resource "google_service_account_iam_member" "executor_workload_identity" {
  service_account_id = data.google_service_account.executor.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.namespace}/${local.executor_ksa}]"
}

# ---------------------------------------------------------------------------
# KMS. Split by what each component actually calls, not by what is convenient.
# ---------------------------------------------------------------------------

# The control plane encrypts on write and decrypts for mcp_oauth_validate and
# the gate-config endpoint.
resource "google_kms_crypto_key_iam_member" "controlplane" {
  crypto_key_id = data.google_kms_crypto_key.cipher.id
  role          = "roles/cloudkms.cryptoKeyEncrypterDecrypter"
  member        = "serviceAccount:${data.google_service_account.controlplane.email}"
}

# The executor's only KMS call for the whole life of the process is the cipher's
# startup probe Encrypt — it builds the cipher to fail fast on a misconfigured
# backend and then discards it, and the resolution it does perform
# (vaultresolve.Bindings) takes no cipher at all. So encrypt is enough, and
# granting decrypt would expand post-compromise access for no runtime benefit:
# an executor holding the shared database connection could otherwise read vault
# ciphertext, strip the format marker, and call KMS directly.
resource "google_kms_crypto_key_iam_member" "executor" {
  crypto_key_id = data.google_kms_crypto_key.cipher.id
  role          = "roles/cloudkms.cryptoKeyEncrypter"
  member        = "serviceAccount:${data.google_service_account.executor.email}"
}

# ---------------------------------------------------------------------------
# GCS. Two roles, and the second is not belt-and-braces: s3.New calls
# BucketExists unconditionally before any object work
# (internal/blob/s3/s3.go), minio-go issues that as HEAD Bucket, and GCS maps it
# to storage.buckets.get — which roles/storage.objectAdmin does NOT grant. An
# identity holding only objectAdmin fails at construction, before it ever
# touches an object.
#
# Bound on the bucket rather than the project, so the identity can reach this
# bucket and no other.
# ---------------------------------------------------------------------------

resource "google_storage_bucket_iam_member" "blob_object_admin" {
  bucket = google_storage_bucket.blob.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${data.google_service_account.storage.email}"
}

resource "google_storage_bucket_iam_member" "blob_bucket_reader" {
  bucket = google_storage_bucket.blob.name
  role   = "roles/storage.legacyBucketReader"
  member = "serviceAccount:${data.google_service_account.storage.email}"
}

# ---------------------------------------------------------------------------
# Artifact Registry: the node pools pull component images through their node
# identity, which is the project's default compute service account.
# ---------------------------------------------------------------------------

data "google_project" "current" {}

resource "google_artifact_registry_repository_iam_member" "node_puller" {
  location   = google_artifact_registry_repository.images.location
  repository = google_artifact_registry_repository.images.name
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${data.google_project.current.number}-compute@developer.gserviceaccount.com"
}
