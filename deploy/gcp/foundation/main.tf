# foundation/ — created once, never destroyed.
#
# The line between this configuration and environment/ is not "expensive versus
# cheap" but "can a rebuild recreate this identically?" (plan 20, Decision 9).
# Everything here answers no:
#
#   - KMS key rings and crypto keys cannot be deleted from a project. Destroying
#     the Terraform resource does NOT leave a working key behind: the provider's
#     default deletion_policy is DELETE, which schedules every CryptoKeyVersion
#     for destruction and renders the key unusable while the name stays taken.
#     The vault ciphertext in Postgres is decryptable by that key and nothing
#     else, so that is silent, irreversible data loss discovered at the next
#     credential read. Hence deletion_policy = "PREVENT" below, and not only
#     prevent_destroy: prevent_destroy is a property of the CONFIGURATION and
#     disappears along with the block it is written in, while deletion_policy is
#     read from STATE and so survives someone deleting the resource block.
#   - Deleting a service account deletes its HMAC keys. An identity in the
#     disposable half would strand the once-readable HMAC secret in Secret
#     Manager: valid-looking, and dead.
#   - The Secret Manager secrets, which are the reconciliation source a rebuilt
#     environment is brought back into agreement with.
#
# What is deliberately NOT here: any secret VALUE. Terraform holds names, IAM
# bindings and preconditions only (plan 20, Decision 6). Putting a value in
# `secret_data` writes it into state in plaintext, and `environment/` reading it
# back would put a second copy in the state of the half that is destroyed and
# rebuilt routinely. So this configuration creates the secret *containers* and
# `bootstrap.sh` adds the first version of each — including the GCS HMAC key,
# whose secret GCS returns exactly once and which therefore must not be a
# Terraform resource either.
#
# `terraform destroy` is not a supported operation here, and `make
# gcp-env-destroy` does not touch this state.

resource "google_project_service" "required" {
  for_each = toset([
    "cloudkms.googleapis.com",
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "secretmanager.googleapis.com",
  ])
  service = each.value

  # Leave the APIs on when this config is removed: another environment in the
  # same project may depend on them, and re-enabling is not free of propagation
  # delay.
  disable_on_destroy = false
}

# ---------------------------------------------------------------------------
# KMS — the cipher's key. internal/secrets/gcpkms seals vault credentials under
# this exact resource name, which it also stores as each row's key id.
# ---------------------------------------------------------------------------

resource "google_kms_key_ring" "map" {
  name     = "${var.name_prefix}-keyring"
  location = var.kms_location

  depends_on = [google_project_service.required]

  # A key ring has no deletion_policy because it has no deletion: the API offers
  # no delete at all. prevent_destroy is the whole guard here, and its real job
  # is to stop a RENAME — `name` and `location` force replacement, so editing
  # name_prefix or kms_location would otherwise plan a destroy of a resource
  # that cannot be destroyed. With the guard the plan fails loudly instead.
  lifecycle {
    prevent_destroy = true
  }
}

resource "google_kms_crypto_key" "cipher" {
  name     = "${var.name_prefix}-credentials"
  key_ring = google_kms_key_ring.map.id

  # ENCRYPT_DECRYPT, not RAW_ENCRYPT_DECRYPT: the backend calls the standard
  # Encrypt/Decrypt methods. A RAW_ENCRYPT_DECRYPT key fails the startup probe.
  purpose = "ENCRYPT_DECRYPT"

  # The default is DELETE, which destroys every key version. See the header.
  deletion_policy = "PREVENT"

  # SOFTWARE, stated rather than defaulted, because the protection level decides
  # the plaintext ceiling the cipher enforces: 64 KiB here, 8 KiB on an HSM key
  # (internal/secrets/gcpkms). Both work; an operator choosing HSM should know
  # they are also choosing the smaller bound.
  version_template {
    algorithm        = "GOOGLE_SYMMETRIC_ENCRYPTION"
    protection_level = "SOFTWARE"
  }

  lifecycle {
    prevent_destroy = true
  }
}

# ---------------------------------------------------------------------------
# Identities. Three, because they need three different privilege sets — see the
# grants in environment/iam.tf.
# ---------------------------------------------------------------------------

# The control plane encrypts on write and decrypts on read (mcp_oauth_validate
# and the gate-config endpoint both call Decrypt), so it is the only identity
# that needs the decrypt half.
resource "google_service_account" "controlplane" {
  account_id      = "${var.name_prefix}-controlplane"
  display_name    = "managed-agent-platform control plane"
  deletion_policy = "PREVENT"

  lifecycle {
    prevent_destroy = true
  }
}

# The executor builds the cipher only to fail fast on a misconfigured backend
# and then discards it — vault credentials are decrypted control-plane side, at
# the gate-config endpoint. So the one KMS call this identity ever makes is the
# startup probe's Encrypt.
resource "google_service_account" "executor" {
  account_id      = "${var.name_prefix}-executor"
  display_name    = "managed-agent-platform executor"
  deletion_policy = "PREVENT"

  lifecycle {
    prevent_destroy = true
  }
}

# Blob storage is reached over the S3 protocol (internal/blob/s3, minio-go), so
# it authenticates with an HMAC key rather than with Workload Identity. The key
# belongs to this account, which is why the account cannot live in the
# disposable half — and why the key itself is bootstrap.sh's to create, since
# GCS returns its secret exactly once and a Terraform resource holding it would
# hold it in state.
resource "google_service_account" "storage" {
  account_id      = "${var.name_prefix}-storage"
  display_name    = "managed-agent-platform blob storage (GCS HMAC)"
  deletion_policy = "PREVENT"

  lifecycle {
    prevent_destroy = true
  }
}

# ---------------------------------------------------------------------------
# Secret Manager: the containers, with their replication. The VALUES are
# bootstrap.sh's job — see the header, and plan 20's Decision 6.
# ---------------------------------------------------------------------------

resource "google_secret_manager_secret" "db_password" {
  secret_id       = "${var.name_prefix}-db-password"
  deletion_policy = "PREVENT"

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]

  lifecycle {
    prevent_destroy = true
  }
}

resource "google_secret_manager_secret" "blob_access_key" {
  secret_id       = "${var.name_prefix}-blob-access-key"
  deletion_policy = "PREVENT"

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]

  lifecycle {
    prevent_destroy = true
  }
}

resource "google_secret_manager_secret" "blob_secret_key" {
  secret_id       = "${var.name_prefix}-blob-secret-key"
  deletion_policy = "PREVENT"

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]

  lifecycle {
    prevent_destroy = true
  }
}
