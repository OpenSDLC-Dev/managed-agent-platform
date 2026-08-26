- **A failed GCP deploy now opens an issue instead of notifying nobody** (#479). `ci` failing
  blocks a merge and is impossible to miss; `deploy` runs after the merge and reported to
  whoever thought to open the Actions tab — which is how the outage fixed in #469 ran red on
  every push for **seven days** unnoticed. A new `deploy-alert.yml` keeps one issue per
  outage: opened on the first failure and assigned, where it can be, to the run's actor;
  commented on each further one; closed by the next run that actually deploys. It names the
  step that failed rather than only the run, and says where the failure fell relative to
  `helm upgrade` — which decides whether staging is running the new commit, the old one, or
  part of each. It also answers the half of this that parking created: a run skipped because
  staging is parked is **green having deployed nothing**, so the notifier reads the run's own
  steps rather than its conclusion and never lets such a run close an open issue, and
  `deploy.yml` now says so on the run summary where it is legible without opening a log.
