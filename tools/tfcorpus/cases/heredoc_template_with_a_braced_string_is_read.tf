locals {
  a = <<EOT
${ join("}", ["a"]) }
EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
