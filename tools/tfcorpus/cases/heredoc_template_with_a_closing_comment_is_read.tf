locals {
  a = <<EOT
${ 1 /* c */ + 1 }
  EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
