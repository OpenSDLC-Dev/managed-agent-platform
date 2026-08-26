- **A staging environment parked and then forgotten now says so** (#504). Parking is a
  deliberate cost saving, so `deploy.yml` skips its deploying steps and finishes **green**
  and `deploy-alert.yml` stays silent on such a run in both directions — both correct, and
  between them they leave nothing that gets louder as the weeks pass, while the cluster
  control plane, the LoadBalancers and storage go on billing. A new weekly
  `staging-parked.yml` asks the **cluster** rather than the deploy history — a deploy run
  exists only when somebody pushes, and a quiet fortnight is exactly when an environment
  gets forgotten — and keeps one issue open while the `power-saved-*` labels are there: it
  is opened only by the scheduled run, so the cadence is the grace period and no threshold
  had to be invented; it is never commented on again, because a standing flag's age is the
  point and a bot that repeats itself gets muted; and it closes itself, with a completed
  `deploy` among its triggers so that reviving staging retires it within minutes rather
  than by the next Monday. The label rule all of that turns on — a KEY beginning with
  `power-saved-`, one `;` away from also matching a label VALUE — moved into
  `.github/scripts/parked.sh`, which `deploy.yml` now shares so the two can never disagree
  about the same cluster, with `make parked-test` behind it.
