variable "project_id" {
  type        = string
  description = "The GCP project that holds the staging environment. Must match foundation/."
}

variable "region" {
  type        = string
  description = "Region for the cluster, Artifact Registry, Cloud SQL and the bucket."
  default     = "us-central1"
}

variable "name_prefix" {
  type        = string
  description = "Must match foundation/'s name_prefix — this configuration finds the foundation's resources by name."
  default     = "map"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,19}$", var.name_prefix))
    error_message = "name_prefix must start with a lowercase letter and be 1-20 characters of [a-z0-9-]."
  }
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
    name, and RELEASE-managed-agent-platform otherwise. Getting this wrong binds
    an identity to a ServiceAccount nothing uses.
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

variable "db_tier" {
  type        = string
  description = "Cloud SQL machine tier."
  default     = "db-custom-2-7680"
}
