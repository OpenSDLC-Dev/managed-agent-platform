locals {
  a = "x﻿y"
  # note ﻿ here
  b = <<EOT
x﻿y
EOT
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
