locals {
  a = "p${ 1 /* say "hi" */ }q"
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
