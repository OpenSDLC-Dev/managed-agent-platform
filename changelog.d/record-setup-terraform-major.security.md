- **`hashicorp/setup-terraform` moved to its current major, 3.1.2 → 4.0.1**
  (Dependabot #344). The action had been pinned at 3.1.2 since the GCP staging
  Terraform brought it in (#250), and this is the first bump Dependabot has
  proposed for it; the new pin's SHA resolves to the tag its trailing comment
  claims. v4.0.0's only breaking change is the runtime — the action now runs on
  Node 24, the same runner floor the earlier batch of action majors
  (#187–#190) established for `checkout`, `setup-go`, `upload-artifact` and
  `setup-helm` — and every job in this repository is `ubuntu-latest`, so only a
  self-hosted runner would need attention. v4.0.1 on top clears the `DEP0169`
  `url.parse()` deprecation warning that the Node 24 move surfaced. Nothing in the
  workflow changed beyond the pin: the action's sole consumer is the
  `terraform` job, and all seven of its checks are green on the new pin,
  including the two that actually invoke the installed binary — `gcp-fmt` and
  `gcp-validate`, the latter re-initialising both configurations with
  `-backend=false` before validating them.
