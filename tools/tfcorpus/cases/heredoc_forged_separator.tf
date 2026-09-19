locals {
  a = <<EOT
EOT
resource "google_kms_crypto_key" "hidden" {
EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
