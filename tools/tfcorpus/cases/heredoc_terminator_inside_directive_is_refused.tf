locals {
  a = <<EOT
%{ if 1 ==
EOT
}x%{ endif }
EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
