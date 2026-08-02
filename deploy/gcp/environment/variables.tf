variable "project_id" {
  type        = string
  description = "The GCP project that holds the staging environment. Must match foundation/."
}

variable "region" {
  type        = string
  description = "Region for Artifact Registry, Cloud SQL and the bucket. The cluster is zonal — see var.zone, which must be inside this region."
  default     = "us-central1"
}

variable "name_prefix" {
  type        = string
  description = "Must match foundation/'s name_prefix — this configuration finds the foundation's resources by name. Same 17-character bound, for the same service-account-id reason."
  default     = "map"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,16}$", var.name_prefix))
    error_message = "name_prefix must start with a lowercase letter and be 1-17 characters of [a-z0-9-] — a service account id is capped at 30 and this config appends up to '-controlplane' (13)."
  }
}

variable "zone" {
  type        = string
  description = <<-EOT
    Zone for the GKE cluster. Zonal rather than regional deliberately: on a
    regional cluster a node pool's node_count is PER ZONE across three zones, so
    the pool sizes below would provision three times what they say. Must be
    inside var.region.
  EOT
  default     = "us-central1-a"

  validation {
    condition     = can(regex("^[a-z0-9-]+-[a-z]$", var.zone))
    error_message = "zone must be a GCP zone such as us-central1-a, not a region."
  }
}

variable "db_password_version" {
  type        = number
  description = <<-EOT
    Bump this after bootstrap.sh rotates the database password. `password_wo` is
    a write-only argument, so Terraform holds no copy to diff against and cannot
    tell that the secret changed; this counter is the signal that it should push
    the current value to the instance again.
  EOT
  default     = 1
}

variable "kms_location" {
  type        = string
  description = "Must match foundation/'s kms_location."
  default     = "us-central1"
}

variable "namespace" {
  type        = string
  description = <<-EOT
    Kubernetes namespace the chart is installed into. It appears in the Workload
    Identity member strings, which name a KSA by "PROJECT.svc.id.goog[NS/KSA]" —
    so a chart installed into a different namespace than this variable names gets
    an ADC failure at pod startup, not a permission denial later.
  EOT
  default     = "map"
}

variable "release_name" {
  type        = string
  description = <<-EOT
    Helm release name. Combined with the chart name to derive the Kubernetes
    ServiceAccount names the Workload Identity bindings target: the chart's
    map.fullname is the release name alone when it already contains the chart
    name, and RELEASE-managed-agent-platform otherwise, truncated to 50. Getting
    this wrong binds an identity to a ServiceAccount nothing uses, which surfaces
    as an ADC failure at pod startup rather than as a permission denial.

    The chart's nameOverride and fullnameOverride are NOT accounted for here —
    they replace the chart name the mirrored arithmetic is built on. Set either
    and you must write the two iam.workloadIdentityUser bindings by hand.
  EOT
  default     = "map"
}

variable "platform_node_count" {
  type        = number
  description = "Nodes in the pool that runs the platform's own components."
  default     = 2
}

variable "platform_machine_type" {
  type        = string
  description = "Machine type for the platform pool."
  default     = "e2-standard-4"
}

variable "sandbox_node_count" {
  type        = number
  description = "Nodes in the dedicated sandbox pool. Sized in slice 5 from slice 4's measurements; while #64 leaves pods unreaped, every leaked pod holds its CPU and ephemeral-storage reservation."
  default     = 2
}

variable "sandbox_machine_type" {
  type        = string
  description = "Machine type for the sandbox pool. Ephemeral storage is requested equal to its limit, so this pool's boot disk is a scheduling input, not just capacity."
  default     = "e2-standard-4"
}

variable "sandbox_disk_size_gb" {
  type        = number
  description = "Boot disk per sandbox node. Sandbox workspaces are the container writable layer on this disk."
  default     = 100
}

variable "sandbox_pod_pids_limit" {
  type        = number
  description = <<-EOT
    The kubelet's per-pod process limit on the sandbox pool — the one containment
    the Pod API cannot express, so Hardening.PidsLimit is Docker-only and the
    Kubernetes provider logs that it ignored a configured value
    (docs/DIVERGENCES.md). Without this a fork bomb in a GKE sandbox is bounded
    by nothing the platform sets. GKE accepts 1024-4194304.
  EOT
  default     = 4096

  validation {
    condition     = var.sandbox_pod_pids_limit >= 1024 && var.sandbox_pod_pids_limit <= 4194304
    error_message = "GKE accepts a podPidsLimit between 1024 and 4194304."
  }
}

variable "cloud_build_service_account" {
  type        = string
  description = <<-EOT
    The identity Cloud Build runs as, which needs artifactregistry.writer to push
    the component images. Empty means the legacy default,
    PROJECT_NUMBER@cloudbuild.gserviceaccount.com.

    This is a variable rather than a constant because the answer is genuinely
    bimodal: projects that enabled the Cloud Build API more recently run builds
    as the Compute Engine default service account instead
    (PROJECT_NUMBER-compute@developer.gserviceaccount.com). Guessing wrong
    grants writer to an account no build uses and the first push fails on a
    permission this apply could have granted. `gcloud builds describe` names the
    one your project actually uses.
  EOT
  default     = ""
}

variable "db_tier" {
  type        = string
  description = "Cloud SQL machine tier."
  default     = "db-custom-2-7680"
}
