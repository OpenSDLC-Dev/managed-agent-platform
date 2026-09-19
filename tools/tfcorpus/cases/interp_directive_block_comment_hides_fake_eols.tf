locals {
  a = "%{ if 1 == /* c  ﻿d */ 1 }x%{ endif }"
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
