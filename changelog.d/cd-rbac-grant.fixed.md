- **Continuous delivery to the GCP staging environment deploys again** — the chart renders a
  namespaced `Role` and `RoleBinding` for the executor, and writing them is guarded twice over
  by systems that do not see each other. Cloud IAM refuses first: the deploy identity holds
  `roles/container.developer`, which carries `get` and `list` on RBAC resources and no write
  verb. Kubernetes refuses second, and its escalation guard resolves what a principal holds
  from RBAC objects alone — GKE's IAM permissions arrive through an authorization webhook the
  rule resolver cannot see, so closing the first gate only moved the error. Helm patches an
  object only where what it renders differs from what it recorded, so both gates stayed
  invisible across the 45 runs that reached the install step and shut the moment v0.3.0's chart
  bump restamped `helm.sh/chart` on every rendered object: 23 consecutive runs failed at `helm
  upgrade` and rolled back, leaving staging seven days behind `main`. A `mapCdRbacWriter`
  custom role answers the first gate and an in-cluster basis Role the second — neither granting
  `escalate` or `bind`. [`deploy/gcp/README.md`](./deploy/gcp/README.md) records both, and why
  `roles/container.admin` was refused. (#469)
