- **A staging environment parked and then forgotten now says so** (#504). Parking is a
  deliberate cost saving, so `deploy.yml` skips its deploying steps and finishes **green** and
  `deploy-alert.yml` stays silent on such a run in both directions — both correct, and between
  them nothing gets louder as the weeks pass while the LoadBalancer forwarding rules and the
  two storage charges go on billing. A new weekly `staging-parked.yml` asks the **cluster**
  rather than the deploy history — a deploy run exists only when somebody pushes, and a quiet
  fortnight is exactly when an environment gets forgotten — and keeps one issue open while the
  `power-saved-*` labels are there: opened only by a scheduled or hand-dispatched run, so the
  cadence is the grace period and no threshold had to be invented; never commented on again
  while it stands, because a standing flag's age is the point; and closed by the workflow
  itself once the cluster is unparked *or* gone, with a completed `deploy` among its triggers
  so reviving staging retires it in minutes. The label rule all of that turns on — a KEY
  beginning with `power-saved-`, one `;` away from also matching a label VALUE — moved into
  `.github/scripts/parked.sh`, which `deploy.yml` now shares so the two can never disagree
  about the same cluster, with `make parked-test` behind it.
