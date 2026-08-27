variable "project_id" {
  type        = string
  description = "The GCP project that holds the staging environment."
}

variable "kms_location" {
  type        = string
  description = <<-EOT
    Location for the KMS key ring. Separate from var.region because a key ring's
    location is immutable and the ring is never destroyed — so a later decision
    to move the environment to another region must not be blocked by, or silently
    orphan, the key that holds the only path back to the vault ciphertext.
  EOT
  default     = "us-central1"
}

variable "state_bucket_location" {
  type        = string
  description = <<-EOT
    Location for the bucket holding environment/'s Terraform state (#478).
    Separate from var.kms_location for the same reason that one is separate from
    a region: a bucket's location is immutable, and the state bucket outlives
    every environment/ rebuild — including one that moves the environment
    somewhere else. Multi-region ("US") and dual-region names are accepted by
    GCS too; a single region is the default because the state is written from
    one place at a time and read by one apply at a time.
  EOT
  default     = "us-central1"
}

variable "name_prefix" {
  type        = string
  description = <<-EOT
    Prefix for every resource name here, so two staging environments can share a
    project without colliding. Bounded at 17 characters: a service account id is
    capped at 30 and the longest suffix this config appends is "-controlplane"
    (13). A longer prefix would pass a laxer check here and fail at apply, which
    is a worse place to learn it.
  EOT
  default     = "map"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,16}$", var.name_prefix))
    error_message = "name_prefix must start with a lowercase letter and be 1-17 characters of [a-z0-9-] — a service account id is capped at 30 and this config appends up to '-controlplane' (13)."
  }
}
