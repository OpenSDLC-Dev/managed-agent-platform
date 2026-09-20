locals {
  a = <<EOT
${ "x${ 1 }y" }
EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
