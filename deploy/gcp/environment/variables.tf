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
    Bump this after rotating the ADMINISTRATOR's password
    (<name_prefix>-db-admin-password). `password_wo` is a write-only argument, so
    Terraform holds no copy to diff against and cannot tell that the secret
    changed; this counter is the signal that it should push the current value to
    the instance again.

    It does NOTHING for the platform's own database role, which Terraform does
    not create or manage — that one is rotated by adding a version to
    <name_prefix>-db-password and re-running `make gcp-db-init`. Bumping this
    counter for it would leave the new secret version applied nowhere.
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

# ---------------------------------------------------------------------------
# The network. Four ranges that must not overlap each other, and one that must
# not overlap anything the cluster will ever peer with.
#
# They are variables rather than constants because a real deployment lands in an
# existing address plan, and a hard-coded 10.10.0.0/20 that collides with one is
# discovered as a failed apply after the cluster has been created. Defaults are
# picked to be unremarkable and mutually disjoint.
# ---------------------------------------------------------------------------

variable "subnet_cidr" {
  type        = string
  description = "Primary range of the cluster subnet — the NODES live here. Private Google Access is on, which is what lets private nodes pull from Artifact Registry with no external address."
  default     = "10.10.0.0/20"

  validation {
    # IPv4 only, and asserted rather than assumed. A GKE subnet, its
    # secondary ranges and the control-plane range are all IPv4 here, and an
    # IPv6 value would otherwise pass this check and then fail inside the
    # overlap arithmetic in main.tf as a raw cidrhost() error rather than as
    # the curated message that precondition exists to print.
    condition     = can(cidrhost(var.subnet_cidr, 0)) && can(regex("^[0-9.]+/[0-9]+$", var.subnet_cidr))
    error_message = "subnet_cidr must be an IPv4 CIDR block, e.g. 10.10.0.0/20."
  }
}

variable "pods_cidr" {
  type        = string
  description = "Secondary range for Pod IPs (VPC-native / alias IPs). Sized generously: it bounds how many Pods the cluster can ever run, and it cannot be resized after the cluster is created."
  default     = "10.20.0.0/16"

  validation {
    # IPv4 only, and asserted rather than assumed. A GKE subnet, its
    # secondary ranges and the control-plane range are all IPv4 here, and an
    # IPv6 value would otherwise pass this check and then fail inside the
    # overlap arithmetic in main.tf as a raw cidrhost() error rather than as
    # the curated message that precondition exists to print.
    condition     = can(cidrhost(var.pods_cidr, 0)) && can(regex("^[0-9.]+/[0-9]+$", var.pods_cidr))
    error_message = "pods_cidr must be an IPv4 CIDR block, e.g. 10.20.0.0/16."
  }
}

variable "services_cidr" {
  type        = string
  description = "Secondary range for ClusterIP Services. Also fixed for the life of the cluster."
  default     = "10.30.0.0/20"

  validation {
    # IPv4 only, and asserted rather than assumed. A GKE subnet, its
    # secondary ranges and the control-plane range are all IPv4 here, and an
    # IPv6 value would otherwise pass this check and then fail inside the
    # overlap arithmetic in main.tf as a raw cidrhost() error rather than as
    # the curated message that precondition exists to print.
    condition     = can(cidrhost(var.services_cidr, 0)) && can(regex("^[0-9.]+/[0-9]+$", var.services_cidr))
    error_message = "services_cidr must be an IPv4 CIDR block, e.g. 10.30.0.0/20."
  }
}

variable "master_cidr" {
  type        = string
  description = <<-EOT
    /28 for the GKE control plane's own VPC, which Google peers to this network.
    It must not overlap any range in this configuration and must be exactly a
    /28 — GKE rejects anything else. The overlap half is checked by a
    precondition in main.tf, which can see the other ranges; this can only check
    the shape.
  EOT
  default     = "172.16.0.0/28"

  # Two conditions, because the obvious one alone is not a check. A bare
  # `regex("/28$")` is satisfied by the string `not-a-cidr/28`, which then
  # reaches the API as a malformed range after the network and subnet have
  # already been created.
  validation {
    # IPv4 only, and asserted rather than assumed. A GKE subnet, its
    # secondary ranges and the control-plane range are all IPv4 here, and an
    # IPv6 value would otherwise pass this check and then fail inside the
    # overlap arithmetic in main.tf as a raw cidrhost() error rather than as
    # the curated message that precondition exists to print.
    condition     = can(cidrhost(var.master_cidr, 0)) && can(regex("^[0-9.]+/[0-9]+$", var.master_cidr))
    error_message = "master_cidr must be an IPv4 CIDR block, e.g. 172.16.0.0/28."
  }

  validation {
    condition     = can(regex("/28$", var.master_cidr))
    error_message = "GKE requires the master range to be exactly a /28."
  }
}

