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
    the component images. Get it with:

        gcloud builds get-default-service-account --project YOUR_PROJECT

    Run that AFTER `make gcp-foundation-apply`, not before: enabling the Cloud
    Build API is what creates this account, and the foundation is what enables it.
    On a project where the API has never been on, the command fails with
    SERVICE_DISABLED rather than printing an account.

    REQUIRED, with no default, because every possible default is wrong for half
    of all projects and wrong in an expensive place. The split is by FIRST BUILD,
    not by project creation date: a project whose first build ran before Google's
    2024 rollout keeps the legacy
    PROJECT_NUMBER@cloudbuild.gserviceaccount.com, and everything else gets the
    Compute Engine default service account,
    PROJECT_NUMBER-compute@developer.gserviceaccount.com. An old project that has
    never built lands on the new default, which is why "how old is the project"
    is not a question you can answer this with. Guessing grants writer
    to an account no build uses, and nothing surfaces until the first image
    push — by which time the apply has already created a cluster and a database.
    A required variable turns that into a prompt with the command above in it.

    If your builds use a trigger-specific service account, name that one instead:
    the default is only what a build runs as when a trigger does not override it.
  EOT

  validation {
    condition     = can(regex("^[^@ /]+@[^@ /]+\\.(iam\\.)?gserviceaccount\\.com$", var.cloud_build_service_account))
    error_message = "Must be a service account email, e.g. 123456789@cloudbuild.gserviceaccount.com or 123456789-compute@developer.gserviceaccount.com. `gcloud builds get-default-service-account` may print it prefixed with `projects/.../serviceAccounts/` — pass only the email."
  }
}

variable "db_tier" {
  type        = string
  description = "Cloud SQL machine tier."
  default     = "db-custom-2-7680"
}
