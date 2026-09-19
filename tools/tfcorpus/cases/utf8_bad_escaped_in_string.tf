locals {
  x = "a\ÿb"
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
