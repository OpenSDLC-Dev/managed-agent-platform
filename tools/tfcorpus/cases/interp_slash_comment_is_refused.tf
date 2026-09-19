locals {
  a = "p${ 1 // c }q"
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
