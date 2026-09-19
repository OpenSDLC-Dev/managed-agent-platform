locals {
  a = "p%{ if 1 ==  1 }x%{ endif }q"
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
