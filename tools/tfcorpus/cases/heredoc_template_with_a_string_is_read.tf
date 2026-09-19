locals {
  a = <<EOT
${ format("%s", 1) }
EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
