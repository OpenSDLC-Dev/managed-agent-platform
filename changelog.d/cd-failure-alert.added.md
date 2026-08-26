- **A failed GCP deploy now opens an issue instead of notifying nobody** (#479). `ci` failing
  blocks a merge and is impossible to miss; `deploy` runs after the merge and reported to
  whoever thought to open the Actions tab — which is how the outage fixed in #469 ran red on
  every push for **seven days** unnoticed. A new `deploy-alert.yml` listens for `deploy`
  finishing and keeps one issue per outage: opened on the first failure, commented on each
  further one, closed by the next run that actually deploys. It names the step that failed,
  not just the run, and it uses a GitHub issue rather than a chat webhook because the backlog
  is already issues and it needs no credential nobody has. It also answers the half of this
  that parking created: a run skipped because staging is parked is **green having deployed
  nothing**, so the notifier reads the run's own steps rather than its conclusion and never
  lets such a run close an open issue — and `deploy.yml` now says "parked, nothing deployed"
  on the run summary, where it is legible without opening a log.
