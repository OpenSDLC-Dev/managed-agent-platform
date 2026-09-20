locals {
  a = <<EOT
${ <<Q
EOT
<<X
resource "google_kms_crypto_key" "hidden" {
  name = "hidden"
}
X
z = {
Q
}
EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
