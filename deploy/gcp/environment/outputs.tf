# The handover to Helm.
#
# Where a value must match something the chart or the platform reads, the output
# emits it in exactly the shape it expects — a YAML map where the chart wants a
# map, a list of Toleration objects where it wants a list — so an operator feeds
# it in rather than transcribing it. `helm_values_mode1` goes one step further
# and is the values file itself.

output "cluster_name" {
  value       = google_container_cluster.map.name
  description = "GKE cluster name. `gcloud container clusters get-credentials $(terraform output -raw cluster_name) --region $(terraform output -raw region)`."
}

output "region" {
  value       = var.region
  description = "Region the cluster and its backing services live in."
}

output "artifact_registry" {
  value       = "${google_artifact_registry_repository.images.location}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}"
  description = "Image prefix for Cloud Build pushes. The chart splits this into image.registry and image.repository — see helm_values_mode1, which does the splitting."
}

# ---------------------------------------------------------------------------
# Sandbox placement. A pair: neither half works alone. Without the tolerations
# every sandbox pod stays Pending forever, because the pool's taint has no other
# tolerator; without the selector sandbox pods land on the platform pool and the
# dedicated pool buys nothing.
# ---------------------------------------------------------------------------

output "sandbox_node_selector" {
  value       = { (local.sandbox_taint_key) = "true" }
  description = "Chart: sandboxPlacement.nodeSelector — an ordinary YAML map, which the chart encodes into SANDBOX_K8S_NODE_SELECTOR's comma-separated key=value form."
}

output "sandbox_tolerations" {
  value = [{
    key      = local.sandbox_taint_key
    operator = "Equal"
    value    = "true"
    effect   = "NoSchedule"
  }]
  description = "Chart: sandboxPlacement.tolerations — a YAML list of Kubernetes Toleration objects, which the chart passes through as the JSON array SANDBOX_K8S_TOLERATIONS parses."
}

# ---------------------------------------------------------------------------
# Mode-1: bundled Postgres/MinIO/OpenBao with inline values (plan 20,
# Decision 4). Only two things in that install come from this configuration —
# where the images live and where sandboxes run — so this fragment holds exactly
# those and nothing else. The bundled services still need their own credentials,
# which the chart deliberately does not generate; the README says so.
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
      nodeSelector = { (local.sandbox_taint_key) = "true" }
      tolerations = [{
        key      = local.sandbox_taint_key
        operator = "Equal"
        value    = "true"
        effect   = "NoSchedule"
      }]
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
