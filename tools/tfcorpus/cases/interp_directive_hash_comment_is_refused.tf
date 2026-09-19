locals {
  a = "%{ if 1 == # c }x%{ endif }"
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
