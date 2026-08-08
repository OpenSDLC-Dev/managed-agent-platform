- **Continuous delivery to the GCP staging environment**. A new
  `.github/workflows/deploy.yml` builds, pushes and installs on every push to
  `main` (plus `workflow_dispatch`), and a new
  [`deploy/gcp/staging-values.yaml`](./deploy/gcp/staging-values.yaml) is the
  versioned deployment input it reads. The deployment is **mode 2** — Cloud SQL,
  Cloud Storage and Cloud KMS behind the pre-created Secret `existingSecret`
  names — and every step of it was proven by **running it by hand** against the
  real project before the workflow was written: the Cloud Build submission
  succeeded in 2m16s, `helm upgrade --install --wait --atomic` brought all three
  Deployments Ready, and the deployed control plane answered
  `GET /v1/agents?limit=1` with 200 for a valid key and 401 for none (the record
  is in [docs/HISTORY.md](./docs/HISTORY.md)). The workflow's own first run is
  still ahead of it: nothing can drive `push: main` until this merges. A pipeline
  installing mode 1 instead would keep the platform's data in three bundled
  StatefulSets while the Cloud SQL instance `environment/` created sat unused.
  Because `existingSecret` is set the chart renders no Secret of its own, so the
  values file carries **no credential at all** — not withheld, structurally
  absent — and no `image.tag`, which the workflow sets from the one commit SHA it
  built. The seven-key Secret is assembled by the job instead: three values read
  from Secret Manager into a mode-700 temp dir, four non-secret literals written
  beside them, and all seven applied with `kubectl create secret generic
  --from-file … --dry-run=client -o yaml | kubectl apply -f -` — never
  `--from-literal`, which would put every credential on an argv. There is **no
  GitHub secret** in this repository and there must not be one: the job
  authenticates by Workload Identity Federation, so rotating
  `controlplane-api-key` or `model-providers` is `gcloud secrets versions add`
  plus a re-run of the workflow, with no repository setting to update. That
  re-run is load-bearing rather than a formality — env from a `secretKeyRef` is
  read once at process start, a dispatch on an unchanged commit renders a
  byte-identical pod template, and with `existingSecret` the chart's
  `checksum/secret` annotation hashes a Secret it never writes — so the job
  keeps what `kubectl apply` reports (created / configured / unchanged) and
  rolls every Deployment in the release exactly when the Secret changed.
  `database-url` is not in that sentence because it is **derived**: rotating
  `map-db-password` also means re-running `make gcp-db-init` for the `ALTER
  ROLE` and re-composing the DSN, and a rebuilt Cloud SQL instance moves the
  private IP inside it. **CD never runs Terraform**:
  the two applies, `make gcp-bootstrap` and `make gcp-db-init` stay human-driven
  (plan 20, Decision 9 — interactive applies, local state), and the pipeline owns
  only build → push → deploy → smoke. In mode 2 `gcp-db-init` is a real
  prerequisite rather than a formality — the DSN names the `map` role, which only
  `dbinit.sql` creates. The run ends by curling the LoadBalancer's address twice:
  **200** with the management key and **401** without one, because an address on
  the public internet has to be proven guarded — 401 exactly, not "anything but
  200", since `curl` reports a connection failure as `000` and a check that
  passes on `000` proves nothing about authentication. That address is plain HTTP on a
  bare IP, deliberately and only for staging — there is no domain yet, so there
  is nothing for a certificate to be issued against, and the cost is stated in
  [docs/deploy-gcp.md](./docs/deploy-gcp.md#exposing-the-control-plane) rather
  than hidden. The `model-providers` secret holds a deliberate **placeholder** (a
  real endpoint with a fake key) so the pipeline could be proven without
  inventing a credential; the docs carry the one-line command that replaces it,
  and the job still fails by name if that secret ever has no readable version.
  The deploy identity is scoped down to what needs it: `id-token: write` is
  granted to the one job rather than to the workflow, and the job's first step
  refuses a `workflow_dispatch` from any ref but `refs/heads/main`. That guard is
  honest about its limits — it stops an accident, not an attacker, since a branch
  that edits the workflow deletes the guard with it — and the cloud-side
  `assertion.ref` condition that would actually close it is recorded as an
  **unapplied** open gap, with its command, in
  [deploy/gcp/README.md](./deploy/gcp/README.md#continuous-delivery).
  CI's `helm` job now renders `staging-values.yaml` itself and asserts two things
  about it. That both halves of the sandbox placement pair reach the executor —
  the chart ships no `values.schema.json`, so `sandboxPlacment:` renders green
  and every sandbox pod then sits `Pending` forever behind a taint nothing
  tolerates. And that the deploy job and the chart still agree on the mode-2
  Secret's seven keys, in both directions: the job's `--from-file` names are read
  out of `deploy.yml` rather than restated, every one must be a key the render
  reads, and every key the render *requires* (a `secretKeyRef` without
  `optional: true`) must be one the job writes. That seam has nothing else
  holding it — `map.blobEnv` and `map.secretsEnv` mark their keys optional, so a
  rename on either side starts a pod that silently reads the chart's default
  (`BLOB_BACKEND` back to `s3`, against a GCS bucket) instead of failing.
