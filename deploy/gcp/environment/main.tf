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
#
# Deliberately NOT here, so a reader can tell deferred from forgotten: private
# nodes and master authorized networks, Binary Authorization, Shielded Nodes
# secure boot, node auto-repair/upgrade, a dedicated node service account, CMEK
# on the bucket and registry, bucket versioning, Cloud SQL backups, and Postgres
# audit logging. Every one of them belongs to the production shape slice 5
# documents; adding them here would make the staging environment slower to
# rebuild and would quietly turn this file into the production example it is
# explicitly not.

locals {
  # One definition of the taint, and one of the toleration that matches it,
  # referenced by the node pool and by the Helm handover — so the taint a node
  # carries and the toleration the chart is told to set cannot drift apart.
  # Sharing only the KEY would not achieve that: change the value here and the
  # outputs would still say "true", and every sandbox pod would stay Pending
  # forever, which is the exact failure this local exists to prevent.
  sandbox_taint_key   = "map.opensdlc.dev/sandbox"
  sandbox_taint_value = "true"

  # GKE's node-pool API spells the effect NO_SCHEDULE; the Kubernetes Toleration
  # the chart passes through spells it NoSchedule. Not drift — two APIs.
  sandbox_toleration = {
    key      = local.sandbox_taint_key
    operator = "Equal"
    value    = local.sandbox_taint_value
    effect   = "NoSchedule"
  }
  sandbox_node_selector = { (local.sandbox_taint_key) = local.sandbox_taint_value }
}

resource "google_project_service" "required" {
  for_each = toset([
    "artifactregistry.googleapis.com",
    # cloudbuild.googleapis.com is deliberately NOT here — the foundation enables
    # it, because var.cloud_build_service_account cannot be read until it is on.
    "container.googleapis.com",
    "sqladmin.googleapis.com",
    "storage.googleapis.com",
    # The acceptance ships traces to Cloud Trace through an in-cluster OTel
    # Collector (plan 20, Decision 5). telemetry.googleapis.com is Google's OTLP
    # ingest service and is the endpoint Decision 5's collector posts to; whether
    # cloudtrace.googleapis.com is additionally REQUIRED or merely the alternative
    # ingest path for the googlecloud exporter is a question two reviewers answered
    # differently, and the docs can be read both ways. Both are enabled because the
    # cost of an unnecessary enablement is nothing and the cost of the missing one
    # is an acceptance criterion — "traces are visible in Cloud Trace" — failing
    # after every static check passed. Slice 4b settles which it was.
    #
    # The collector's own IAM grant (roles/telemetry.tracesWriter, or
    # roles/cloudtrace.agent) is NOT here: the chart ships no collector — it has
    # an otlpEndpoint value and nothing to point it at — so the identity to bind
    # does not exist until slice 4b deploys one.
    "cloudtrace.googleapis.com",
    "telemetry.googleapis.com",
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
  name = "${var.name_prefix}-staging"

  # ZONAL, and that is a decision rather than a shortcut. On a regional cluster
  # `node_count` is per zone and GKE spreads across three, so the pool defaults
  # below would silently provision twelve nodes where the variable descriptions
  # promise four — on a cluster Decision 9 keeps up between slices. A staging
  # environment does not need zone redundancy; it needs the number in
  # variables.tf to be the number an operator is billed for. A production
  # deployment should be regional, and should then read node_count as per-zone.
  location = var.zone

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

  lifecycle {
    precondition {
      condition     = startswith(var.zone, "${var.region}-")
      error_message = "var.zone must be inside var.region — the cluster would otherwise sit in a different region from the registry, database and bucket it talks to."
    }
  }
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
      value  = local.sandbox_taint_value
      effect = "NO_SCHEDULE"
    }

    labels = local.sandbox_node_selector

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

      # No authorized networks are configured, so nothing can reach this address
      # without the proxy — but that is an access rule, not an encryption one,
      # and the two should not be conflated. Slice 5's shape is sslmode=require;
      # this is the server-side half of it, on from the first apply rather than
      # arriving with the production configuration.
      ssl_mode = "ENCRYPTED_ONLY"
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
#
# It is read EPHEMERALLY and written through `password_wo` — a write-only
# argument. Neither leaves a copy in this state file, which matters more here
# than in the foundation: this is the half that is destroyed and rebuilt
# routinely, so an ordinary data source plus `password` would scatter plaintext
# copies of the credential across every rebuild's state (plan 20, Decision 6 —
# "Terraform never sees any of these; it holds names, IAM bindings and
# preconditions only").
#
# This makes the apply order load-bearing: bootstrap.sh must have added the
# secret's first version before this runs. It fails loudly if not, naming the
# secret.
ephemeral "google_secret_manager_secret_version" "db_password" {
  secret = "${var.name_prefix}-db-password"
}

resource "google_sql_user" "map" {
  name     = var.name_prefix
  instance = google_sql_database_instance.map.name

  password_wo = ephemeral.google_secret_manager_secret_version.db_password.secret_data

  # Write-only arguments are not stored, so Terraform cannot diff them. This
  # counter is what tells it to write again: bump it after bootstrap.sh rotates
  # the secret, and the next apply pushes the new value to the instance.
  password_wo_version = var.db_password_version
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

  # Uniform access decides WHERE permissions come from; it does not stop one of
  # them from being a grant to allUsers. This does. The bucket holds session
  # files and skill archives, so the difference is not theoretical.
  public_access_prevention = "enforced"

  depends_on = [google_project_service.required]
}
