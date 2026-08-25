- **Staging can be parked between uses, and revived from any machine.** `make gcp-env-stop`
  resizes every GKE node pool to zero and stops Cloud SQL; `make gcp-env-start` reverses it;
  `make gcp-env-status` reports what is parked. Together they end the two charges that
  dominate the bill while keeping every resource — unlike `make gcp-env-destroy`, which takes
  the staging database with it. They need no Terraform, no state and no tfvars, only
  credentials for the project, so parking works from a machine that has never run an apply.
  The parked node counts live in cluster resource labels rather than on a laptop, so `start`
  restores the sizes that were actually running, and refuses the whole revival rather than
  guessing when one is missing. Order is enforced both ways: nodes drain before the database
  stops, and Cloud SQL reports `RUNNABLE` before any node returns. A parked environment is
  now skipped by `deploy.yml` with a notice rather than failing its smoke test — keyed on
  those labels, so a cluster at zero nodes that nobody parked still fails loudly. Do not
  `terraform apply` while parked; see [deploy/gcp/README.md](./deploy/gcp/README.md). (#480)
