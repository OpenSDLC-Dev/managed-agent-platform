# environment/ reads these by NAME through data sources, never by remote state,
# so the two configurations stay independently appliable and the disposable half
# can never own — or destroy — anything here.

output "kms_key_name" {
  value       = google_kms_crypto_key.cipher.id
  description = "Full CryptoKey resource name. This is GCPKMS_KEY_NAME (chart: gcpKMS.keyName) verbatim, and it is also the key id stored beside every vault ciphertext."
}

output "kms_key_ring_name" {
  value       = google_kms_key_ring.map.id
  description = "Full KeyRing resource name."
}

output "controlplane_service_account" {
  value       = google_service_account.controlplane.email
  description = "Google service account the controlplane pods impersonate via Workload Identity."
}

output "executor_service_account" {
  value       = google_service_account.executor.email
  description = "Google service account the executor pods impersonate via Workload Identity."
}

output "storage_service_account" {
  value       = google_service_account.storage.email
  description = "Google service account bootstrap.sh creates the GCS HMAC key under. Reached over the S3 protocol, so it is NOT bound to any Kubernetes ServiceAccount."
}

output "db_password_secret" {
  value       = google_secret_manager_secret.db_password.secret_id
  description = "Secret Manager secret id holding the password of the PLATFORM's database role — the one dbinit.sh creates outside cloudsqlsuperuser, and the one that appears in DATABASE_URL."
}

output "db_admin_password_secret" {
  value       = google_secret_manager_secret.db_admin_password.secret_id
  description = "Secret Manager secret id holding the Cloud SQL built-in `postgres` administrator's password. Used by one bootstrap connection and by break-glass access; never by the platform."
}

output "blob_access_key_secret" {
  value       = google_secret_manager_secret.blob_access_key.secret_id
  description = "Secret Manager secret id holding the GCS HMAC access id."
}

output "blob_secret_key_secret" {
  value       = google_secret_manager_secret.blob_secret_key.secret_id
  description = "Secret Manager secret id holding the GCS HMAC secret."
}
