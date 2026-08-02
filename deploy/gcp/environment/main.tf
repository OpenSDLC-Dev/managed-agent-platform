# environment/ — created and destroyed freely.
#
# "Freely" is not the provider's default, and three settings make it true. They
# are stated here, deliberately and visibly, rather than discovered during a
# teardown that refuses to finish:
#
#   - a GKE cluster will not destroy while deletion_protection is on;
#   - a Cloud SQL instance will not destroy while ITS deletion_protection is on,
#     and there are two such flags — the resource-level one and
#     settings.deletion_protection_enabled — so both are set;
#   - a non-empty bucket will not destroy without force_destroy, and the
#     acceptance battery leaves objects in it by construction.
#
# All three are correct for a staging environment and wrong for anything holding
# data someone would miss. The deploy guide states them as a staging choice
# rather than shipping them as a default an operator inherits.
#
# What a destroy actually costs, said plainly: this configuration owns Cloud SQL,
# so tearing it down destroys the staging database — and vault ciphertext lives
# only in Postgres. Retaining the KMS key does not bring a deleted row back.

locals {
  # One definition, referenced by the node pool's taint and label and by the two
  # sandbox-placement outputs — so the taint a node carries and the toleration
  # the chart is told to set cannot drift apart.
  sandbox_taint_key = "map.opensdlc.dev/sandbox"
}

resource "google_project_service" "required" {
  for_each = toset([
    "artifactregistry.googleapis.com",
    "container.googleapis.com",
    "sqladmin.googleapis.com",
    "storage.googleapis.com",
  ])
  service            = each.value
  disable_on_destroy = false
}

# ---------------------------------------------------------------------------
# The foundation, read by name. Never owned, so a destroy here cannot reach it.
# ---------------------------------------------------------------------------

data "google_kms_key_ring" "map" {
  name     = "${var.name_prefix}-keyring"
  location = var.kms_location
}

data "google_kms_crypto_key" "cipher" {
  name     = "${var.name_prefix}-credentials"
  key_ring = data.google_kms_key_ring.map.id
}

data "google_service_account" "controlplane" {
  account_id = "${var.name_prefix}-controlplane"
}

data "google_service_account" "executor" {
  account_id = "${var.name_prefix}-executor"
}

data "google_service_account" "storage" {
  account_id = "${var.name_prefix}-storage"
}

# ---------------------------------------------------------------------------
# GKE. Standard rather than Autopilot: the sandbox pool needs a node taint and a
# kubelet podPidsLimit, and Autopilot manages node configuration itself.
# ---------------------------------------------------------------------------

resource "google_container_cluster" "map" {
  name     = "${var.name_prefix}-staging"
  location = var.region

  # The default pool is replaced immediately by the two below. Terraform's own
  # documented idiom, kept because the pools differ in taints and kubelet config
  # and a mutated default pool cannot express that.
  remove_default_node_pool = true
  initial_node_count       = 1

  # Dataplane V2 — the configuration the plan's gate probes ran against, where
  # the in-pod iptables payload from cmd/gate applies and enforces on IPv4 and
  # IPv6.
  datapath_provider = "ADVANCED_DATAPATH"

  # Workload Identity: what lets the gcpKMS cipher authenticate with no key
  # material in the release at all.
  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  # Staging. See the header.
  deletion_protection = false

  depends_on = [google_project_service.required]
}

resource "google_container_node_pool" "platform" {
  name       = "platform"
  cluster    = google_container_cluster.map.id
  node_count = var.platform_node_count

  node_config {
    machine_type = var.platform_machine_type

    # Least privilege for the node identity itself. Pod-level access comes from
    # Workload Identity, not from the node's scopes.
    oauth_scopes = ["https://www.googleapis.com/auth/cloud-platform"]

    workload_metadata_config {
      mode = "GKE_METADATA"
    }
  }
}

