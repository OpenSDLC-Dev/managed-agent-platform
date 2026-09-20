locals {
  a = <<EOT
${ format("a$${b}c") }
  EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
