- **`make gcp-env-tfvars` and `make gcp-env-init`, the two things a fresh checkout needs
  before it can touch staging** (#478). Remote state alone would not have made a destroy
  portable: Terraform evaluates the whole configuration before destroying anything, `*.tfvars`
  is gitignored, and a partial backend has to be told its bucket — so a clone stops twice. The
  generator replaces forty lines of documented shell, including the part that mattered:
  `gcloud builds get-default-service-account` prints `projects/…/serviceAccounts/EMAIL` in
  some versions and the bare email in others, and on Windows appends a CR. Each writes a file
  that looks right and then fails variable validation — at plan, so nothing is half-created,
  but a `destroy` fails identically and a destroy is what you run against an environment that
  is billing. It refuses rather than guesses: a disabled Cloud Build API names the step that
  enables it; an answer that is not a service account is rejected against a pattern
  deliberately *stricter* than Terraform's, which admits quotes and `${…}` that a quoted HCL
  string will not survive; and an existing tfvars is never overwritten, since it may carry
  `master_authorized_cidrs` or the two IAP settings. `gcp-env-init` is the cheap half — it
  points a checkout at the state and changes nothing, so `terraform output` and
  `make gcp-db-init` work without an apply. `make gcp-tfvars-test` runs all of it against a
  fake `gcloud`.
