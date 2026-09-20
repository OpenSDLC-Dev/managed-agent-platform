locals {
  a = <<EOT
${ 1 +
2 }
EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
