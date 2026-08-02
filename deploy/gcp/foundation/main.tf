# foundation/ — created once, never destroyed.
#
# The line between this configuration and environment/ is not "expensive versus
# cheap" but "can a rebuild recreate this identically?" (plan 20, Decision 9).
# Everything here answers no:
#
#   - KMS key rings and crypto keys CANNOT BE DELETED. `terraform destroy`
#     removes them from state and leaves them in the project, so a naive
#     single-configuration setup collides on the name at the next apply — and
#     for the crypto key the stakes are higher than a name collision, because
#     the vault ciphertext in Postgres is decryptable by that key and nothing
#     else.
#   - Deleting a service account deletes its HMAC keys. An identity in the
#     disposable half would strand the once-readable HMAC secret in Secret
#     Manager: valid-looking, and dead.
#   - The secrets themselves, which are the reconciliation source a rebuilt
#     environment is brought back into agreement with.
#
# So `terraform destroy` is not a supported operation here. Every resource that
# would lose data carries prevent_destroy, and `make gcp-env-destroy` does not
# touch this state.

resource "google_project_service" "required" {
  for_each = toset([
    "cloudkms.googleapis.com",
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
  account_id   = "${var.name_prefix}-controlplane"
  display_name = "managed-agent-platform control plane"

  lifecycle {
    prevent_destroy = true
  }
}

# The executor builds the cipher only to fail fast on a misconfigured backend
# and then discards it — vault credentials are decrypted control-plane side, at
# the gate-config endpoint. So the one KMS call this identity ever makes is the
# startup probe's Encrypt.
resource "google_service_account" "executor" {
  account_id   = "${var.name_prefix}-executor"
  display_name = "managed-agent-platform executor"

  lifecycle {
    prevent_destroy = true
  }
}

# Blob storage is reached over the S3 protocol (internal/blob/s3, minio-go), so
# it authenticates with an HMAC key rather than with Workload Identity. The key
# belongs to this account, which is why the account cannot live in the
# disposable half.
resource "google_service_account" "storage" {
  account_id   = "${var.name_prefix}-storage"
  display_name = "managed-agent-platform blob storage (GCS HMAC)"

  lifecycle {
    prevent_destroy = true
  }
}

# ---------------------------------------------------------------------------
# The GCS HMAC key. Its secret is returned exactly once, at creation, and is
# therefore in Terraform state — the README says so plainly rather than leaving
# an operator to assume otherwise.
# ---------------------------------------------------------------------------

resource "google_storage_hmac_key" "blob" {
  service_account_email = google_service_account.storage.email

  lifecycle {
    prevent_destroy = true
  }
}

# ---------------------------------------------------------------------------
# Secret Manager. These are what a rebuilt environment is reconciled against:
# the database password is reapplied to the new Cloud SQL instance, and the
# HMAC pair is proven against the new bucket by an authenticated call.
# ---------------------------------------------------------------------------

resource "random_password" "db" {
  length = 32
  # Cloud SQL accepts far more than this, but the password travels through a
  # DSN in a Kubernetes Secret and through psql command lines during
  # acceptance; restricting to characters that survive both unquoted removes a
  # class of failure that looks like an authentication bug.
  special          = true
  override_special = "-_.~"

  lifecycle {
    ignore_changes = all
  }
}

resource "google_secret_manager_secret" "db_password" {
  secret_id = "${var.name_prefix}-db-password"

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]

  lifecycle {
    prevent_destroy = true
  }
}

resource "google_secret_manager_secret_version" "db_password" {
  secret      = google_secret_manager_secret.db_password.id
  secret_data = random_password.db.result
}

resource "google_secret_manager_secret" "blob_access_key" {
  secret_id = "${var.name_prefix}-blob-access-key"

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]

  lifecycle {
    prevent_destroy = true
  }
}

resource "google_secret_manager_secret_version" "blob_access_key" {
  secret      = google_secret_manager_secret.blob_access_key.id
  secret_data = google_storage_hmac_key.blob.access_id
}

resource "google_secret_manager_secret" "blob_secret_key" {
  secret_id = "${var.name_prefix}-blob-secret-key"

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]

  lifecycle {
    prevent_destroy = true
  }
}

resource "google_secret_manager_secret_version" "blob_secret_key" {
  secret      = google_secret_manager_secret.blob_secret_key.id
  secret_data = google_storage_hmac_key.blob.secret
}
