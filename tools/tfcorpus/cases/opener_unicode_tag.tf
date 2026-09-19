locals {
  outer = <<Ö
  decoy = <<HIDE
Ö
}
resource "google_kms_crypto_key" "hidden" {
  name = "hidden"
}
locals {
  inner = <<HIDE
text
HIDE
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