variable "private_service_access_prefix_length" {
  type        = number
  description = <<-EOT
    Prefix length of the range reserved for private services access — the block
    Google allocates managed producer services out of, and where this Cloud SQL
    instance's private address comes from. A /16 is Google's recommendation: the
    range is shared by every producer service peered to this network, and it
    cannot be grown once the peering exists.
  EOT
  default     = 16

  # Checked at plan time, because the alternative is discovering it at apply
  # time — after the network, subnet, router and NAT already exist.
  #
  # Upper bound: Google documents /24 as the smallest reserved range it accepts
  # for private services access. Lower bound: this range is ALLOCATED, not
  # chosen — google_compute_global_address gets a prefix length and no address,
  # so Google picks the block. Ask for a /8 and the only RFC1918 candidate is
  # 10.0.0.0/8, which contains three of this file's four network defaults (the
  # master range is 172.16/12); whichever of the two is created second then
  # fails, and the survivor makes the retry fail too.
  # Nine is the first prefix length that cannot swallow 10/8 whole. The overlap
  # precondition in main.tf cannot help here: an allocated range is unknown at
  # plan time, and referencing it would turn that plan-time check into an
  # apply-time one.
  # The floor() half is not pedantry: Terraform's `number` is not an integer
  # type, so 16.5 satisfies the bounds on either side of it and then reaches the
  # API as a prefix length that cannot exist.
  validation {
    condition = (var.private_service_access_prefix_length >= 9
      && var.private_service_access_prefix_length <= 24
    && var.private_service_access_prefix_length == floor(var.private_service_access_prefix_length))
    error_message = "private_service_access_prefix_length must be a whole number between 9 and 24 — Google accepts no range smaller than a /24, and a /8 allocation would collide with the 10.0.0.0/8 defaults this file ships."
  }
}

variable "master_authorized_cidrs" {
  type = list(object({
    cidr_block   = string
    display_name = string
  }))
  description = <<-EOT
    Who may reach the Kubernetes API server. EMPTY MEANS EVERY ADDRESS — the
    block is omitted entirely rather than rendered empty, because an empty
    master_authorized_networks_config would lock out the operator applying it.

    The nodes are private either way; this governs only the control plane, whose
    endpoint stays public so `kubectl` and `helm` work without a bastion (see
    enable_private_endpoint in main.tf). Narrowing it to the operator's own
    address is the single highest-value hardening step this variable offers, and
    it is left to the operator because a wrong value here is a lockout.
  EOT
  default     = []

  # A malformed entry here is worse than the usual malformed value: it reaches
  # the API as part of the cluster's authorized-networks list, and the failure
  # mode of getting that list wrong is losing access to the control plane. Fail
  # at plan time instead. IPv4 for the same reason the four CIDR variables
  # above are IPv4 — this list narrows access to the same public endpoint.
  validation {
    condition = alltrue([
      for c in var.master_authorized_cidrs :
      can(cidrhost(c.cidr_block, 0)) && can(regex("^[0-9.]+/[0-9]+$", c.cidr_block))
    ])
    error_message = "every master_authorized_cidrs entry needs an IPv4 CIDR cidr_block, e.g. 203.0.113.4/32."
  }
}

variable "docker_hub_mirror" {
  type        = bool
  description = <<-EOT
    Create an Artifact Registry remote repository that mirrors Docker Hub, so the
    chart's third-party images (postgres, minio, openbao) are pulled through the
    project's own registry rather than from a rate-limited anonymous upstream.

    It is a mirror, not a vendoring step: a cache miss still reaches Docker Hub,
    through Cloud NAT. What it buys is a stable pull path, one place to scan, and
    survival of a Docker Hub rate limit — not air-gap independence.
  EOT
  default     = true
}
