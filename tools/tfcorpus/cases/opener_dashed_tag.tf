locals {
  x = <<EOT-X
resource "google_kms_crypto_key" "hidden" {
EOT-X
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
