- **`environment/terraform.tfvars` is generated rather than hand-assembled** —
  `PROJECT=… make gcp-env-tfvars` (#478). Remote state alone would not have made a destroy
  portable: Terraform evaluates the whole configuration before destroying anything, and
  `*.tfvars` is gitignored, so a fresh checkout stops to prompt. The generator replaces forty
  lines of documented shell in `deploy/gcp/README.md` that did the same job by hand, including
  the part that mattered — `gcloud builds get-default-service-account` prints
  `projects/…/serviceAccounts/EMAIL` in some versions and the bare email in others, and on
  Windows it appends a CR. Each writes a file that looks right and fails Terraform's own
  validation mid-apply, after a cluster and a database already exist. It refuses rather than
  guesses: a disabled Cloud Build API names the step that enables it, anything that is not an
  account is rejected, and an existing tfvars is never overwritten, since it may carry
  `master_authorized_cidrs` or the two IAP settings and all three decide who can reach the
  cluster. `make gcp-tfvars-test` runs it against a fake `gcloud`, checking what it writes
  against the validation regex read out of `variables.tf` itself.