resource "google_container_node_pool" "sandbox" {
  name       = "sandbox"
  cluster    = google_container_cluster.map.id
  node_count = var.sandbox_node_count

  node_config {
    machine_type = var.sandbox_machine_type
    disk_size_gb = var.sandbox_disk_size_gb

    # The taint is the whole point of a separate pool: it keeps the platform's
    # own components off these nodes. The sandbox pods tolerate it through
    # SANDBOX_K8S_TOLERATIONS, and select the pool through
    # SANDBOX_K8S_NODE_SELECTOR — see outputs.tf, which emits both verbatim.
    #
    # Host isolation only. It does nothing about network reachability: a sandbox
    # here still reaches the control plane's Service exactly as before, and the
    # egress gate — not the scheduler — is what governs that.
    taint {
      key    = local.sandbox_taint_key
      value  = "true"
      effect = "NO_SCHEDULE"
    }

    labels = {
      (local.sandbox_taint_key) = "true"
    }

    # The per-pod process limit the Pod API cannot express. Node configuration,
    # so it belongs here rather than to any slice of platform code.
    kubelet_config {
      pod_pids_limit = var.sandbox_pod_pids_limit
    }

    oauth_scopes = ["https://www.googleapis.com/auth/cloud-platform"]

    workload_metadata_config {
      mode = "GKE_METADATA"
    }
  }
}

# ---------------------------------------------------------------------------
# Artifact Registry, Cloud SQL, GCS.
# ---------------------------------------------------------------------------

resource "google_artifact_registry_repository" "images" {
  location      = var.region
  repository_id = "${var.name_prefix}-images"
  format        = "DOCKER"
  description   = "managed-agent-platform component images"

  depends_on = [google_project_service.required]
}

resource "google_sql_database_instance" "map" {
  name             = "${var.name_prefix}-staging"
  region           = var.region
  database_version = "POSTGRES_16"

  # Staging. See the header — and note this is the resource-level flag; the
  # settings-level one below is a separate switch that also blocks a destroy.
  deletion_protection = false

  settings {
    tier = var.db_tier

    deletion_protection_enabled = false

    ip_configuration {
      # Slice 4 reaches the instance through the Cloud SQL Auth Proxy, which is
      # a plain TCP tunnel and is fine — the plan's LISTEN/NOTIFY probe ran
      # through it. Slice 5 moves this to private IP.
      ipv4_enabled = true
    }

    # Managed connection pooling stays OFF, and this is not a default worth
    # inheriting silently: transaction-mode pooling breaks a persistent LISTEN,
    # which is the failure mode internal/events/broker.go itself names. The
    # event log's SSE delivery depends on one pooled connection holding LISTEN
    # for the life of the subscription.
    connection_pool_config {
      connection_pooling_enabled = false
    }
  }

  depends_on = [google_project_service.required]
}

resource "google_sql_database" "map" {
  name     = var.name_prefix
  instance = google_sql_database_instance.map.name
}

# The password comes from the foundation's secret rather than being generated
# here, which is what makes a rebuild reconcilable: a destroyed instance takes
# its user with it, and the next apply reapplies the surviving password to the
# new one.
data "google_secret_manager_secret_version" "db_password" {
  secret = "${var.name_prefix}-db-password"
}

resource "google_sql_user" "map" {
  name     = var.name_prefix
  instance = google_sql_database_instance.map.name
  password = data.google_secret_manager_secret_version.db_password.secret_data
}

resource "google_storage_bucket" "blob" {
  name     = "${var.project_id}-${var.name_prefix}-blob"
  location = var.region

  # Staging. See the header: the acceptance battery leaves objects behind by
  # construction, and without this the destroy refuses.
  force_destroy = true

  # Uniform access, so the bucket's permissions are the IAM grants in iam.tf and
  # nothing else. Legacy ACLs would make the least-privilege claim untestable.
  uniform_bucket_level_access = true

  depends_on = [google_project_service.required]
}
