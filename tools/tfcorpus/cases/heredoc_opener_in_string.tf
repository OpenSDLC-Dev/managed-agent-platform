locals {
  cmd = "docker build - <<EOF"
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
