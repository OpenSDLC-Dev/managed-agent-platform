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
# Deliberately NOT here, so a reader can tell deferred from forgotten: Binary
# Authorization, Shielded Nodes secure boot, node auto-repair/upgrade, a
# dedicated node service account, CMEK on the bucket and registry, bucket
# versioning, Cloud SQL backups, VPC flow logs, and Postgres audit logging.
# Flow logs are the one on that list a scanner will flag as a plain
# misconfiguration rather than a missing feature, so to be explicit: they are
# omitted because they bill per gigabyte of sampled traffic for an environment
# whose network behaviour is already the thing under test, not because the
# subnet was written without noticing log_config exists. Every one of them
# belongs to the production shape docs/deploy-gcp.md documents; adding them here
# would make the staging environment slower to rebuild and would quietly turn
# this file into the production example it is explicitly not.
#
# Private nodes, Cloud NAT, a private-IP database and a Docker Hub mirror WERE
# on that list and are now below it. They moved because slice 5 accepts mode-2
# on this environment rather than only describing it, and mode-2's claims are
# claims about that topology: an acceptance run against public nodes and a
# public database would evidence a shape the guide does not recommend. The cost
# is real and is stated rather than hidden — a Cloud NAT gateway bills whether
# or not it forwards a packet, and a private-IP instance is unreachable from the
# operator's own machine, which is why the database DDL runs as an in-cluster
# Job (deploy/gcp/dbinit.sh) instead of over a local proxy.

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

  # The custom database role the platform's own role is created under. A local
  # rather than a variable: nothing outside this configuration and dbinit.sh
  # names it, and a role name that can be overridden is one more thing that can
  # disagree between the two.
  #
  # Underscore rather than hyphen, because it goes into SQL unquoted. Split
  # across two locals so the interpolation carries no quoted string of its own —
  # check_split.py refuses to parse those rather than risk losing its place, and
  # a guard that cannot read this file is a guard that prints ok over it.
  db_app_role_stem = replace(var.name_prefix, "-", "_")
  db_app_role      = "${local.db_app_role_stem}_app"

  # ---------------------------------------------------------------------------
  # The four ranges that must be mutually disjoint, and the arithmetic that
  # proves it. Each variable validates its own SHAPE; only something that can see
  # all four can check them against each other, and an overlap is not a
  # cosmetic error: GKE rejects the cluster for it, but only after the network,
  # the subnet, the router and the NAT gateway have been created — so the apply
  # half-builds an environment and then stops.
  #
  # Terraform has no cidrcontains (checked: the function does not exist in
  # 1.15.8), so containment is computed. Reinterpreting one range's network
  # address under the OTHER's prefix length and asking whether that lands on the
  # other's network address answers "does this range start inside that one" —
  # and two CIDRs overlap if and only if one starts inside the other. Verified
  # against both answers before being written here.
  # ---------------------------------------------------------------------------
  checked_cidrs = {
    subnet_cidr   = var.subnet_cidr
    pods_cidr     = var.pods_cidr
    services_cidr = var.services_cidr
    master_cidr   = var.master_cidr
  }

  # Each unordered pair once. Two details that are not style:
  #
  #   - paired by INDEX rather than by comparing the names, because Terraform's
  #     `<` accepts numbers only and `name_a < name_b` fails to evaluate rather
  #     than ordering the strings;
  #   - each pair is an OBJECT, not a two-element list, because flatten() is
  #     recursive: a list of two-element lists comes back as a flat list of
  #     strings, and every `p[0]` downstream then indexes into a string.
  cidr_names = keys(local.checked_cidrs)

  cidr_pairs = flatten([
    for i, a in local.cidr_names : [
      for j, b in local.cidr_names : { first = a, second = b } if j > i
    ]
  ])

  cidr_overlaps = [
    for p in local.cidr_pairs :
    format("%s (%s) overlaps %s (%s)",
    p.first, local.checked_cidrs[p.first], p.second, local.checked_cidrs[p.second])
    if cidrhost(format("%s/%s",
      cidrhost(local.checked_cidrs[p.first], 0),
      split("/", local.checked_cidrs[p.second])[1]), 0
    ) == cidrhost(local.checked_cidrs[p.second], 0)
    || cidrhost(format("%s/%s",
      cidrhost(local.checked_cidrs[p.second], 0),
      split("/", local.checked_cidrs[p.first])[1]), 0
    ) == cidrhost(local.checked_cidrs[p.first], 0)
  ]

  # Pre-joined, because an error_message is a template and check_split.py
  # refuses to parse a quoted string inside a ${...} interpolation — it cannot
  # tell where such a string ends, and a guard that loses its place prints `ok`
  # over configuration it never read.
  cidr_overlap_report = join("; ", local.cidr_overlaps)
}

