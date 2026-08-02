# State is local, deliberately, and the README says what that costs.
#
# A remote backend for `foundation/` would have to live in a bucket, and a
# bucket is exactly the kind of thing `foundation/` exists to own — so the
# bootstrap is circular and the usual fix (a hand-made state bucket outside
# Terraform) reintroduces the untracked resource this split exists to avoid.
# Local state is honest instead: losing it does not lose the resources, because
# every resource here is adoptable. The README documents the `terraform import`
# recovery, which is the same command a project that already has a key ring
# runs on its first apply.
terraform {
  required_version = ">= 1.5"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 7.0"
    }
  }
}

provider "google" {
  project = var.project_id
}
