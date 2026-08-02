# The handover to Helm.
#
# Where a value must match something the chart or the platform reads, the output
# emits it in exactly the shape it expects — a YAML map where the chart wants a
# map, a list of Toleration objects where it wants a list — so an operator feeds
# it in rather than transcribing it. `helm_values_mode1` goes one step further
# and is the values file itself.

output "cluster_name" {
  value       = google_container_cluster.map.name
  description = "GKE cluster name. See the zone output for the get-credentials invocation."
}

output "zone" {
  value       = var.zone
  description = "Zone the cluster lives in. `gcloud container clusters get-credentials $(terraform output -raw cluster_name) --zone $(terraform output -raw zone)`."
}

output "region" {
  value       = var.region
  description = "Region Artifact Registry, Cloud SQL and the bucket live in."
}

output "artifact_registry" {
  value       = "${google_artifact_registry_repository.images.location}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}"
  description = "Image prefix for Cloud Build pushes. The chart splits this into image.registry and image.repository — see helm_values_mode1, which does the splitting."
}

# ---------------------------------------------------------------------------
# Mode-1: bundled Postgres/MinIO/OpenBao with inline values (plan 20,
# Decision 4). Only two things in that install come from this configuration —
# where the images live and where sandboxes run — so this fragment holds exactly
# those and nothing else, and holds them once. The sandbox placement is a PAIR
# whose halves fail differently: without the tolerations every sandbox pod stays
# Pending forever, because the pool's taint has no other tolerator; without the
# selector they land on the platform pool, which is the pool the taint existed
# to protect. Emitting them as one values file rather than as separate strings
# is what stops an operator applying half of it.
# ---------------------------------------------------------------------------

output "helm_values_mode1" {
  value = yamlencode({
    # The chart composes registry/repository/COMPONENT:tag, appending the
    # component name itself — so this stops at the Artifact Registry repository
    # and renders e.g. .../map-images/controlplane:<appVersion>.
    image = {
      registry   = "${google_artifact_registry_repository.images.location}-docker.pkg.dev"
      repository = "${var.project_id}/${google_artifact_registry_repository.images.repository_id}"
    }
    sandboxPlacement = {
      nodeSelector = local.sandbox_node_selector
      tolerations  = [local.sandbox_toleration]
    }
  })
  description = "Ready-to-use Helm values fragment for the mode-1 install: `terraform output -raw helm_values_mode1 > values-gcp.yaml`."
}

# ---------------------------------------------------------------------------
# Mode-2 inputs: Cloud SQL + GCS + KMS. Emitted now because this configuration
# creates the resources now; consumed by slice 5, which assembles them into the
# pre-created Secret `existingSecret` points at.
# ---------------------------------------------------------------------------

output "kms_key_name" {
  value       = data.google_kms_crypto_key.cipher.id
  description = "GCPKMS_KEY_NAME, and the chart's gcpKMS.keyName. Also the key id stored beside every vault ciphertext."
}

output "controlplane_service_account_annotation" {
  value       = { "iam.gke.io/gcp-service-account" = data.google_service_account.controlplane.email }
  description = "Chart: controlplane.serviceAccount.annotations, verbatim. The IAM half of the Workload Identity pair is already applied by this configuration."
}

output "executor_service_account_annotation" {
  value       = { "iam.gke.io/gcp-service-account" = data.google_service_account.executor.email }
  description = "Chart: executor.serviceAccount.annotations, verbatim."
}

output "blob_endpoint" {
  value       = "storage.googleapis.com"
  description = "BLOB_S3_ENDPOINT. GCS's S3-compatible XML API, which is what internal/blob/s3 speaks."
}

output "blob_bucket" {
  value       = google_storage_bucket.blob.name
  description = "BLOB_S3_BUCKET."
}

output "sql_instance_connection_name" {
  value       = google_sql_database_instance.map.connection_name
  description = "PROJECT:REGION:INSTANCE — the Cloud SQL Auth Proxy's argument."
}

output "sql_database" {
  value       = google_sql_database.map.name
  description = "Database name inside the instance."
}

output "sql_user" {
  value       = google_sql_user.map.name
  description = "Database user. Its password is the foundation's Secret Manager secret and is never an output here."
}
