# The bindings that attach the foundation's identities to this environment's
# resources. They live here, not in foundation/, because they name resources a
# destroy takes with it.

# ---------------------------------------------------------------------------
# Workload Identity. Two halves, and the annotation alone is not enough: the
# Kubernetes ServiceAccount must be annotated (chart:
# controlplane.serviceAccount.annotations / brain.serviceAccount.annotations /
# executor.serviceAccount.annotations) AND the Google service account must
# permit that KSA to impersonate it. This is the second half.
#
# The member string names one exact KSA in one exact namespace. It is bound by
# name rather than by any label or suffix match deliberately — a suffix filter
# would grant impersonation, and with it this identity's key permissions, to any
# unrelated ServiceAccount that happened to match.
# ---------------------------------------------------------------------------

locals {
  # The chart's map.fullname, mirrored: the release name alone when it already
  # contains the chart name, RELEASE-managed-agent-platform otherwise — then
  # `trunc 50 | trimSuffix "-"`. The truncation is not decoration. Helm allows a
  # release name up to 53 characters, and without it a long release name would
  # bind Workload Identity to a ServiceAccount that does not exist while the one
  # that does gets nothing: an ADC failure at pod startup with no permission
  # denial to point at it.
  #
  # NOT mirrored: nameOverride and fullnameOverride, which replace the chart name
  # this arithmetic is built on. Set either and these bindings name the wrong
  # ServiceAccount — see var.release_name.
  chart_name = "managed-agent-platform"
  fullname_untruncated = (
    strcontains(var.release_name, local.chart_name)
    ? var.release_name
    : "${var.release_name}-${local.chart_name}"
  )
  fullname = trimsuffix(
    substr(local.fullname_untruncated, 0, min(50, length(local.fullname_untruncated))),
    "-",
  )

  controlplane_ksa = "${local.fullname}-controlplane"
  brain_ksa        = "${local.fullname}-brain"
  executor_ksa     = "${local.fullname}-executor"
}

# All three bindings depend on the CLUSTER, and the dependency has to be written out:
# the member string names the identity pool `PROJECT.svc.id.goog`, which is not a
# project-level fact but is created by the first cluster configured with
# `workload_identity_config`. Terraform sees no reference from these resources to
# that cluster — the pool name is built from var.project_id — so without this it
# schedules them in the first wave and they fail with
# `Identity Pool does not exist`. Observed on the first real apply: both bindings
# were attempted before the cluster had even started creating, ~10 minutes before
# the pool came into existence.
resource "google_service_account_iam_member" "controlplane_workload_identity" {
  service_account_id = data.google_service_account.controlplane.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.namespace}/${local.controlplane_ksa}]"

  depends_on = [google_container_cluster.map]
}

resource "google_service_account_iam_member" "brain_workload_identity" {
  service_account_id = data.google_service_account.brain.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.namespace}/${local.brain_ksa}]"

  depends_on = [google_container_cluster.map]
}

resource "google_service_account_iam_member" "executor_workload_identity" {
  service_account_id = data.google_service_account.executor.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.namespace}/${local.executor_ksa}]"

  depends_on = [google_container_cluster.map]
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
# GCS. Two roles, and the second is not belt-and-braces: s3.New checks the
# bucket before any object work (internal/blob/s3/s3.go), minio-go issues that
# as HEAD Bucket, and GCS maps it to storage.buckets.get — which
# roles/storage.objectAdmin does NOT grant. An identity holding only objectAdmin
# fails at construction, before it ever touches an object.
#
# That check is skippable since #241 (BLOB_BUCKET_PRECREATED), which would drop
# this grant — but only alongside an explicit BLOB_REGION, and whether GCS's S3
# XML API accepts an arbitrary SigV4 region scope for a bucket in var.region is
# unmeasured. This deployment is moving off the S3 path entirely instead (#240),
# so the grant stays until it does.
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

locals {
  node_service_account = "serviceAccount:${data.google_project.current.number}-compute@developer.gserviceaccount.com"
}

resource "google_artifact_registry_repository_iam_member" "node_puller" {
  location   = google_artifact_registry_repository.images.location
  repository = google_artifact_registry_repository.images.name
  role       = "roles/artifactregistry.reader"
  member     = local.node_service_account
}

