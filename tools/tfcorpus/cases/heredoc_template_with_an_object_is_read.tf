locals {
  a = <<EOT
${ jsonencode({ a = 1 }) }
EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
