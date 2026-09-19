locals {
  a = "%{ if true }xy%{ endif }"
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
