locals {
  a = "p$${ xy }q"
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
