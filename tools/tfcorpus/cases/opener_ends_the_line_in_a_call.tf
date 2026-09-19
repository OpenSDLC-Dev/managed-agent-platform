locals {
  x = trimspace(<<EOT
resource "google_kms_crypto_key" "hidden" {
EOT
  )
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
