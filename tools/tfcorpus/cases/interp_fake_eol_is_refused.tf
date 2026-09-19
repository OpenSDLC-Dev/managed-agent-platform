locals {
  a = "p${  1 }q"
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
