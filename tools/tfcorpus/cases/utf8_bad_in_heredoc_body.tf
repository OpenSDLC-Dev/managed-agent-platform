locals {
  a = <<EOT
aÿb
EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
