locals {
  a = <<EOT
${"${"}"}" [
  EOT
]}
z = {
x = <<Z
EOT
}
resource "google_kms_crypto_key" "hidden" {
  name = "hidden"
  d    = <<Q
Z
Q
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
