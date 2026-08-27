- **The Workload Identity Federation commands in the GCP deployment notes could not run as
  written** (#508). `deploy/gcp/README.md` took `--project` out of `$WIF_PROVIDER`, which
  carries the project **number**, on the stated grounds that "`gcloud` accepts either". It
  does not: every `gcloud iam workload-identity-pools` verb — `describe`, `list`, `create`,
  `create-oidc` and `update-oidc`, pools and providers alike — refuses a project number
  before it makes any API call, and asks for the ID instead. That broke both commands the
  file documents, including the `update-oidc` that sets the attribute condition CD's security
  actually rests on. Both now pass `$WIF_PROVIDER` **whole** as the positional argument, which
  needs no `--project`, `--location` or `--workload-identity-pool` at all — and which neither
  a wrongly-set `--project` nor an unset `core/project` can redirect, so it is a stronger form
  of the guarantee the old wording was reaching for rather than a retreat from it.
