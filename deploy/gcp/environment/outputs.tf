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
# Mode-2 inputs: Cloud SQL + GCS + KMS. Emitted individually rather than as one
# fragment because they are assembled by hand into the pre-created Secret
# `existingSecret` points at — a values file cannot carry them, since in mode 2
# the chart renders no Secret at all.
# ---------------------------------------------------------------------------

output "kms_key_name" {
  value       = data.google_kms_crypto_key.cipher.id
  description = "GCPKMS_KEY_NAME, and the chart's gcpKMS.keyName. Also the key id stored beside every vault ciphertext."
}

output "controlplane_service_account_annotation" {
  value       = { "iam.gke.io/gcp-service-account" = data.google_service_account.controlplane.email }
  description = "Chart: controlplane.serviceAccount.annotations, verbatim. The IAM half of the Workload Identity pair is already applied by this configuration."
}

output "brain_service_account_annotation" {
  value       = { "iam.gke.io/gcp-service-account" = data.google_service_account.brain.email }
  description = "Chart: brain.serviceAccount.annotations, verbatim. Needed only under the Cloud SQL Auth Proxy topology (chart: cloudSQLProxy.enabled) — the brain holds no cipher, so this identity carries roles/cloudsql.client and nothing else. Applying it is harmless either way."
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
  value       = var.name_prefix
  description = <<-EOT
    The role the PLATFORM authenticates as — the one that appears in
    DATABASE_URL. Its password is the foundation's map-db-password secret and is
    never an output here.

    It is var.name_prefix rather than a reference to a resource because no
    resource creates it: deploy/gcp/dbinit.sh does, from SQL, which is what
    keeps it out of cloudsqlsuperuser. The two agree by construction — dbinit.sh
    reads this same output.
  EOT
}

output "sql_admin_user" {
  value       = google_sql_user.admin.name
  description = "The Cloud SQL built-in administrator. Holds cloudsqlsuperuser, is used by dbinit.sh for exactly one session, and is never given to the platform."
}

output "sql_app_role" {
  value       = local.db_app_role
  description = "The custom database role the platform's role is created under. Existence alone suppresses nothing: this is the role to NAME in --database-roles if a user is ever created through the Cloud SQL Admin API later, and naming it at creation is what suppresses that user's cloudsqlsuperuser grant."
}

output "sql_private_ip" {
  value       = google_sql_database_instance.map.private_ip_address
  description = "The instance's address on the VPC. Reachable from inside the VPC and from nowhere else — not from the machine running Terraform. Useful for diagnosis, and also a supported DSN target: a Pod reaches it directly with sslmode=require, which is what make gcp-db-init does. The Cloud SQL Auth Proxy is the better shape where you can run it, and takes sql_instance_connection_name rather than an address."
}

output "network" {
  value       = google_compute_network.map.name
  description = "The VPC this environment created. Named because a second environment in the same project must not reuse it — the peering range is reserved per network."
}

output "docker_hub_mirror" {
  # A comprehension over the count'd resource: zero elements when the mirror is
  # off, one when it is on, and join makes that "" or the prefix. It states the
  # arity directly instead of deriving the empty case from a caught error, which
  # is what the previous `try(...[0]..., "")` did — that worked (an out-of-range
  # index is a dynamic error, which try does catch) but reads as "if anything
  # goes wrong, say nothing".
  #
  # NOT coalesce(one(...), ""), which is the obvious-looking form and is broken:
  # Terraform's coalesce rejects the empty string along with null, so
  # `coalesce(one([]), "")` fails with "no non-null, non-empty-string arguments"
  # — at output time, on the very apply that switches the mirror off. Verified
  # in terraform console; `terraform validate` does not catch it.
  value = join("", [
    for r in google_artifact_registry_repository.docker_hub :
    "${r.location}-docker.pkg.dev/${var.project_id}/${r.repository_id}"
  ])
  description = <<-EOT
    Prefix that mirrors Docker Hub, or empty when var.docker_hub_mirror is false.

    Docker Hub's own naming is what makes the rewrite non-obvious: an official
    image published as `postgres:16-alpine` is really `library/postgres`, and the
    mirror is addressed with that full path. So the chart's postgres image
    becomes PREFIX/library/postgres:16-alpine, while minio/minio and
    openbao/openbao — which already name an organisation — become
    PREFIX/minio/minio and PREFIX/openbao/openbao.
  EOT
}