resource "google_project_service" "required" {
  for_each = toset([
    "artifactregistry.googleapis.com",
    # cloudbuild.googleapis.com is deliberately NOT here — the foundation enables
    # it, because var.cloud_build_service_account cannot be read until it is on.
    "container.googleapis.com",
    # Named rather than relied upon. container.googleapis.com pulls Compute in as
    # a dependency on most projects, so this looks redundant — but this
    # configuration now owns Compute resources of its own (the network, the
    # router, the NAT gateway, the reserved peering range), and a resource whose
    # API is enabled only as somebody else's side effect is a resource that fails
    # to create the day that side effect changes.
    "compute.googleapis.com",
    # The peering that gives Cloud SQL a private address. Without it the
    # google_service_networking_connection below fails, and it fails AFTER the
    # cluster is built.
    "servicenetworking.googleapis.com",
    "sqladmin.googleapis.com",
    "storage.googleapis.com",
    # IAP, for the access bindings in iam.tf. Enabled unconditionally although
    # both IAP variables default to off, for the same reason compute is named
    # above rather than relied upon: a configuration should name the APIs its own
    # resources call. In practice it will already be on by the time a binding is
    # applied — enabling IAP on the backend service is what turns it on, and that
    # step precedes naming the backend service here — so this is the
    # configuration saying what it uses, not a step the operator is waiting on.
    #
    # No OAuth brand or client is enabled or declared anywhere, and that is not an
    # omission: the IAP OAuth Admin APIs were shut down on 2026-03-19 and the
    # provider's own google_iap_brand carries the deprecation notice saying so, so
    # there is no brand left to manage. IAP uses its Google-managed OAuth client
    # and there is no client secret for Terraform to hold — which suits this tree,
    # where no secret VALUE is in either state file by design.
    "iap.googleapis.com",
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

data "google_service_account" "brain" {
  account_id = "${var.name_prefix}-brain"
}

data "google_service_account" "executor" {
  account_id = "${var.name_prefix}-executor"
}

# ---------------------------------------------------------------------------
# The network. Its own VPC rather than the project's default, and that is a
# requirement rather than tidiness: the default network is auto-mode, so its
# subnets carry no secondary ranges and cannot host a VPC-native cluster's Pod
# and Service ranges, and a project-wide default is the wrong thing to attach a
# database peering to.
#
# Everything downstream hangs off this: private nodes need a subnet with
# Private Google Access, Cloud NAT needs a router in this network, and the
# database's private address is allocated out of a range reserved on it.
# ---------------------------------------------------------------------------

resource "google_compute_network" "map" {
  name                    = "${var.name_prefix}-staging"
  auto_create_subnetworks = false

  depends_on = [google_project_service.required]
}

resource "google_compute_subnetwork" "map" {
  name          = "${var.name_prefix}-staging"
  network       = google_compute_network.map.id
  region        = var.region
  ip_cidr_range = var.subnet_cidr

  # The single setting that makes private nodes workable. Without it a node with
  # no external address cannot reach *.pkg.dev, so every image pull fails —
  # including the platform's own components, which is a cluster that comes up
  # and never runs anything. Cloud NAT would paper over it by sending registry
  # traffic out to the internet and back; Private Google Access keeps it inside
  # Google's network instead.
  private_ip_google_access = true

  # Named rather than positional. GKE's ip_allocation_policy references these by
  # name, and a cluster pointed at a range that does not exist fails at create.
  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = var.pods_cidr
  }

  secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = var.services_cidr
  }
}

# Cloud NAT: the only way out for nodes with no external address. It is needed
# even though Private Google Access covers the registry, because the platform's
# own work reaches the public internet — the model endpoint, the web tools'
# backends, and a Docker Hub cache miss all leave the VPC.
#
# It bills per gateway-hour whether or not it forwards a packet. That is the
# standing cost of private nodes, and it is the reason this configuration is
# torn down between slices rather than left up.
resource "google_compute_router" "map" {
  name    = "${var.name_prefix}-staging"
  network = google_compute_network.map.id
  region  = var.region
}

resource "google_compute_router_nat" "map" {
  name   = "${var.name_prefix}-staging"
  router = google_compute_router.map.name
  region = var.region

  nat_ip_allocate_option = "AUTO_ONLY"

  # Every range in the subnet, which specifically includes the Pod range —
  # listing only the primary range would give the nodes egress and leave every
  # Pod without it, and Pods are what actually make the outbound calls.
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"

  log_config {
    enable = true
    filter = "ERRORS_ONLY"
  }
}

# ---------------------------------------------------------------------------
# Private services access — the peering Cloud SQL's private address comes from.
#
# Two resources and one hazard, and the hazard is on the way OUT. The reserved
# range is ours; the peering is a connection to Google's producer network, and
# Terraform is historically bad at removing one — a destroy can fail because the
# producer still holds an address out of the range.
#
# deletion_policy = "ABANDON" is the widely-cited escape and this configuration
# deliberately does NOT use it, because here it makes the teardown strictly
# worse rather than better. ABANDON does not delete the connection; it only
# drops it from state. The connection then still references this VPC, Google
# refuses to delete a network anything still references, and the destroy fails
# anyway — except that now the connection is no longer in state, so Terraform
# cannot clean it up on a retry either. It is the right setting for a
# configuration that does not own its network. This one owns its network.
#
# So the default is kept: the provider actually deletes the connection, ordered
# after Cloud SQL by the dependency below. Still expect a destroy to fail here:
# Google documents a FOUR-DAY wait after a Cloud SQL instance is deleted before
# the producer side releases its resources, and the manual
# `gcloud services vpc-peerings delete` is subject to the same wait — no amount
# of retrying inside one session gets past it. What that failure strands — the
# VPC, the reserved range, the connection and the API enablements — is
# non-billable; docs/deploy-gcp.md ("Tearing it down") has the command, the
# error signatures, and why stopping there is a legitimate end state.
# ---------------------------------------------------------------------------

resource "google_compute_global_address" "private_service_access" {
  name          = "${var.name_prefix}-psa"
  purpose       = "VPC_PEERING"
  address_type  = "INTERNAL"
  prefix_length = var.private_service_access_prefix_length
  network       = google_compute_network.map.id
}

resource "google_service_networking_connection" "private_vpc" {
  network                 = google_compute_network.map.id
  service                 = "servicenetworking.googleapis.com"
  reserved_peering_ranges = [google_compute_global_address.private_service_access.name]

  # No deletion_policy — see the header for why ABANDON is wrong here.
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

  network    = google_compute_network.map.id
  subnetwork = google_compute_subnetwork.map.id

  # VPC-native, by naming the subnet's secondary ranges. Required for private
  # nodes, and the ranges are fixed for the life of the cluster — a cluster is
  # rebuilt, not resized, to change them.
  ip_allocation_policy {
    cluster_secondary_range_name  = "pods"
    services_secondary_range_name = "services"
  }

  private_cluster_config {
    # No external addresses on nodes. Egress is Cloud NAT's job and registry
    # pulls are Private Google Access's; nothing on the internet can open a
    # connection to a node.
    enable_private_nodes = true

    # The control plane endpoint stays PUBLIC, and this is the one place this
    # configuration knowingly stops short of the production shape. A private
    # endpoint puts the Kubernetes API on an internal address only, which means
    # every `kubectl`, `helm` and `terraform` run needs a bastion, a VPN or a
    # proxy inside the VPC — real work, and work that would have to exist before
    # the acceptance battery could run at all. Nodes being private is the
    # property that removes the internet-facing attack surface; the endpoint
    # being public is an authenticated, TLS-terminated API that
    # var.master_authorized_cidrs narrows to named addresses.
    #
    # docs/deploy-gcp.md states the production position: set this true and reach
    # the API from inside the VPC.
    enable_private_endpoint = false

    master_ipv4_cidr_block = var.master_cidr
  }

  # Omitted entirely when the list is empty — an empty
  # master_authorized_networks_config is not "no restriction", it is "no address
  # may connect", which locks out the operator running the apply.
  dynamic "master_authorized_networks_config" {
    for_each = length(var.master_authorized_cidrs) > 0 ? [1] : []
    content {
      dynamic "cidr_blocks" {
        for_each = var.master_authorized_cidrs
        content {
          cidr_block   = cidr_blocks.value.cidr_block
          display_name = cidr_blocks.value.display_name
        }
      }
    }
  }

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

    # Checked HERE rather than in the variables, because a variable validation
    # cannot see its siblings. It still fires at plan time, before anything is
    # created — which is the whole point: GKE's own rejection of an overlap
    # arrives after the network, subnet, router and NAT already exist.
    precondition {
      condition     = length(local.cidr_overlaps) == 0
      error_message = "The cluster's address ranges must not overlap: ${local.cidr_overlap_report}."
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

# The Docker Hub mirror. A REMOTE repository is a read-through cache: a pull
# that misses fetches from upstream and keeps a copy, so the second pull of
# postgres:16-alpine does not depend on Docker Hub being reachable or on the
# project's anonymous rate limit not having been spent by somebody else.
#
# It is a SEPARATE repository from the one Cloud Build pushes to, because a
# repository is either STANDARD or REMOTE and cannot be both. The chart reaches
# it by having its third-party image values rewritten to this prefix — see the
# docker_hub_mirror output, which emits that prefix.
resource "google_artifact_registry_repository" "docker_hub" {
  count = var.docker_hub_mirror ? 1 : 0

  location      = var.region
  repository_id = "${var.name_prefix}-dockerhub"
  format        = "DOCKER"
  mode          = "REMOTE_REPOSITORY"
  description   = "Read-through cache of Docker Hub for the chart's third-party images"

  remote_repository_config {
    description = "docker.io"
    docker_repository {
      public_repository = "DOCKER_HUB"
    }
  }

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

    # Written out rather than left to the API's default. Left unset, the first
    # real apply was rejected with `Invalid Tier (db-custom-2-7680) for
    # (ENTERPRISE_PLUS) Edition`: the API chose Plus on its own, and Plus accepts
    # none of the db-custom-* tiers var.db_tier defaults to. Why it chose Plus is
    # not established here — what is established is that naming the edition
    # settles it, and that Enterprise is both the edition db-custom tiers belong
    # to and the cheaper of the two. The connection_pool_config block below is
    # accepted on Enterprise; that combination is what this instance was created
    # with.
    edition = "ENTERPRISE"

    deletion_protection_enabled = false

    ip_configuration {
      # No public address at all. The instance is reachable only from inside the
      # VPC, over the peering reserved above — which also means it is NOT
      # reachable from the operator's machine, and that is the intended
      # consequence rather than an oversight. Every step that needs a SQL
      # connection runs inside the cluster; deploy/gcp/dbinit.sh is that step.
      ipv4_enabled    = false
      private_network = google_compute_network.map.id

      # Encryption as a server-side rule, which is the only kind worth
      # asserting. A DSN parameter states what the CLIENT asked for; this states
      # what the server will accept, and it rejects an unencrypted connection
      # from any client that forgot to ask.
      #
      # Note what it does NOT mean for the platform's own DSN: the Cloud SQL
      # Auth Proxy terminates the encrypted leg itself and offers the
      # application a plain loopback socket, so the platform's DATABASE_URL
      # carries sslmode=disable while the connection this server sees is TLS.
      # The property is asserted where it is true — pg_stat_ssl on the backend —
      # rather than inferred from the client's request.
      #
      # That proxy is OPERATOR-SUPPLIED and this configuration does not create
      # it: nothing here, and no chart template, adds the sidecar. Read this
      # comment as a description of the topology docs/deploy-gcp.md tells the
      # operator to build, not as a promise that it exists. Its "in the same
      # pod, not a shared Deployment" is the load-bearing half — a shared proxy
      # would put that same sslmode=disable traffic on the pod network in
      # cleartext.
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

  # The peering, not just the API. `private_network` above references the
  # network, so Terraform already orders this after the network — but a
  # private-IP instance is created out of the RESERVED RANGE, and the range is
  # only usable once the service networking connection exists. Nothing in the
  # instance's arguments mentions that connection, so without this the instance
  # is scheduled alongside the peering and fails with a range that is not yet
  # allocated to any producer.
  depends_on = [
    google_project_service.required,
    google_service_networking_connection.private_vpc,
  ]
}

resource "google_sql_database" "map" {
  name     = var.name_prefix
  instance = google_sql_database_instance.map.name

  # ABANDON, and it is a correctness fix rather than a convenience. The Admin
  # API's database-delete runs as `cloudsqlsuperuser`, and dbinit.sql
  # deliberately transfers this database to the platform's own role — so the
  # delete fails with `must be owner of database map`, and the whole destroy
  # stops there. Found by the mode-2 acceptance run's teardown, which is the
  # first destroy this repo has ever run against an environment that had
  # `make gcp-db-init` applied to it (mode-1's teardown never touches Cloud SQL
  # ownership, which is why slice 4b's teardown proof passed).
  #
  # The obvious alternative — hand ownership back before destroying — is not
  # merely awkward, it is impossible here: the instance has no public address,
  # so only a Pod in the cluster can run SQL against it, and Terraform destroys
  # the cluster BEFORE it reaches Cloud SQL. By the time this delete is
  # attempted there is nothing left that could have prepared for it.
  #
  # ABANDON drops the resource from state without calling the API, and the
  # instance destroyed in the same run takes the database with it. The lifetime
  # this expresses — the database lives and dies with its instance — is the one
  # that was always true.
  deletion_policy = "ABANDON"
}

# ---------------------------------------------------------------------------
# The ONLY database user this configuration creates, and it is not the
# platform's.
#
# Cloud SQL grants `cloudsqlsuperuser` to every built-in user created through
# its Admin API — which is what google_sql_user uses — and that role carries
# CREATEDB and CREATEROLE. The documented way out is to name one or more custom
# database roles at creation (`database_roles`), which suppresses the
# cloudsqlsuperuser grant; but a custom role is an ordinary PostgreSQL role that
# must already EXIST, and creating one takes a SQL connection to an instance
# that does not exist until this resource does.
#
# That circle cannot be closed inside one apply, and closing it across two
# applies would make the ordinary path a two-phase ritual. So the split is by
# owner instead: Terraform creates the administrator, and deploy/gcp/dbinit.sh —
# one SQL session, from inside the cluster, because the instance has no public
# address — creates both the custom role and the platform's own role under it.
# A role created with CREATE ROLE never goes through the Admin API's user path
# and so is never granted cloudsqlsuperuser at all. dbinit.sh asserts that
# rather than asserting the reasoning.
#
# `postgres` is the built-in PostgreSQL administrator Cloud SQL provisions with
# the instance; naming it here sets its password rather than creating a second
# privileged account.
#
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
ephemeral "google_secret_manager_secret_version" "db_admin_password" {
  secret = "${var.name_prefix}-db-admin-password"
}

resource "google_sql_user" "admin" {
  name     = "postgres"
  instance = google_sql_database_instance.map.name

  password_wo = ephemeral.google_secret_manager_secret_version.db_admin_password.secret_data

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
