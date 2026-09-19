locals {
  a = "a\éb"
  b = "a\€b"
  c = "a\😀b"
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
