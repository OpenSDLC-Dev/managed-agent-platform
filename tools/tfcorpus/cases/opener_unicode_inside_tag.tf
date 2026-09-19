locals {
  x = <<EÖT
resource "google_kms_crypto_key" "hidden" {
EÖT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
