locals {
  a = <<EOT
${ join("", [
EOT
]) }
EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
