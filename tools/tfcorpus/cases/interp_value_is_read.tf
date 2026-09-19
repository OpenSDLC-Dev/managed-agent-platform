locals {
  m = "x:${google_kms_crypto_key.after.id}"
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
