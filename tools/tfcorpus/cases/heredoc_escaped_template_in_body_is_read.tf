locals {
  a = <<EOT
$${ x
EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
