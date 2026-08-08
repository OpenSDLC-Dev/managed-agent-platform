- **Continuous delivery to the GCP staging environment**. A new
  `.github/workflows/deploy.yml` builds, pushes and installs on every push to
  `main` (plus `workflow_dispatch`), and a new
  [`deploy/gcp/staging-values.yaml`](./deploy/gcp/staging-values.yaml) is the
  versioned deployment input it reads. The deployment is **mode 2** — Cloud SQL,
  Cloud Storage and Cloud KMS behind the pre-created Secret `existingSecret`
  names — and the pipeline was proven by running it: the Cloud Build submission
  succeeded in 2m16s, `helm upgrade --install --wait --atomic` brought all three
  Deployments Ready, and the deployed control plane answered
  `GET /v1/agents?limit=1` with 200 for a valid key and 401 for none. A pipeline
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
  authenticates by Workload Identity Federation, so rotating a credential is a
  `gcloud secrets versions add` and nothing else. **CD never runs Terraform**:
  the two applies, `make gcp-bootstrap` and `make gcp-db-init` stay human-driven
  (plan 20, Decision 9 — interactive applies, local state), and the pipeline owns
  only build → push → deploy → smoke. In mode 2 `gcp-db-init` is a real
  prerequisite rather than a formality — the DSN names the `map` role, which only
  `dbinit.sql` creates. The run ends by curling the LoadBalancer's address twice:
  200 with the management key, and *not* 200 without it, because an address on
  the public internet has to be proven guarded. That address is plain HTTP on a
  bare IP, deliberately and only for staging — there is no domain yet, so there
  is nothing for a certificate to be issued against, and the cost is stated in
  [docs/deploy-gcp.md](./docs/deploy-gcp.md#exposing-the-control-plane) rather
  than hidden. The `model-providers` secret holds a deliberate **placeholder** (a
  real endpoint with a fake key) so the pipeline could be proven without
  inventing a credential; the docs carry the one-line command that replaces it,
  and the job still fails by name if that secret ever has no readable version.
  CI's `helm` job now renders `staging-values.yaml` itself and asserts that both
  halves of the sandbox placement pair reach the executor — the chart ships no
  `values.schema.json`, so `sandboxPlacment:` renders green and every sandbox pod
  then sits `Pending` forever behind a taint nothing tolerates.
