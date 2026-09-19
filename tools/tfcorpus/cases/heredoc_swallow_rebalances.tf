locals {
  a = <<EOT
    EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
locals {
  b = <<EOT2
EOT
EOT2
}
