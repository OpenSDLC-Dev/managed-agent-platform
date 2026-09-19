locals {
  a = <<EOT
${ join("abc
EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
