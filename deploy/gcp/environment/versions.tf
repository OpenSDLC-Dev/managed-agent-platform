terraform {
  # 1.11, not 1.5: this configuration uses an `ephemeral` resource (1.10) and
  # `password_wo` (1.11). Declaring the lower bound would trade a clear
  # version-constraint error for an obscure unsupported-block one, which is the
  # entire job of this field.
  required_version = ">= 1.11"

  # State lives in the bucket foundation/ owns (#478), not in a file beside
  # these .tf files. Local state made two operations non-portable and one of
  # them SILENTLY: `terraform destroy` iterates the state, so on a machine
  # without it destroy finds nothing to destroy and reports success while the
  # environment keeps billing.
  #
  # The block is PARTIAL on purpose. A backend cannot interpolate a variable —
  # it is read before Terraform evaluates anything — so a bucket named here
  # would have to be a literal, and this repository is public: an operator's
  # bucket name is exactly the kind of identifier #356 keeps out of the tree.
  # `bucket` therefore arrives at init time, and the Makefile derives it from
  # PROJECT and NAME_PREFIX the same way foundation/ composes it:
  #
  #     terraform init -backend-config="bucket=<project>-<prefix>-tfstate"
  #
  # `make gcp-env-apply` and `gcp-env-destroy` pass it; `make gcp-env-migrate-state`
  # is the one-time move from the laptop that still holds the local copy.
  #
  # `prefix` stays here because it is not an operator identifier — it names a
  # path inside whatever bucket is chosen, and it is `environment` so that
  # foundation/ could take a prefix of its own if its state ever moves too.
  backend "gcs" {
    prefix = "environment"
  }

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
