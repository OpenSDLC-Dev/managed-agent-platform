locals {
  x = <<~TAG
resource "google_kms_crypto_key" "hidden" {
TAG
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
