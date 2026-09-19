locals {
  s = "resource \"google_kms_crypto_key\" \"hidden\" {"
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
