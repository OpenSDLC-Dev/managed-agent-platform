- **Deployment identifiers leave the public repository** (issue #355, following
  the console's [#69](https://github.com/OpenSDLC-Dev/managed-agent-console/issues/69)).
  `.github/workflows/deploy.yml` named one GCP project, zone, cluster, Artifact
  Registry path, Workload Identity Federation provider, deploy service account,
  blob bucket and KMS key; `deploy/gcp/staging-values.yaml` named the registry
  split and three Workload-Identity service accounts; `deploy/gcp/README.md`
  repeated all of it across roughly fifteen commands and paragraphs, including
  the project *number*. **None of it was ever a credential** — there is no
  service-account key, the WIF provider only trusts tokens from this
  organisation's repositories and asserts the ref, and every secret is read from
  Secret Manager inside the job. The reason to move it is different: a public
  repository that names one operator's project, cluster, registry, buckets, key
  and identities hands a reader a target list, and — the part that matters for an
  open-source project — ties the repository to a cluster nobody cloning it can
  reach. All eleven coordinates are now GitHub Actions **variables** (not
  secrets, because they are not secret), created **before** this landed: the
  reverse order lets any push to `main` deploy with `--project ""` and an image
  tag with no repository. Three names deliberately stay literals —
  `K8S_NAMESPACE`, `K8S_SECRET` and `HELM_RELEASE` name the *chart's* own
  objects, and `K8S_SECRET` has to equal `existingSecret` in a file that is in
  git, so a variable would let the two drift into a release bound to a Secret
  nothing creates, with no diff to notice it in. **`staging-values.yaml` becomes
  a reference deployment rather than this cluster's values file**, and keeps
  every key: the five that would name an operator are written against the neutral
  project `your-project` and overridden by the workflow with `--set-string`. The
  alternative — deleting those keys — was rejected because `helm template -f
  staging-values.yaml` on its own is exactly what CI runs against it, and a file
  missing `image.registry` renders against the chart's `ghcr.io` defaults while
  one missing the annotations renders a ServiceAccount with no identity, so the
  worked example of mode 2 would document a deployment that cannot start. Nothing
  is weakened by keeping them, because a dropped override fails closed:
  `your-project`'s registry does not exist (`ImagePullBackOff`) and its service
  accounts do not exist (ADC fails at pod startup), and `--wait --atomic` turns
  either into a rolled-back release and a red run. The workflow gains a
  **fail-fast guard** that asserts all eleven before it authenticates or builds
  anything, running *second* — after the ref guard, which refuses an unauthorised
  ref while telling it nothing, so the step that prints the deployment's
  configuration keys never runs for a ref that was already rejected. It rejects
  whitespace and commas as well as emptiness: a value of one space is no
  configuration while passing a `-z` test, and five of these reach
  `helm --set-string`, whose assignment list is comma-separated, so a comma would
  split one value into two paths and set neither to what was meant. It also
  asserts the one *composed* value's shape — `ARTIFACT_REGISTRY` is carried as a
  single `HOST/PROJECT/REPOSITORY` string rather than a host and a path joined in
  the `env:` block, because `${{ vars.A }}/${{ vars.B }}` with either half unset
  renders as `host/` or `/path`, which is non-empty and would pass the guard; the
  chart's `registry`/`repository` split is taken back off it at the first slash,
  and a value with no slash would set both to the same host and render
  `host/host/controlplane:sha`, a reference that is merely wrong rather than
  invalid. The annotation overrides escape the dots in `iam.gke.io/…`, which are
  not path separators: unescaped, `--set-string` would nest four levels of map
  under `annotations` and the ServiceAccount would carry no annotation the GKE
  metadata server looks for. What this does **not** do is retroactive — the
  literals remain in git history and in `docs/HISTORY.md`, which records what was
  actually run; rewriting the history of a repository that has cut releases is
  not worth it for identifiers, and the trust policy is what protects the
  deployment.
