locals {
  a = <<EOT
p${ 1  }q
EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