# The SAME grant on the Docker Hub mirror, because a repository IAM binding is
# scoped to one repository and the mirror is a second one — a remote repository
# cannot also be the standard repository Cloud Build pushes to.
#
# Without this, pointing the chart's third-party images at the mirror produces
# ImagePullBackOff with a 403 from Artifact Registry, and only in projects that
# have not been handed the broad automatic Editor grant — so it would work on
# the project it was developed in and fail on a properly locked-down one.
resource "google_artifact_registry_repository_iam_member" "node_puller_mirror" {
  count = var.docker_hub_mirror ? 1 : 0

  location   = google_artifact_registry_repository.docker_hub[0].location
  repository = google_artifact_registry_repository.docker_hub[0].name
  role       = "roles/artifactregistry.reader"
  member     = local.node_service_account
}

# ---------------------------------------------------------------------------
# Cloud SQL. Mode-1 runs on the chart's bundled Postgres and does not touch
# this; mode-2 reaches the instance through the Cloud SQL Auth Proxy, which
# authenticates as the pod's Google identity and needs roles/cloudsql.client.
#
# All three processes open the database, so all three get it. The grant is
# unconditional rather than following the chart's cloudSQLProxy switch, because
# this configuration cannot see how the chart is installed and an IAM role
# nobody exercises costs nothing: under the direct-connection path
# (sslmode=require against the private IP) none of the three uses its Google
# identity for the database at all.
#
# Project-scoped, and it is worth knowing that this reaches every Cloud SQL
# instance in the project rather than only the one below: a Cloud SQL instance
# carries no IAM policy of its own, so the binding has nowhere narrower to go.
# Google's documented way to narrow it is an IAM Condition on this binding
# matching the instance's resource name. Not done here, because this project
# holds exactly one Cloud SQL instance and the condition would narrow the
# binding to the only instance it can already reach — but that is the lever, and
# a project that grows a second database is the moment to reach for it.
# ---------------------------------------------------------------------------

resource "google_project_iam_member" "controlplane_sql_client" {
  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${data.google_service_account.controlplane.email}"
}

resource "google_project_iam_member" "brain_sql_client" {
  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${data.google_service_account.brain.email}"
}

resource "google_project_iam_member" "executor_sql_client" {
  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${data.google_service_account.executor.email}"
}

# ---------------------------------------------------------------------------
# Cloud Build pushes the component images. Without this the very next step after
# a successful apply fails on a permission the apply could have granted.
#
# WHICH identity that is depends on when the project FIRST BUILT, not on when it
# was created: builds run as the legacy
# PROJECT_NUMBER@cloudbuild.gserviceaccount.com only where the first build
# predates Google's 2024 rollout, and as the Compute Engine default service
# account everywhere else — including in an old project that has never built.
# This file cannot decide that, and neither default is
# safe to assume — so var.cloud_build_service_account is REQUIRED. Terraform
# prompts for it, and its description carries the command that prints it.
#
# Not granted to both candidates — and read what that does and does not buy,
# because the honest version is uncomfortable. On a project on the MODERN
# default, `gcloud builds get-default-service-account` returns the Compute
# Engine default account, which is also local.node_service_account above,
# because neither node pool sets node_config.service_account. On such a project
# this grant hands `artifactregistry.writer` to the identity every node runs as,
# and a container escaping to the node can then overwrite the platform's own
# images and wait to be pulled. Declining to grant both only helps a project
# still on the legacy `PROJECT_NUMBER@cloudbuild` default, where the two
# identities really are different.
#
# Left as it is rather than guarded: a precondition asserting the two differ
# would reject the flow this repo's own README tells the operator to follow, on
# every modern project. Staging accepts that. Anything past staging should pass
# a dedicated build service account here, and give the node pools an identity of
# their own — issue territory, not a comment's to fix.
# `gcloud builds get-default-service-account` names the one your project uses.
# ---------------------------------------------------------------------------

resource "google_artifact_registry_repository_iam_member" "build_pusher" {
  location   = google_artifact_registry_repository.images.location
  repository = google_artifact_registry_repository.images.name
  role       = "roles/artifactregistry.writer"

  member = "serviceAccount:${var.cloud_build_service_account}"
}
