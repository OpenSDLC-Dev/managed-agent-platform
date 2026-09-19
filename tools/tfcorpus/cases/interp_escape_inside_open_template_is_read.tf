locals {
  a = "${x$${y}}"
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
