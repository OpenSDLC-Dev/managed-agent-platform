terraform {
  # 1.11, not 1.5: this configuration uses an `ephemeral` resource (1.10) and
  # `password_wo` (1.11). Declaring the lower bound would trade a clear
  # version-constraint error for an obscure unsupported-block one, which is the
  # entire job of this field.
  required_version = ">= 1.11"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 7.0"
    }
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}
