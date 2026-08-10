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
  secrets, because they are not secret), created **before** this landed. The
  ordering matters, though not for the reason the pre-guard version of this
  change would have had: the fail-fast step below ships in the same commit, so a
  repository whose variables do not exist yet gets a red run at step two rather
  than a deployment with `--project ""` — the ordering buys an unbroken `main`,
  and the guard is what makes the broken case loud instead of silent. Three names deliberately stay literals —
  `K8S_NAMESPACE`, `K8S_SECRET` and `HELM_RELEASE` name the *chart's* own
  objects, and `K8S_SECRET` has to equal `existingSecret` in a file that is in
  git, so a variable would let the two drift into a release bound to a Secret
  nothing creates, with no diff to notice it in. **`staging-values.yaml` becomes
  a reference deployment rather than this cluster's values file**, and keeps
  every key: the five that would name an operator are written against
  `registry.invalid` and the neutral project `your-project`, and overridden by
  the workflow with `--set-string`. The alternative — deleting those keys — was
  rejected because `helm template -f staging-values.yaml` on its own is exactly
  what CI runs against it, and a file missing `image.registry` renders against
  the chart's `ghcr.io` defaults while one missing the annotations renders a
  ServiceAccount with no identity, so the worked example of mode 2 would document
  a deployment that cannot start. The **image** half of that fails closed by
  construction, which is why the registry placeholder is an unresolvable host and
  not a plausible `LOCATION-docker.pkg.dev` path: a GCP project id is a globally
  unique namespace anyone may register, so
  `us-central1-docker.pkg.dev/your-project/…` would be a real Artifact Registry
  path in whatever project owns that id, and a reader running the file verbatim
  could silently pull a stranger's images instead of failing. RFC 2606 reserves
  `.invalid` so no such name can exist, so the pods cannot pull and
  `--wait --atomic` rolls the release back. The **identity** half does not fail
  closed, and claiming it did would have been the comfortable answer rather than
  the true one: this repository already documents, in that same values file and
  in `internal/brain/grader.go`, that a brain which cannot read the blob bucket
  downgrades to grading against the outcome description alone — a warning, not an
  error — so a dropped brain override would deploy **green** and quietly grade
  worse. The workflow therefore no longer trusts that it set what it set. A new
  step after `helm upgrade` reads all three annotations back out of the cluster
  and compares them against the variables they came from, turning that silent
  downgrade into a red run; it was validated against a real API server, including
  the dropped-override case. What it cannot catch — stated rather than implied —
  is a human swapping two variables in the settings page, since the deploy would
  then agree with its own inputs. That is the half of the cost of moving values
  out of the diff that stays. The workflow also gains a
  **fail-fast guard** that asserts all eleven before it authenticates or builds
  anything, running *second* — after the ref guard, which refuses an unauthorised
  ref while telling it nothing, so the step that prints the deployment's
  configuration keys never runs for a ref that was already rejected. It rejects
  whitespace and commas as well as emptiness: a value of one space is no
  configuration while passing a `-z` test, and four of them — `ARTIFACT_REGISTRY`
  and the three service accounts — reach `helm --set-string`, whose assignment
  list is comma-separated, so a comma would split one value into two paths and
  set neither to what was meant. It also asserts the one *composed* value's shape
  — `ARTIFACT_REGISTRY` is carried as a single `HOST/PROJECT/REPOSITORY` string
  rather than a host and a path joined in the `env:` block, because
  `${{ vars.A }}/${{ vars.B }}` with either half unset renders as `host/` or
  `/path`, which is non-empty and would pass the guard; the chart's
  `registry`/`repository` split is taken back off it at the first slash, and a
  value with no slash would set both to the same host and render
  `host/host/controlplane:sha`, a reference that is merely wrong rather than
  invalid. Counting slashes is not enough on its own, so empty segments are
  rejected too — `/p/r`, `h/p/`, `h//r` and an `https://` prefix all satisfy a
  naive three-segment test while being no registry path, and although Docker's
  reference parser rejects each of them a few steps later, this step promises to
  catch a malformed coordinate *before* the job authenticates. The annotation
  overrides escape the dots in `iam.gke.io/…`, which are
  not path separators: unescaped, `--set-string` reads the key as three nested
  maps under `annotations` — `iam` → `gke` → `io/gcp-service-account` — which is
  not the string map annotations are, so helm refuses the render with "cannot
  unmarshal object into Go struct field .metadata.annotations of type string".
  Dropping a backslash there therefore cannot deploy a ServiceAccount missing its
  annotation; nothing reaches the cluster at all. What this does **not** do is retroactive — the
  literals remain in git history and in `docs/HISTORY.md`, which records what was
  actually run; rewriting the history of a repository that has cut releases is
  not worth it for identifiers, and the trust policy is what protects the
  deployment. Nor is it concealment, and `deploy/gcp/README.md` now says so
  rather than leaving the stronger reading available: a run's logs are public on
  a public repository, and `docker push`, `get-credentials` and the smoke check
  print the registry, the cluster and the external address in the ordinary course
  of working. Masking them was rejected — a mask is a literal string replacement
  across the whole log, so the substitution that hides the project id also hides
  it inside every diagnostic that names it. What the move buys is what the issue
  asked for, stated at the width it actually holds: a clone's **deployment
  configuration** — the workflow, the values file, the runbook's commands — names
  no operator, so `deploy/` reads as a reference deployment rather than a
  half-edited template. A clone still carries the identifiers in
  `docs/HISTORY.md`, which is tracked and which this change deliberately leaves
  alone, because it is the record of what was actually run.
  `deploy/gcp/README.md` describes every command by variable and opens with the
  loader that puts them in an operator's shell, which is deliberately **not** the
  `gh variable list | eval` one-liner the issue sketched. That form has two
  failure modes an operator meets under pressure: it exports every variable the
  repository happens to have — `PROJECT` would silently redirect
  `make gcp-bootstrap` and `make gcp-db-init`, `TF_VAR_*` would redirect
  Terraform — and, since `gh` reports failure on stderr while an `eval` of
  nothing succeeds, a failed load leaves whatever the shell already held, so the
  assertion then passes on a *previous* deployment's coordinates and the
  `gcloud` commands below operate on the wrong project saying nothing. The
  documented loader clears all eleven names first, reads them back one at a time
  by name, and asserts all eleven rather than four.
