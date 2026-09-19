locals {
  a = "p /* q" # r */
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
