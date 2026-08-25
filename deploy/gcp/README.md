# GCP staging environment

Terraform for the staging environment the GCP slices of
[docs/plan/20_gcp-deployment.md](../../docs/plan/20_gcp-deployment.md) deploy into.

This is **developer tooling for GCP deployment only**. Terraform is never a dependency of
the platform, its build, or `make verify`, and the [Helm chart](../helm) stays the
portable installation path — nothing here is required to run managed-agent-platform.

These settings are **staging settings**. Several of them are destructive by design and are
called out below; do not carry them into an environment holding data someone would miss.

## Two configurations, not one

The line between them is not "expensive versus cheap" but **"can a rebuild recreate this
identically?"**

| | `foundation/` | `environment/` |
| --- | --- | --- |
| Lifecycle | created once, **never destroyed** | created and destroyed freely |
| Holds | KMS key ring + crypto key, the three service accounts, the Secret Manager secret *containers* | GKE cluster and node pools, Artifact Registry, Cloud SQL, the GCS bucket, all IAM bindings |
| Idle cost | cents a month | a running cluster and database |
| `terraform destroy` | not supported — `prevent_destroy` **and** `deletion_policy = "PREVENT"` | `make gcp-env-destroy` |

Two things make the split necessary rather than tidy:

- **A KMS key ring can never be deleted, a crypto key only by destroying and deleting
  every version of it — and destroying the *resource* is worse than leaving it.** The provider's default `deletion_policy` is `DELETE`, which
  schedules every `CryptoKeyVersion` for destruction: the name stays taken, so a
  single-configuration setup still collides at the next apply, and the key is now unusable.
  The vault ciphertext in Postgres is decryptable by that key and nothing else, so that is
  silent, irreversible data loss discovered at the next credential read.

  Hence **two** guards, not one. `prevent_destroy` is a property of the *configuration* and
  vanishes along with the block it is written in — Terraform will destroy a resource whose
  block you simply deleted. `deletion_policy = "PREVENT"` is read from *state*, so it
  survives that. Between them they also make a rename loud: `name`, `location`, `key_ring`
  and `account_id` force replacement, so editing `name_prefix` or `kms_location` aborts the
  plan instead of quietly recreating the key.
- **The secrets are the reconciliation source.** A rebuilt environment is brought back into
  agreement with them, and by two different mechanisms now that there are two database
  credentials: the **administrator's** password is reapplied to the new Cloud SQL instance by
  the apply itself, through `password_wo`; the **platform's** is reapplied by re-running
  `make gcp-db-init`, which is the same run that re-creates its role.

A third reason retired with #240, and it is worth recording why rather than deleting it
silently: **deleting a service account deletes its HMAC keys**, so the identity holding the
GCS HMAC pair could not live in the disposable half without stranding a once-readable secret
in Secret Manager — valid-looking, and dead. Object storage is now reached by the workloads
themselves through Workload Identity, so there is no HMAC key, no fourth identity, and
nothing for a rebuild to carry forward. The two reasons above are untouched.

**No secret value is in either configuration.** Terraform holds names, IAM bindings and
preconditions; `bootstrap.sh` owns value creation, and `environment/` reads the database
password through an *ephemeral* resource into a *write-only* argument, so neither state file
contains it. A value in `secret_data` would be plaintext in state — and in the state of the
half that is destroyed and rebuilt routinely, so every rebuild would scatter another copy.

`environment/` reads the foundation through `data` sources, by name, never by remote state.
So the two apply independently, and a destroy in the disposable half structurally cannot
reach anything in the durable one. CI enforces both halves of that: `environment/` may not
declare a `resource` of any unrecoverable kind, and every
`foundation/` resource whose loss is unrecoverable must carry `prevent_destroy`.

The check reads both halves **recursively**, so moving a resource into a child module does
not move it out of scope — and it *refuses* rather than skips anything it cannot read: a
`.tf.json` file, a `module` whose source is a registry or git address or resolves outside the
half or computed at plan time, an unterminated heredoc, unbalanced braces, a multi-line
`${...}` interpolation, or a quoted string inside a `${...}` interpolation or a `%{...}`
template directive (quote tracking is per line, so either a string left open or a nested
quote would desync the brace depth for the rest of the file). Silently skipping any of those would print `ok` over a partial scan, which is
worse than no check at all.

## What a destroy actually costs

`environment/` owns Cloud SQL, so **`make gcp-env-destroy` destroys the staging database**,
and vault ciphertext lives only in Postgres. Retaining the KMS key does not bring a deleted
row back. That is the right trade for a staging environment and this configuration makes it
deliberately — but it means a rebuild is proven with a *fresh* credential round-tripping on
the rebuilt stack, never with an old one surviving.

## Parking it between uses

Destroying is not the only way to stop paying for staging, and for anything short of
"we are done with this environment" it is the wrong one — it takes the database with it.
Parking stops the two charges that dominate the bill and keeps every resource:

```sh
PROJECT=your-project make gcp-env-stop     # nodes to zero, then the database
PROJECT=your-project make gcp-env-start    # the database, then the nodes back
PROJECT=your-project make gcp-env-status   # what is parked, and what it was parked from
```

These are the one part of this directory that needs **neither Terraform, nor state, nor
tfvars** — only credentials for the project. That is deliberate: parking is the operation
you want to perform from whatever machine you happen to be at, including one that has
never run an apply, and every coordinate is derived from `PROJECT` and `NAME_PREFIX`.

| | parked | keeps billing |
|---|---|---|
| both node pools | resized to zero — nodes, boot disks and Cloud NAT's per-VM charge go with them | |
| Cloud SQL | activation policy `NEVER`, which suspends the instance charge | its storage, and its backups |
| the GKE control plane | | $0.10/cluster-hour, which one zonal cluster's free-tier credit covers |
| the two LoadBalancer forwarding rules | | the control plane's and the console's alike |

The last two rows are why "the two charges that dominate the bill" is the honest phrasing
rather than "everything hourly": the control plane and the forwarding rules bill by the hour
too. They are small, and taking the forwarding rules down would mean deleting a `Service`,
which is a change to the workload rather than a power operation. Cloud SQL keeps no IP
charge here, because `environment/` gives the instance a private address only.

**Parking the cluster is safe because mode 2 already put every piece of state outside it** —
Postgres is Cloud SQL, blobs are GCS, the credential cipher is Cloud KMS, and `kubectl get
pvc -A` is empty by design. Nodes are genuinely disposable, so `start` has no restore step:
the control plane, etcd, the Deployments and the Helm release records never went away, and
pods reschedule themselves as soon as there is somewhere to put them. The console lives in
the same cluster and is parked and revived along with the platform.

**The parked sizes are stored on the cluster, not on your laptop.** `stop` writes each
pool's node count into a `power-saved-<pool>` cluster resource label before zeroing it, and
`start` restores from them and then clears every one it finds — so a colleague on another
machine can revive what you parked, and `start` restores the size that was actually running
rather than a default. If a label is ever missing, `start` refuses the whole revival and
asks for `NODES=<n>` instead of guessing, before it starts the database: `variables.tf`
defaults to two nodes a pool, that may not be this deployment's size, and a guess in that
direction doubles the bill the parking was for. Terraform does not declare `resource_labels`
on this cluster, which is what lets the labels survive an apply.

One thing to know about that label, because `gcloud`'s flag is misleadingly named:
`--update-labels` **replaces** the cluster's whole label set rather than merging into it, so
every write the script makes sends the complete set — its own labels and everyone else's.
Change that and a stop deletes `goog-terraform-provisioned`, and a resumed stop deletes the
size the interrupted attempt had already saved.

**Do not `terraform apply` while parked.** `node_count` is declared in
`environment/main.tf` and Cloud SQL's `activation_policy` is not, so an apply scales the
pools back up and leaves the database stopped — which is the one ordering this whole
mechanism exists to prevent. Run `make gcp-env-start` first. If it happens anyway, `start`
is still the way out: it finds the pools already running, resizes nothing, and clears the
stale marker that would otherwise keep CD skipping.

Order is why these are two commands and not four. Going down, the nodes drain before the
database goes; coming up, Cloud SQL reports `RUNNABLE` before any pod can land on a new node.
`RUNNABLE` is the state the Admin API reports, not a promise that Postgres is accepting
connections — it is the closest signal available from outside the VPC, and far better than
not waiting. `make gcp-power-test` asserts both orderings against a fake `gcloud`.

**CD skips a parked environment rather than failing on it.** `deploy.yml` checks for those
same labels after authenticating and, finding them, posts a notice and deploys nothing —
otherwise every push during a money-saving weekend would go red on the smoke test. It keys
on the label and not on the node count on purpose: a cluster at zero nodes that nobody
parked has fallen over, and that still fails loudly. Staging therefore stays at whatever
commit it was parked on; `gh workflow run deploy.yml --ref main` catches it up.

## Prerequisites

- A GCP project with billing enabled, and `gcloud auth application-default login`.
- Terraform ≥ 1.11 — `brew install hashicorp/tap/terraform`. It is not in Homebrew core.
  (1.11 for `password_wo`; `foundation/` alone needs only 1.5.)
- `kubectl` and `helm` for the deploy that follows.

## Running it

```sh
cat > deploy/gcp/foundation/terraform.tfvars <<'EOF'
project_id = "your-project"
EOF
cp deploy/gcp/foundation/terraform.tfvars deploy/gcp/environment/terraform.tfvars

make gcp-foundation-apply              # never destroyed — creates the secrets EMPTY
                                       # (re-apply it when foundation/ GAINS a resource:
                                       #  environment/ reads each one by name and fails
                                       #  the lookup if the apply that creates it has
                                       #  not run — #269's map-brain account, for one)
PROJECT=your-project make gcp-bootstrap  # fills them

# Only now: the foundation apply above is what enables the Cloud Build API, and the
# lookup answers SERVICE_DISABLED until it is on.
#
# If it still says SERVICE_DISABLED, wait a minute and retry: Service Usage can
# report an API enabled before its serving layer agrees, which is most likely on a
# just-created project. This is the one step in the sequence that is not idempotent
# by construction, so it is the one worth retrying rather than debugging.
#
# Written by replacing rather than appending: Terraform rejects a tfvars file that
# assigns the same variable twice, so a plain `>>` breaks the second time you run it.
# The `if` is load-bearing: a bare `sa=$(...)` does NOT stop an interactive
# shell when the lookup fails, so the block below would go on to delete a good
# assignment and write an empty one — turning the retryable SERVICE_DISABLED
# above into a corrupted tfvars file.
if ! sa="$(gcloud builds get-default-service-account --project your-project)" || [ -z "$sa" ]; then
  echo "lookup failed — tfvars left untouched; retry the command above" >&2
else
  tfvars=deploy/gcp/environment/terraform.tfvars
  touch "$tfvars"
  # grep exits 1 when it selects nothing — the valid case where the file held
  # only this assignment — and 2 on a read error. A blanket `|| true` would treat
  # a half-read file as an empty one and let the mv below replace a good tfvars
  # with a truncated one, so only status 1 is accepted. (`rc`, not `status`:
  # `status` is read-only in zsh, where it is a synonym for `$?`.)
  rc=0
  grep -vE '^[[:space:]]*cloud_build_service_account' "$tfvars" > "$tfvars.new" || rc=$?
  if [ "$rc" -gt 1 ]; then
    echo "could not read $tfvars — left untouched" >&2
    rm -f "$tfvars.new"
  else
    # ${sa##*/} keeps only the EMAIL. Which form comes back is not fixed —
    # gcloud 578.0.0 printed a bare email here, and the documented shape is
    # projects/…/serviceAccounts/EMAIL — and this strips a prefix if present
    # and leaves a bare email alone, so it does not matter which you get.
    echo "cloud_build_service_account = \"${sa##*/}\"" >> "$tfvars.new"
    mv "$tfvars.new" "$tfvars"
  fi
fi

make gcp-env-apply

# The cluster must be reachable before the next step, which runs inside it.
gcloud container clusters get-credentials \
  "$(terraform -chdir=deploy/gcp/environment output -raw cluster_name)" \
  --zone "$(terraform -chdir=deploy/gcp/environment output -raw zone)" \
  --project your-project

# Creates the platform's database role OUTSIDE cloudsqlsuperuser, and asserts it.
PROJECT=your-project make gcp-db-init
```

**The order is load-bearing.** `environment/` reads the administrator's password
ephemerally at apply time; run it before `gcp-bootstrap` and it fails naming the secret that
has no version. `gcp-bootstrap` is idempotent by *skipping*: a secret that already has a
version is left exactly as it is, because that version may be the only thing that can
decrypt something. Re-running it after an `environment/` teardown is the normal case and is
a no-op.

`gcp-db-init` comes last for a reason that is not politeness about ordering. Cloud SQL grants
`cloudsqlsuperuser` — CREATEDB and CREATEROLE — to every built-in user created through its
Admin API, which is the path `google_sql_user` takes. So Terraform creates exactly one such
account, `postgres`, and the platform is not it: `deploy/gcp/dbinit.sql` creates the
platform's own role with plain `CREATE ROLE`, which never goes through that path, and then
asserts the result — not a superuser, not a member of `cloudsqlsuperuser`, owner of the
platform database and of nothing else, over an encrypted session. A failed assertion fails
the step.

It runs as a Kubernetes **Job** rather than as a local `psql` because `environment/` gives
Cloud SQL a private address only: the instance is reachable from inside the VPC and from
nowhere else, including not from the machine running Terraform. Re-run it after every
`environment/` rebuild, and after rotating the password — the second case is the whole
rotation procedure.

`terraform.tfvars` is gitignored. `name_prefix` and `kms_location` must match between the two
configurations — `environment/` finds the foundation's resources by name, so a mismatch
surfaces as a missing data source rather than as a wrong-looking value. (`region` does not
need to match: no foundation resource is regional. `zone` must be inside `environment/`'s
`region`, which a precondition checks.)

**`bootstrap.sh` is a third consumer of that prefix**, and the only one that is not read from
a `.tfvars` file: it derives every secret id from the `NAME_PREFIX` environment variable,
which defaults to `map`. If you set `name_prefix` in
tfvars, pass it here too —

```sh
NAME_PREFIX=acme PROJECT=your-project make gcp-bootstrap
```

— because the default does not fail safe. It reports `secret map-db-password does not exist —
run 'make gcp-foundation-apply' first`, which is untrue and points at the wrong step; and if
a `map-*` foundation happens to exist in the project from another environment, bootstrap
fills *that* one's secrets instead and `make gcp-env-apply` then fails on a prefix it never
touched.

Neither apply target passes `-auto-approve`. Read the plan before the first one.

**Cloud Build's identity is a required variable** (`cloud_build_service_account`) because the
answer is bimodal and neither answer is safe to default. The split is by FIRST BUILD, not by
project creation date: a project whose first build ran before Google's 2024 rollout keeps
`PROJECT_NUMBER@cloudbuild.gserviceaccount.com`, and everything else — including an old
project that has never built — gets the Compute Engine default service account. So "how old
is this project" is not a question you can answer this with. There is no "empty means the
legacy default" — a default would grant writer to an account no build uses, and that surfaces
only at the first image push, after the apply has already created a cluster and a database.

`gcloud builds get-default-service-account --project your-project` names the one your project
uses. Run it **after** `make gcp-foundation-apply`: the foundation is what enables the Cloud
Build API, and the lookup fails with `SERVICE_DISABLED` until it is on.

Know what that answer costs you on the modern default. It returns the Compute Engine default
service account — which is also the identity the node pools run as, since neither pool sets
`node_config.service_account`. So on such a project the `artifactregistry.writer` grant lands
on every node, and a container that escapes to its node can overwrite the platform's own
images and wait to be pulled. This is accepted for staging and stated rather than hidden;
nothing here guards it, because a precondition demanding two distinct identities would reject
the very flow this section documents. Past staging, pass a dedicated build service account
and give the node pools an identity of their own.

**Rotating a database password** — and there are now two, rotated by different mechanisms.
Getting this backwards leaves a new secret version that nothing ever applies, so the value
in Secret Manager and the value the database will accept silently disagree until the next
restart:

| Secret | Whose password | How the rotation is applied |
| --- | --- | --- |
| `map-db-password` | the **platform's** role, the one in `DATABASE_URL` | add a version, then re-run `make gcp-db-init` — its `ALTER ROLE` is unconditional |
| `map-db-admin-password` | the Cloud SQL built-in **administrator** | add a version, then bump `db_password_version` and re-apply `environment/` |

The counter belongs to the administrator's password alone, because that is the only one
Terraform writes. Write-only arguments leave Terraform nothing to diff, so it is the only
signal that it should push the new value. Bumping it does nothing for the platform's role,
which Terraform does not manage.

Adding the version is the same for either — name the secret you mean. Both names carry the
`NAME_PREFIX` the environment was built with, so set it once at the top of the snippet rather
than relying on `NAME_PREFIX` being exported in whatever shell this gets pasted into:

```bash
prefix=map                          # the NAME_PREFIX this environment was built with
secret="$prefix-db-password"        # or "$prefix-db-admin-password"

pw="$(openssl rand -hex 32)"
if [[ "$pw" =~ ^[0-9a-f]{64}$ ]]; then
  printf '%s' "$pw" | gcloud secrets versions add "$secret" \
    --project your-project --data-file=-
else
  echo "generator failed — nothing written" >&2
fi
unset pw
```

Bash (or zsh), not `sh`: `[[ ... =~ ... ]]` is a bash construct and a POSIX shell such as
`dash` rejects it. Under bash 3.2 — the one macOS ships — it works as written.

A prefix that names no secret is safe — `gcloud secrets versions add` creates a version of an
**existing** secret and errors when there is none. The unsafe case is a prefix that names a
*different* environment's secret in the same project: that rotation succeeds, and what breaks
is the other environment, at its next restart rather than here.

(An `if`, not `... || { …; exit 1; }`: this is meant to be pasted into a shell, where `exit`
would close the terminal and `return` is an error outside a function.)

Generate and **check** before writing, exactly as `bootstrap.sh` does, rather than piping
`openssl` straight into `gcloud`. In a pipeline a failure on the left does not stop the
right: `gcloud` starts anyway, reads EOF, and stores an *enabled* zero-byte version. Here
that version becomes `latest` and then rides into Cloud SQL — through
`db_password_version` for the administrator, or through the next `make gcp-db-init` for the
platform's role.

The command substitution also strips the newline `openssl` prints, which `--data-file=-`
would otherwise store as a 65th byte — and a raw control character in `DATABASE_URL` makes
the DSN unparseable, long after the apply that accepted it. Keep the value on **stdin** via a
shell builtin: passing it as an argument would put it in `ps` output and shell history.

## Handover to Helm

The acceptance ran in the two phases the chart's own render-time exclusivity dictates
(plan 20, Decision 4); both are done, and their records are in
[docs/HISTORY.md](../../docs/HISTORY.md). **Mode-1** is bundled Postgres/MinIO/OpenBao with inline values —
it proves the images, the Kubernetes sandbox path, the gate and the end-to-end flows with
the fewest variables. **Mode-2** is Cloud SQL + GCS + KMS behind a pre-created Secret: the
production shape. This configuration creates the resources for both; mode-1 only consumes
two of them.

```sh
cd deploy/gcp/environment
gcloud container clusters get-credentials "$(terraform output -raw cluster_name)" \
  --zone "$(terraform output -raw zone)"

terraform output -raw helm_values_mode1 > ~/map-values-gcp.yaml
```

Write it **outside the repo tree**. The chart also needs credentials Terraform does not
supply, the natural place to put them is that same file, and a file of live credentials
inside a checkout is one `git add -A` from being committed.

That fragment carries exactly what mode-1 needs from Terraform — where the images live and
where sandboxes run — and nothing else. The chart deliberately generates no credentials (a
generated credential is unstable under `helm template` and GitOps), so these are yours to
supply alongside it, and the render fails naming each one you miss:

| Value | Why |
| --- | --- |
| `controlplane.apiKey` | the management API key |
| `brain.modelProviders` | at least one model route, as a JSON array |
| `postgresql.password` | bundled Postgres; must be URL-safe — it is embedded in `DATABASE_URL` |
| `minio.rootUser` / `minio.rootPassword` | bundled object storage |
| `openbao.staticSealKey` / `openbao.platformToken` | bundled OpenBao; the seal key is base64 of exactly 32 bytes |

Two more come from the **build** rather than from Terraform, and the render does *not* fail
without them — which is why they are called out here rather than left to be discovered:

| Value | Why |
| --- | --- |
| `image.tag` | the tag `cloudbuild.yaml` pushed. Empty falls back to the chart's `appVersion` (`0.3.0`), which this flow's Artifact Registry does not hold, so all three platform pods sit in `ImagePullBackOff` |
| `executor.gateImage` | the full `…/gate:TAG` reference. Empty is *valid* and means no gate: `limited` and vault-attached sessions fall back to the backend's own fail-closed networking, and credential substitution does not happen |

Pass them on the `helm` command line, **not** by appending to the values file. The fragment
already contains a top-level `image:` mapping, and a second one does not merge with it:
Helm accepts the duplicate key silently and the later mapping wins whole, so `registry` and
`repository` revert to the chart's `ghcr.io` defaults. That is the same
`ImagePullBackOff` this section exists to prevent, arrived at by trying to prevent it —
verified by rendering the duplicate, which produced
`ghcr.io/opensdlc-dev/managed-agent-platform/brain:TAG`.

Set `tag` **once** and let it drive both the build and the install. Deriving it twice — a
`_TAG` for the build and a fresh `git rev-parse` for the install — is how they come to
disagree: commit anything in between and the second one names an image that was never
pushed.

```sh
tag="$(git rev-parse --short HEAD)"
prefix="$(terraform -chdir=deploy/gcp/environment output -raw artifact_registry)"

# From the repository root: the build context is the whole module.
gcloud builds submit --config deploy/gcp/cloudbuild.yaml \
  --substitutions=_IMAGE_PREFIX="$prefix",_TAG="$tag" .

helm install map deploy/helm/managed-agent-platform \
  --namespace map --create-namespace \
  -f ~/map-values-gcp.yaml \
  --set "image.tag=$tag" \
  --set "executor.gateImage=$prefix/gate:$tag"
```

**That build needs BuildKit, and `cloudbuild.yaml` now asks for it explicitly.** The
Dockerfile's first line is `FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build`,
and `BUILDPLATFORM` / `TARGETOS` / `TARGETARCH` are BuildKit-only variables. Under Cloud
Build's classic builder they expand to the empty string and the daemon rejects the platform
specifier outright — `failed to parse platform : "" is an invalid component of ""` — so
`env: ["DOCKER_BUILDKIT=1"]` on both docker steps is what makes the build run at all, not a
performance choice. This was a real defect in this path, not something continuous delivery
introduced: the file had always described a build nobody had run since the Dockerfile gained
that line.

**On a project created under an organisation, name the build's service account.** Such a
project no longer gets the automatic Editor grant on the Compute Engine default service
account, so the identity Cloud Build otherwise picks cannot read the source tarball
`builds submit` has just uploaded — `does not have storage.objects.get access to …
<project>_cloudbuild`. Add `--service-account="projects/<project>/serviceAccounts/<sa>"` and
give that account `roles/storage.admin` on the `_cloudbuild` bucket and
`roles/logging.logWriter` on the project; the "Continuous delivery" section below records the
two grants this environment made and why `CLOUD_LOGGING_ONLY` becomes mandatory alongside
them.

The sandbox-placement values in that fragment are a **pair, and neither half works alone**.
Without the tolerations every sandbox pod stays `Pending` forever — the pool's taint has no
other tolerator. Without the node selector sandbox pods land on the platform pool, and the
dedicated pool buys nothing.

Mode-2's inputs are emitted individually, to be assembled into the pre-created Secret that
the chart's `existingSecret` value names — its full key list is in
[docs/deploy-gcp.md](../../docs/deploy-gcp.md#the-two-modes), and there is no script for it
because a script that touched every one of these would be a credential-handling tool of its
own. The mode-2 acceptance run built it with a single `kubectl create secret generic`:

```sh
terraform output -raw  kms_key_name                              # gcpKMS.keyName / GCPKMS_KEY_NAME
terraform output -json controlplane_service_account_annotation   # controlplane.serviceAccount.annotations
terraform output -json brain_service_account_annotation          # brain.serviceAccount.annotations
terraform output -json executor_service_account_annotation       # executor.serviceAccount.annotations
terraform output -raw  blob_backend                              # BLOB_BACKEND
terraform output -raw  blob_bucket                               # BLOB_BUCKET
terraform output -raw  sql_instance_connection_name              # cloudSQLProxy.instanceConnectionName
```

The last one is for a deploy you drive by hand. CD does not read it: `deploy.yml` asks the
Cloud SQL Admin API for the connection name at deploy time, so no operator's project is
written into this repository — the placeholder in `staging-values.yaml` neutralises the
project the way the service-account emails beside it do, and carries only the region and
instance name that `environment/`'s own public defaults already spell out. Why the proxy is
the shape this deployment uses, and the one migration step that is not automatic, are under
"Continuous delivery" below.

**Apply all three.** The brain's annotation used to be the one that was only sometimes
needed — it existed for the Cloud SQL Auth Proxy (chart: `cloudSQLProxy.enabled`), and under
the direct-connection path no component uses a Google identity for the database. #240 ended
that: the brain reads the blob bucket (rubric snapshots and deliverables) and its account
now holds `roles/storage.objectViewer` there, so the annotation is required in mode 2
whichever database path you take. Skipping it does not fail loudly — the brain's reads
degrade rather than error, grading against the outcome description alone — which is the
reason to apply it rather than to find out later.

Workload Identity needs both halves and this configuration only applies one of them: the
IAM binding permitting a Kubernetes ServiceAccount to impersonate the Google service
account is created here; the **annotation** on that KSA comes from the chart values above.
`namespace` and `release_name` are what the binding names — install the chart somewhere
else and ADC fails at pod startup rather than denying a permission later.

## Single sign-on: Google's identity provider, not ours

[docs/plan/31_console-sso-rbac.md](../../docs/plan/31_console-sso-rbac.md) bundles a
hardened Casdoor as the self-host default IdP — in compose, and in the Helm chart behind
`casdoor.enabled`. **This environment deploys none of it**, and that is the plan's decision
rather than a gap: on GCP the identity provider is Google's, so Cloud Identity Platform or
Workspace does the SSO behind Identity-Aware Proxy, `casdoor.enabled` stays `false`, and the
human lane the platform runs here is `IDENTITY_MODE=trusted_proxy` with the `gcp-iap` preset
— configured below, and left switched off until there is an IAP to switch it on behind.

**The preset is most of the configuration, and it refuses to be talked out of any of it.**
[`internal/identity/preset.go`](../../internal/identity/preset.go) fixes the assertion header
(`x-goog-iap-jwt-assertion`), the issuer (`https://cloud.google.com/iap`), the key set
(`https://www.gstatic.com/iap/verify/public_key-jwk`) and the algorithm (ES256).
Supplying `IDENTITY_PROXY_HEADER`, `_ISSUER`, `_KEYS_URL` or `_ALGS` beside it is a **boot
error** — `configureGCPIAP` in [`internal/identity/config.go`](../../internal/identity/config.go)
answers `IDENTITY_PROXY_PRESET=gcp-iap supplies IDENTITY_PROXY_HEADER; unset it` — not a
variable that is quietly ignored. So the chart's values carry exactly `identity.mode`,
`identity.proxy.preset`, `identity.proxy.audience`, `identity.claims.roles` and
`identity.roleMap`, and [`staging-values.yaml`](./staging-values.yaml) carries those five and
no more — `mode` empty for now, the other four spelled as a working deployment wants them.

**The audience is the whole tenant boundary, and calling it configuration undersells it.**
The gstatic key set is global across every Google Cloud customer, so every customer's IAP
assertion is signed by a key this deployment trusts; what separates them is `aud`. An empty
or wrong audience is not a broken login, it is cross-customer authentication — which is why
`configureGCPIAP` requires the value and rejects anything not shaped `/projects/…`. This
configuration therefore derives it rather than asking anyone to transcribe it:

| Knob | Where | What it does |
| --- | --- | --- |
| `iap_backend_service` | `environment/terraform.tfvars` | Names the backend service IAP is enabled on. Empty (the default) wires nothing at all. |
| `iap_members` | same | The IAM member strings admitted, each granted `roles/iap.httpsResourceAccessor` on that backend service. `allUsers`/`allAuthenticatedUsers` are refused by the variable's validation. |
| `identity_proxy_audience` | `terraform output` → the chart's `identity.proxy.audience` | `/projects/PROJECT_NUMBER/global/backendServices/BACKEND_SERVICE_ID`, composed from the project's number and the backend service's server-assigned numeric id. |

Nothing here creates the backend service, because nothing here can: a GKE Ingress or Gateway
creates it at admission time, under a generated name of the form
`k8s1-HASH-NAMESPACE-SERVICE-PORT-HASH`. `gcloud compute backend-services list --global`
names it, and **it changes when the Service behind it is recreated** — a stale value fails
every human login while every machine credential keeps working, which is the quiet half of
that failure. The audience is read back from the live backend service rather than taken as a
second variable, so what the output prints and what the IAM bindings admit people to cannot
name different backend services.

**Terraform does not create the OAuth consent screen or the OAuth client, and as of March
2026 it could not.** The provider says so itself — `terraform validate` on a
`google_iap_brand` resource against `hashicorp/google` 7.42.0 answers:

```text
Warning: Deprecated Resource
This resource is deprecated on Jan 22, 2025. After Jan 19, 2026 the
`google_iap_brand` Terraform resource will no longer function as intended due to
the deprecation of the IAP OAuth Admin APIs. New projects will not be able to use
these APIs. March 19, 2026 The IAP OAuth Admin APIs will be permanently shut down.
Access to this feature will no longer be available.
```

Two further reasons would have kept it out even before that date, and they are worth
recording because "add the brand to Terraform" is the obvious-looking next step: an IAP
brand has **no delete at all**, so it belongs to the durable half by the same argument the
KMS key does; and `google_iap_client` exports its `secret`, which would put a live credential
in a state file this configuration promises holds none ("No secret value is in either
configuration", above). Configure the consent screen in the Cloud console, use IAP's
Google-managed OAuth client where you can, and treat a custom client as an out-of-band
credential like the Workload Identity Federation provider already is.

**Roles come from the email claim, because an IAP assertion names a person rather than their
groups.** `identity.claims.roles: email` points the mapper at the claim the assertion
actually carries, and `identity.roleMap` maps addresses to `admin` / `developer` / `viewer`.
Two consequences to hold on to. A claim value may contain neither `,` nor `=` — `parseRoleMap`
splits on both, and a value carrying one fails startup rather than mapping something
unintended; email and Google group addresses are unaffected. And **admission is not
authorization**: `iap_members` decides who reaches the control plane, the role map decides
what they may do, and a human who passes IAP and matches no entry in the map holds no role
and is refused by every role-gated route — 403, not 401. An IdP that does place a group claim
in the assertion is configured by pointing `identity.claims.roles` at that name instead;
nothing in the platform presumes `email`.

**The topology is a requirement, not a diagram.** The proxy must front the *control plane's
own* backend service, so that every human request carries an assertion audience-bound to it
and the browser reaches the platform directly rather than through a server-to-server console
hop — which would make IAP authenticate the console's workload identity and collapse every
user onto one principal. Machine traffic (`/v1` keys, worker environment keys) rides a
**separate, non-proxied backend** at the load balancer.

**None of this is switched on here, and the reason is one line above it in this file.** IAP
lives on an HTTPS load balancer; this environment publishes the control plane as a
plain-HTTP L4 LoadBalancer with no domain, so there is no backend service to enable IAP on
and nothing for a certificate to be issued against. Both variables default to empty and the
audience output is then empty; `staging-values.yaml` carries the platform half spelled out
but with `identity.mode` empty, so the lane is off. That is deliberate rather than
unfinished: enabling it would grant no human anything without a proxy in front, while adding
a dependency on an external key set to a boot that otherwise needs only Postgres — and the
pipeline deploys with `--wait --atomic`, where a transient fetch failure becomes a rolled-back
release. Turning it on once the front door exists is `mode: trusted_proxy` plus a real
audience, with everything else in that block already in place. Building the front door is
the GKE Gateway work [docs/deploy-gcp.md](../../docs/deploy-gcp.md#exposing-the-control-plane)
describes; these knobs are what configures IAP once it exists.

## Continuous delivery

Everything above is the manual path, and it stays the manual path. What
[`.github/workflows/deploy.yml`](../../.github/workflows/deploy.yml) automates is only the
last two lines of it — the build and the install — against **one** staging environment, in
**mode 2**, on every push to `main` and on `workflow_dispatch`.

| Step | Who runs it |
| --- | --- |
| `make gcp-foundation-apply` | a human, interactively |
| `PROJECT=… make gcp-bootstrap` | a human, once — it fills the two database-password secrets |
| `make gcp-env-apply` | a human, interactively |
| `PROJECT=… make gcp-db-init` | a human, after every `environment/` rebuild and every password rotation — **mode 2 genuinely depends on it** |
| creating `controlplane-api-key`, `database-url` and `model-providers` | a human, once — `bootstrap.sh` does not create these |
| replacing the `model-providers` placeholder | a human, once |
| setting the eleven Actions **variables** below | a human, once, and **before** the first run: until they exist the workflow stops at its second step, so every push to `main` in the meantime is a red run rather than a deployment |
| build and push the four images → assemble the `map-platform` Secret → `helm upgrade --install` → smoke | **CD** |

**Three of those secrets are not `bootstrap.sh`'s.** It owns exactly `<prefix>-db-password`
and `<prefix>-db-admin-password`, because those are the two Terraform reads back. The three
the pipeline reads — `controlplane-api-key`, `database-url`, `model-providers` — are created
out of band by whoever stands the environment up, and there is no script for them here for
the same reason there is none for the mode-2 Secret: a tool that generated all of them would
be a credential-handling tool of its own. `controlplane-api-key` is any high-entropy value
(`openssl rand -hex 32`), `database-url` is composed below, and `model-providers` is the one
a human must supply.

**`gcp-db-init` is a prerequisite here, not a formality.** Mode 1 never ran it at all — the
bundled Postgres creates its own role from `postgresql.password`. Mode 2's `database-url`
names the `map` role on the Cloud SQL instance, and
[`dbinit.sql`](./dbinit.sql) is the only thing that creates it: with plain `CREATE ROLE`,
outside `cloudsqlsuperuser`, and it asserts the result. Skip it and the pipeline still goes
green through `helm upgrade` and then fails at the first connection, because the DSN names a
role the database does not have.

**CD runs no Terraform, and the split is not a phase.** The applies are interactive on
purpose and the state is local by design (plan 20, Decision 9; "State, and recovering it"
above) — and it lives in the main checkout, not anywhere a runner could reach. A pipeline
that ran them would need that state somewhere it could reach, and would be one bad plan away
from destroying the Cloud SQL instance the platform's data lives in — which `environment/`
is built to allow, because a staging rebuild has to be one command. So the pipeline assumes
the environment exists: Terraform, `gcp-bootstrap` and `gcp-db-init` stay human-driven, and
CD owns exactly **build → push → deploy → smoke**.

### The deployment's coordinates

**Nothing in this repository names them.** `deploy/` here is a **reference deployment, not a
template**: it demonstrates a shape that works, and the one deployment it was proven against
is described by variable rather than by name, because this repository is public. Each is a
GitHub Actions **variable** (Settings → Secrets and variables → Actions → Variables), not a
secret — none of them grants anything, the credentials are in Secret Manager, and the
Workload Identity Federation trust policy is what actually protects the deployment. They are
held outside the repository because naming one operator's project, cluster, registry,
buckets, key and identities hands a reader a target list, and ties an open-source project to
a cluster nobody cloning it can reach.

**Where each value comes from**, because "read them off `terraform output`" is true of most
of them and not of all — and the three exceptions are exactly the ones a new deployment would
be left hunting for. `E` is `terraform -chdir=deploy/gcp/environment output -raw`, `F` is the
same against `foundation/`:

| Variable | What it names | Where to get it |
| --- | --- | --- |
| `GCP_PROJECT_ID` | the project everything else lives in | **not an output** — it is the `project_id` you set in `terraform.tfvars` |
| `GKE_CLUSTER`, `GCP_ZONE` | the cluster and its zone | `E cluster_name`, `E zone` |
| `ARTIFACT_REGISTRY` | `HOST/PROJECT/REPOSITORY` — the image prefix. The tag is always the commit SHA, and the chart's `registry`/`repository` split is taken off this one value at its first slash | `E artifact_registry` |
| `WIF_PROVIDER` | the Workload Identity Federation provider the job's OIDC token is exchanged at | **not in this repository's Terraform** — the pool and provider are created by hand; read the full resource name back with `gcloud iam workload-identity-pools providers describe POOL_PROVIDER … --format="value(name)"` |
| `DEPLOY_SERVICE_ACCOUNT` | the identity that provider lets this repository impersonate | **not in this repository's Terraform** — created by hand alongside the provider, and bound to this repository separately |
| `BLOB_BUCKET` | the GCS bucket, written into the mode-2 Secret as `blob-bucket` | `E blob_bucket` |
| `KMS_KEY_NAME` | the CryptoKey resource name, written in as `gcpkms-key-name` | `E kms_key_name` (`F kms_key_name` is the same key) |
| `CONTROLPLANE_SERVICE_ACCOUNT`, `BRAIN_SERVICE_ACCOUNT`, `EXECUTOR_SERVICE_ACCOUNT` | the three Workload-Identity Google service accounts the chart annotates each component's ServiceAccount with | `F controlplane_service_account`, `F brain_service_account`, `F executor_service_account` — bare emails. (`environment/`'s `*_service_account_annotation` outputs hold the same emails wrapped in the chart's annotation map, which is the wrong shape for a variable.) |

The two "not in Terraform" rows are not an oversight to be tidied away: the WIF provider is
deliberately outside this configuration — see the attribute-condition command below, which
exists here precisely because nothing in the repository would otherwise record it — and the
deploy identity is created with it. They are named here so that setting up a fresh deployment
is a list to work through rather than a guess, since the workflow's guard refuses to run
until all eleven exist.

Three names are deliberately **not** variables — `K8S_NAMESPACE`, `K8S_SECRET` and
`HELM_RELEASE` stay literals in the workflow, because they name the *chart's* own objects
rather than an operator, and `K8S_SECRET` in particular has to equal `existingSecret` in
[`staging-values.yaml`](./staging-values.yaml), which is in git: a variable would let the two
drift into a release bound to a Secret nothing creates, with no diff to notice it in.

The workflow **asserts every one of them** before it authenticates or builds anything, in a
step that runs second — after the ref guard, which refuses an unauthorised ref while telling
it nothing. An unset variable renders as the empty string rather than failing, and
`gcloud --project ""`, `get-credentials "" --zone ""` and an image tag with no repository all
fail late and confusingly. Whitespace and commas are rejected as well as emptiness: a value
of one space is no configuration while passing a `-z` test, and four of them —
`ARTIFACT_REGISTRY` and the three service accounts — reach `helm --set-string`, whose
assignment list is comma-separated.

The cost of the move is that the deployment target is no longer reviewable in the diff that
changes it — a variable is edited in a settings page, not in a PR. That is the trade, and
that assertion step is what keeps it from being silent.

**What this is not is concealment, and the distinction matters because the weaker claim is
the one people act on.** A run's logs are public on a public repository, and several of these
values appear in them by the ordinary operation of the tools: `docker push` prints the
repository it is pushing to, `gcloud container clusters get-credentials` prints the cluster
it wrote a kubeconfig entry for, and the smoke step prints the LoadBalancer's external
address on purpose. Registering them as `::add-mask::` would close that and was rejected: a
mask is a literal string replacement across the whole log, so the same substitution that
hides the project id also hides it inside every diagnostic that names it — `cannot read
Secret Manager secret 'x' in ***` is a worse error message than the one it replaces, and the
same reasoning already keeps short values unmasked in the Secret step. What the move actually
buys is the two things the issue asked for: a clone's **deployment configuration** — this
workflow, this values file, the commands in this runbook — names no operator, and `deploy/`
reads as a reference deployment instead of a half-edited template. Anyone determined to read
a public build log was never the threat model.

Note the width of that claim, because the stronger one is not true: a clone **does** still
carry the identifiers, in [docs/HISTORY.md](../../docs/HISTORY.md) and in git history, both
of which this change leaves alone on purpose — the first is the record of what was actually
run, and rewriting the second is not worth it for identifiers that are not credentials. What
changed is that nothing you would *configure or execute* names them.

Commands below name the variables as the table does. Load them into the shell first —
`gh variable list` only prints a table and exports nothing:

```bash
vars='GCP_PROJECT_ID GCP_ZONE GKE_CLUSTER ARTIFACT_REGISTRY WIF_PROVIDER
      DEPLOY_SERVICE_ACCOUNT BLOB_BUCKET KMS_KEY_NAME
      CONTROLPLANE_SERVICE_ACCOUNT BRAIN_SERVICE_ACCOUNT EXECUTOR_SERVICE_ACCOUNT'

unset $vars                       # clear FIRST — see below
for v in $vars; do
  val="$(gh variable get "$v")" && [ -n "$val" ] && export "$v=$val"
done
for v in $vars; do eval ": \"\${$v:?did not load — see the table above}\""; done
```

**`gh` reads whichever repository the current checkout points at**, which is the behaviour
you want and the reason no `--repo` is written here: a fork's operator gets the fork's
variables, and pinning `OpenSDLC-Dev/managed-agent-platform` into a public runbook would put
back one of the coordinates this whole section exists to remove. The consequence is that this
block belongs in a checkout of the repository you are deploying — run it somewhere else and
it loads that repository's values, which will pass the assertion below while being the wrong
deployment. `gh repo view --json nameWithOwner -q .nameWithOwner` is the one-line check.

**The `unset` is the load-bearing line, and it goes first.** A partial load is the dangerous
state, not a failed one: if `gh` errors on one name — an expired token, a variable renamed,
a network blip — and this shell already holds values from an earlier load or another
environment, then the assertion below passes on **stale** coordinates and the `gcloud`
commands further down operate on the wrong project or the wrong service account, saying
nothing. Clearing every name before the load means a variable that does not arrive is
absent rather than out of date, which is a state the assertion can see.

**It reads the eleven by name rather than exporting the whole list.** `gh variable list`
piped into an `eval` would export whatever else is configured on the repository, under
whatever name it has — including names the rest of this file reads for a different purpose.
`PROJECT` is the sharp one: the table above hands it to `make gcp-bootstrap` and
`make gcp-db-init`, so a repository variable of that name would silently redirect both, and
`TF_VAR_*` is the same hazard aimed at Terraform. Naming what you want keeps the blast
radius to this section's eleven.

All eleven are asserted, not the few any one command needs, because the commands below spend
`$DEPLOY_SERVICE_ACCOUNT` as well as `$GCP_PROJECT_ID`, and a half-loaded environment is
exactly what turns a promised named stop into `--member="serviceAccount:"` and an argument
parse error. `:?` names the first variable that did not load and stops there, without closing
an interactive shell.

**There is no GitHub secret in this repository, and adding one would be a regression.** The
job mints an OIDC token, Workload Identity Federation exchanges it for a short-lived
impersonation of `DEPLOY_SERVICE_ACCOUNT`, and every credential is then read from Secret
Manager at run time. So a rotation is `gcloud secrets versions add` plus a re-run of the
workflow — no repository setting to update afterwards, and no long-lived key in a settings
page to leak. The provider is locked to `assertion.repository_owner == 'OpenSDLC-Dev'`, and
each repository is bound separately, so a fork cannot use it.

**Closed: the provider asserts the ref, so the ref a `workflow_dispatch` selects is no longer
trusted.** A dispatch can name a tag as readily as a branch, which is why the condition tests
both — see `ref_type` below. GitHub runs a dispatched workflow from the ref it was dispatched
on, so without that condition anyone who could push a ref to this repository could push an
edited `deploy.yml`, dispatch it, and have the provider hand that run the deploy identity —
Secret Manager, Cloud Build and the cluster, around the review boundary `main` exists to be.

There are two controls, and the order matters. The workflow's first step refuses a run whose
`github.ref` is not `refs/heads/main`, and it runs *before* the auth action, so an ordinary
dispatch from any other ref stops there and never reaches a token exchange at all. That guard
stops the accident but cannot stop the attack, because the ref that edits the file deletes the
step with it — which is why the real control has to be cloud-side, where an edited workflow
cannot reach it. It is one command — **applied to this project**, and recorded here because
the provider is not in this repository's Terraform, so this is the only place the command that
sets it, and the read-back that checks it, are written down:

Every coordinate is taken **out of `$WIF_PROVIDER` itself**, and that is not tidiness. This
condition is the control that survives a branch deleting the workflow's own guard, so a
command that configured a *different* provider than the one the job exchanges its token at
would leave the real one unconditioned while reading as though the work were done. A
hard-coded `github-oidc` in pool `github` does exactly that on any deployment whose
`WIF_PROVIDER` names another pool or provider — which is now every deployment but this one:

```sh
wif_project="${WIF_PROVIDER#projects/}";                  wif_project="${wif_project%%/*}"
wif_location="${WIF_PROVIDER#*/locations/}";              wif_location="${wif_location%%/*}"
wif_pool="${WIF_PROVIDER#*/workloadIdentityPools/}";      wif_pool="${wif_pool%%/*}"
wif_name="${WIF_PROVIDER##*/providers/}"

gcloud iam workload-identity-pools providers update-oidc "$wif_name" \
  --project="$wif_project" --location="$wif_location" \
  --workload-identity-pool="$wif_pool" \
  --attribute-condition="assertion.repository_owner == 'OpenSDLC-Dev' && assertion.ref == 'refs/heads/main' && assertion.ref_type == 'branch'"
```

`--project` gets the project **number** the resource name carries rather than
`$GCP_PROJECT_ID`; `gcloud` accepts either, and the number is the one that is guaranteed to
name the project the pool is in — a provider may live beside the workload rather than in it.

`ref_type` rides along because a *tag* named `main` would present `refs/tags/main` — not
equal, so the ref test already rejects it, but stating both makes the intent legible rather
than incidental. Read the live condition back with:

```sh
gcloud iam workload-identity-pools providers describe "$wif_name" \
  --project="$wif_project" --location="$wif_location" \
  --workload-identity-pool="$wif_pool" \
  --format="value(attributeCondition,state)"
```

It applies to every repository bound to this provider, which is what is wanted: both this
repository and `managed-agent-console` deploy from `main` and dispatch only to redeploy or
to finish a rotation, both of which are dispatches *on* `main`. Verify it the way it will
first be exercised — dispatch from a throwaway branch and confirm the auth step fails, not
the guard.

**CD does not use Cloud Build, and the reason is worth keeping.** The pipeline's first real
run failed at the build step in under a second, before a byte was uploaded:

```text
ERROR: (gcloud.builds.submit) The user is forbidden from accessing the bucket
[PROJECT_cloudbuild]. Please check your organization's policy
or if the user has the "serviceusage.services.use" permission.
```

It kept failing with `roles/storage.admin` on that bucket **and**
`roles/serviceusage.serviceUsageConsumer` on the project — the documented remedy for exactly
that message. The trap underneath it: **`gcloud builds submit` stages the source as the
CALLER, not as the build's `--service-account`.** Every manual run that worked was called by
a human with Owner, so none of them exercised the path CI takes, and no amount of granting
the *build* identity more could have.

So the workflow builds on the runner and pushes to Artifact Registry. That needs one
permission the deploy identity already holds — `roles/artifactregistry.writer` — and no
bucket, no staging upload and no Cloud Build API. `cloudbuild.yaml` remains the **manual**
path's build definition; keep the two saying the same thing, which is why the workflow writes
its four tags out rather than inferring them.

**Three IAM grants live outside Terraform, and each blocks a different path when it is
missing.** All were made by hand and are recorded here because nothing in the repository
would otherwise say they exist:

| Grant | Scope | Why |
| --- | --- | --- |
| `roles/logging.logWriter` | the project | `cloudbuild.yaml` sets `options.logging: CLOUD_LOGGING_ONLY`, which is *mandatory* once a build names its own service account — with a user-specified identity the API refuses a build that would write to the default logs bucket |
| the `mapCdRbacWriter` custom role, below | the project | the chart renders a namespaced `Role` and `RoleBinding` for the executor, and `roles/container.developer` carries only `get`/`list` on RBAC resources — so `helm upgrade` is refused the moment either object's rendered content changes. **This role is half the remedy**: an in-cluster basis Role, also below, answers a second gate that Cloud IAM does not reach |
| `roles/cloudsql.viewer` | the project | `deploy.yml` asks the Cloud SQL Admin API for the instance's connection name rather than composing it, which needs `cloudsql.instances.get`. The three *workload* identities' `roles/cloudsql.client` (`environment/iam.tf`) does not cover the deployer, which is not in this repository's Terraform at all. Without it the deploy fails at the resolve step — early, before the image build, but on every push |

The third is what the Cloud SQL Auth Proxy cutover added, and it is read-only:

```sh
gcloud projects add-iam-policy-binding "$GCP_PROJECT_ID" \
  --member="serviceAccount:$DEPLOY_SERVICE_ACCOUNT" --role=roles/cloudsql.viewer
```

`roles/storage.admin` on the project's `_cloudbuild` staging bucket and
`roles/serviceusage.serviceUsageConsumer` on the project were also granted to the deploy
service account while diagnosing the above. Neither is needed by CD anymore. They are left in
place because the manual `gcloud builds submit` path still uses that bucket, and are named
here so that a later tidy-up knows what they were for.

They are on the deploy service account, and what makes them necessary is naming a build
service account at all — `--service-account="projects/…/serviceAccounts/…"`, which the manual
path must pass here. **A project created under an organisation no longer gets the automatic
Editor grant on the Compute Engine default service account**, so the identity Cloud Build
would otherwise pick cannot read its own source upload. That is not a hypothetical — it is
how the first submission failed:

```text
PROJECT_NUMBER-compute@developer.gserviceaccount.com does not have storage.objects.get
access to the Google Cloud Storage object. ... PROJECT_cloudbuild
```

Reproduce the two Cloud Build grants with (the variables come from the loader in "The
deployment's coordinates" above; `${GCP_PROJECT_ID}` is braced in the bucket name because
`$GCP_PROJECT_ID_cloudbuild` would be read as one variable name that does not exist, and
would silently address `gs://_cloudbuild`):

```sh
gcloud projects add-iam-policy-binding "$GCP_PROJECT_ID" \
  --member="serviceAccount:$DEPLOY_SERVICE_ACCOUNT" --role=roles/logging.logWriter
gcloud storage buckets add-iam-policy-binding "gs://${GCP_PROJECT_ID}_cloudbuild" \
  --member="serviceAccount:$DEPLOY_SERVICE_ACCOUNT" --role=roles/storage.admin
```

**The chart renders a namespaced `Role` and `RoleBinding` for the executor**, and writing
them is guarded twice, by systems that cannot see each other — which is why closing one gate
only moves the error. The pair is
[`executor-rbac.yaml`](../helm/managed-agent-platform/templates/executor-rbac.yaml), carrying
the `pods` and `pods/exec` rules the Kubernetes sandbox provider needs.

**Cloud IAM answers first.** The deploy identity holds `roles/container.developer`, which
carries `get` and `list` on `roles` and `rolebindings` and no write verb at all —
deliberately, on Google's side: a namespaced deployer able to write RBAC objects could mint
itself a Role granting more than it holds.

```text
Error: UPGRADE FAILED: release map failed, and has been rolled back due to
rollback-on-failure being set: cannot patch "map-managed-agent-platform-executor" with kind
Role: … is forbidden: User "…" cannot patch resource "roles" in API group
"rbac.authorization.k8s.io" in the namespace "map": requires one of
["container.roles.update"] permission(s) in Cloud IAM …
```

**Kubernetes answers second**, and its refusal reads as though the first fix was the wrong
one. Its escalation guard will not let a principal write a Role granting permissions the
principal does not itself hold, and what it holds is resolved from **RBAC objects only**:
GKE's IAM permissions reach the API server through a separate authorization webhook the rule
resolver cannot see. So an identity carrying every `container.pods.*` permission in Cloud IAM
still counts as holding nothing here.

```text
cannot patch "map-managed-agent-platform-executor" with kind Role: … is forbidden: user "…"
is attempting to grant RBAC permissions not currently held:
{APIGroups:[""], Resources:["pods"], Verbs:["create" "get" "list" "delete"]}
{APIGroups:[""], Resources:["pods/exec"], Verbs:["create"]} && … with kind RoleBinding: …
```

**Neither gate is reached until a rendered RBAC object changes.** Helm issues a write only
where what it renders differs from what it recorded, so a chart whose RBAC objects render
identically run after run exercises neither permission, and no length of green history is
evidence that the deploy identity can write them at all. A chart-version bump is the usual way
it surfaces, since `map.labels` stamps `helm.sh/chart` and `app.kubernetes.io/version` onto
every object the chart renders, these two included. A pipeline that does its own first install
meets the same wall one step earlier, at `create` rather than `patch`.

**A custom role rather than `roles/container.admin`, and an in-cluster basis rather than
`escalate`** — both deliberately. `container.admin` closes both gates at once, because it
carries `container.roles.escalate` and `container.roles.bind`; but those are exactly the verbs
that let a principal grant more than it holds, which is the entire reason
`container.developer` withholds RBAC writes, and it brings cluster create and delete along
with them. The pair below closes the same two gates while granting neither verb. The custom
role answers Cloud IAM. A small Role holding just the executor's own rules, bound to the
deploy identity, answers Kubernetes — and doubles as a reviewable ceiling, since it is the
complete list of what CD is permitted to grant. It is not chart-managed: it is the permission
that lets the chart be installed, so it cannot come from the chart.

Keep its rules equal to the executor Role's in
[`executor-rbac.yaml`](../helm/managed-agent-platform/templates/executor-rbac.yaml). Narrowing
the executor Role is safe to land first; **widening it is not** — the basis is read before the
patch is applied, so a rule the basis does not yet carry is refused, and CD goes red until a
human adds it here. That ordering is the ceiling working as intended, not a defect.

```sh
gcloud iam roles create mapCdRbacWriter --project="$GCP_PROJECT_ID" \
  --title="MAP CD RBAC writer" --stage=GA \
  --permissions=container.roles.create,container.roles.update,container.roles.delete,container.roleBindings.create,container.roleBindings.update,container.roleBindings.delete
gcloud projects add-iam-policy-binding "$GCP_PROJECT_ID" \
  --member="serviceAccount:$DEPLOY_SERVICE_ACCOUNT" \
  --role="projects/$GCP_PROJECT_ID/roles/mapCdRbacWriter"
```

The custom role is granted at the **project**, because GKE offers no narrower scope for these
permissions: a cluster takes no IAM policy of its own, and IAM has no namespace dimension —
namespace scoping in GKE is Kubernetes RBAC, which is the basis Role's job.

**What the basis bounds is which rules may be granted, not to whom.** `create` and `update` are
checked against it, and it grants nothing outside namespace `map` — elsewhere the deploy
identity resolves only to what every authenticated principal already holds, so the most it
could mint there is a Role granting what the cluster grants everyone anyway. Inside `map` it
can bind the basis's own rules to a subject of its choosing; the ceiling is on the rules, and
those are the executor's.

**What the basis does not bound is destruction**, and `delete` is not the only verb that
reaches: the guard checks the rules being *granted* and never compares them with the object's
previous ones, so an `update` stripping a Role to `rules: []` grants nothing and passes.
Project-wide, then, the deploy identity can empty a namespaced Role, and delete a Role or a
RoleBinding, in any cluster in the project. All of that is denial of service rather than
privilege escalation, since RBAC carries no deny rules and removing a grant can only subtract,
and all of it is far narrower than `roles/container.admin`. It is the residual, and dropping
the two `delete` permissions would not close it: `delete` is there for `--atomic`, which tears
a failed *first* install down whole, RBAC objects included.

`kubectl create role` cannot express the basis: repeated `--verb` flags apply to every
`--resource` named, which would hand `pods/exec` the three verbs it must not have and make the
basis wider than the Role it exists to bound. So it is a manifest — unquoted heredoc, so the
identity loaded in "The deployment's coordinates" is the one bound, and `map` is the namespace
the workflow installs into.

**Apply it as a principal that already holds these rules**, or one holding both `escalate` and
`bind` — in practice the human with Owner who stood the cluster up. The deploy identity cannot
apply its own basis: creating a Role granting `pods` and `pods/exec` trips the very guard this
exists to satisfy, and the RoleBinding needs `bind` on the Role it references.

```sh
kubectl apply -f - <<YAML
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: { name: map-cd-deployer-rbac-basis, namespace: map }
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["create", "get", "list", "delete"]
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: { name: map-cd-deployer-rbac-basis, namespace: map }
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: map-cd-deployer-rbac-basis
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: User
    name: $DEPLOY_SERVICE_ACCOUNT
YAML
```

[`staging-values.yaml`](./staging-values.yaml) is the versioned input the pipeline reads, and
it is **mode 2**: `existingSecret: map-platform`, the bundled Postgres/MinIO/OpenBao all
`enabled: false`, all three Workload Identity annotations, the sandbox
`nodeSelector`/`tolerations` pair, `controlplane.service.type: LoadBalancer`, and modest
resource requests. It holds **no credential and no `image.tag`**. The credentials are not
withheld from it — in mode 2 the chart renders no Secret at all, so it takes no credential
values; and `image.tag` must come from the same commit SHA the build used, which is the whole
reason it is not a line in a file someone edits.

**It names no operator either, and that is the shape rather than a redaction.** The five
values in it that would — the image `registry`/`repository` split and the three
service-account emails — are written against `registry.invalid` and the neutral project
`your-project`, and the workflow overrides all five with `--set-string` from
`ARTIFACT_REGISTRY` and the three `*_SERVICE_ACCOUNT` variables. The keys stay in the file
rather than being deleted so that it remains a **worked example that renders on its own**:
`helm template -f staging-values.yaml` is what CI runs against it, and a file with
`image.registry` removed renders against the chart's `ghcr.io` defaults while one with the
annotations removed renders a ServiceAccount with no identity — an example documenting a
deployment that cannot start.

**The two halves do not fail the same way, and the comfortable claim that they do is the one
worth not making.** The image half fails closed **by construction**: `registry.invalid` can
never resolve, because RFC 2606 reserves the `.invalid` TLD so that no one can register a
name under it, so the pods cannot pull and `--wait --atomic` rolls the release back. That
host is also why the placeholder is not a plausible `us-central1-docker.pkg.dev/your-project/…`
— a GCP project id is a globally unique namespace anyone may register, so such a path would
be a *real* Artifact Registry path in whichever project owns that id, and a reader running
this file verbatim could silently pull a stranger's images rather than fail.

The identity half is different, and for the **brain** it does not fail closed at all. The
note on `brain.serviceAccount` in that same file says so — "omitting it does not fail; the
render succeeds, the pod starts, and the brain's reads degrade" — and
[`internal/brain/grader.go`](../../internal/brain/grader.go) implements it: a deliverable it
cannot read becomes a warning and the grade proceeds against the outcome description alone.
So a dropped brain override would deploy **green** and quietly grade worse. That is why the
workflow does not stop at setting the annotations: the step after `helm upgrade` reads all
three back out of the cluster and compares them against the variables they came from, so an
override that never reached the chart is a red run rather than a silent downgrade. What that
step cannot catch is a human swapping two of the variables in the settings page — the deploy
would then agree with its own inputs. Nothing here can, and that is the half of the cost of
moving values out of the diff that stays.

Rebuilding `environment/` does not change any of this **unless a coordinate changes**, and
the two places to check are no longer both files: the sandbox pair stays in
`staging-values.yaml`, while the image prefix, the three service-account emails,
`BLOB_BUCKET` and `KMS_KEY_NAME` are now repository variables. All of them are transcribed
from `environment/`, not generated from it, so diff them against `terraform output` after a
change to `main.tf` — `gh variable list` for the variables, the file for the rest. Get the
registry prefix wrong and the render still succeeds; it just names images that do not exist,
and all three pods sit in `ImagePullBackOff`.

**There is a third thing a rebuild moves, and it is not in either file: `database-url`.** A
recreated Cloud SQL instance gets a **new private IP**, and a DSN that hard-codes the old one
points the whole platform at an address nothing answers on — the pipeline applies the Secret,
and every pod fails its first query.

The way out of that is not to name an address at all, and `cloudSQLProxy.enabled` is now on
in [staging-values.yaml](./staging-values.yaml): each pod runs the Cloud SQL Auth Proxy as a
sidecar, the proxy is given the instance's **connection name** — which a rebuild does not
change — and the DSN names the proxy's loopback socket. `deploy.yml` resolves the connection
name from the Admin API at deploy time, so no operator's project is written down here.

**The DSN is the half that is not automatic, and it is deliberately not CD's to write** —
`database-url` is one of the three secrets a human creates out of band, for the reason given
under "The three secrets the pipeline reads". So the migration is two steps, in this order,
and the order is not optional: the proxy has to be listening before anything is told to use
it.

1. Deploy with the proxy on. It starts in all three pods and sits unused; the DSN still names
   the instance's address, and nothing about connectivity changes. **A proxy that starts is
   not a proxy that can reach the instance**: its two probes are `/startup` and `/liveness`,
   neither of which dials, and the chart passes no `--run-connection-test` — so `/startup`
   answers 200 once the listeners are up, whatever the connection name says. What step 1 does
   prove is that the right connection name reached the pods, because the deploy reads the
   rendered manifests back and refuses a release that does not name the instance it resolved.
   Reachability is first proven in step 2, by the pods themselves.
2. Point the DSN at it, then re-run the deploy so the pods restart onto the new Secret:

   ```sh
   pw="$(gcloud secrets versions access latest --secret=map-db-password \
           --project="$GCP_PROJECT_ID")"
   printf '%s' "postgres://map:$pw@127.0.0.1:5432/map?sslmode=disable" \
     | gcloud secrets versions add database-url \
         --project="$GCP_PROJECT_ID" --data-file=-
   ```

   `sslmode=disable` is safe **here and only here**: the hop it describes is a loopback socket
   inside one pod, and the proxy makes its own encrypted connection onward. That is also why
   the proxy is a sidecar and not a shared Deployment — the tidier shape would put the
   password and every result on the pod network in cleartext.

   **This re-run is the pipeline's one unprotected path.** A changed Secret takes the "Roll
   the pods onto the rotated Secret" step, which runs *after* the release is complete —
   `--atomic` cannot reach it, so a DSN the platform cannot use leaves the pods crash-looping
   and the step fails on `rollout status`. The old pod keeps serving while that happens (one
   replica, and a Deployment never tears the old one down for a new one that will not become
   ready), so this is a red pipeline rather than an outage.
   Rolling back means publishing the **previous DSN as a new version** — re-run the
   address-form command below. Do **not** reach for `gcloud secrets versions disable`:
   `latest` is an alias for the most recently *created* version, not the most recently
   enabled one, so disabling the bad version does not promote the good one back. It only
   makes `latest` unreadable, and the next deploy then dies fetching `database-url` — leaving
   you unable to deploy at all, mid-incident.

Until step 2 is done, a rebuild still moves `database-url`, and the old address form is what
to re-compose:

```sh
ip="$(terraform -chdir=deploy/gcp/environment output -raw sql_private_ip)"
pw="$(gcloud secrets versions access latest --secret=map-db-password \
        --project="$GCP_PROJECT_ID")"
printf '%s' "postgres://map:$pw@$ip:5432/map?sslmode=require" \
  | gcloud secrets versions add database-url \
      --project="$GCP_PROJECT_ID" --data-file=-
```

**The `map-platform` Secret is assembled by the pipeline**, because nothing else can: the
chart writes no Secret in this mode and Terraform holds no secret *values* by design. The
workflow reads `controlplane-api-key`, `database-url` and `model-providers` out of Secret
Manager into a mode-700 temp directory, writes the four non-secret literals
(`blob-backend=gcs`, `blob-bucket`, `secrets-backend=gcpkms`, `gcpkms-key-name`) beside them,
and applies all seven with `kubectl create secret generic … --from-file=… --dry-run=client
-o yaml | kubectl apply -f -`. `--from-file` and never `--from-literal`: a literal puts every
credential on the process's argv. Rotating `controlplane-api-key` or `model-providers` is
`gcloud secrets versions add` followed by a re-run of the workflow.

**That re-run has to roll the pods, and it will not do so by itself.** Env from a
`secretKeyRef` is read once, at process start; a `workflow_dispatch` after a rotation
deploys the *same* commit, so `helm upgrade` produces a byte-identical pod template and
Kubernetes correctly changes nothing. (The chart's `checksum/secret` annotation cannot help
here — with `existingSecret` no Secret is rendered, so it hashes an empty template on every
run, which is what the three deployment templates' comments say.) The pods would go on
serving the *archived* key, and `controlplane`'s rotation-by-restart semantics
(`EnsureAPIKey`, `internal/api/auth.go`) mean the new key is not even registered until one
restarts. So the workflow keeps the one word `kubectl apply` prints — created, configured or
unchanged — and rolls every Deployment in the release when, and only when, the Secret
actually changed. An ordinary push skips it: the seven values are identical and the rollout
`helm upgrade` did for the new `image.tag` is the only one.

`database-url` is the exception, because it is **derived** rather than primary: it is composed
once by hand from the secret the table above already calls the platform's password. So
rotating `map-db-password` is three steps, not one — add the version, re-run `make
gcp-db-init` so the `ALTER ROLE` lands, then re-compose `database-url` — and skipping the
third leaves a value in Secret Manager that the database no longer accepts, discovered at the
next pod restart.

**Compose it in whichever form is in force**, because the two-step migration under
"Continuous delivery" moves it once. Before the cutover it is
`postgres://map:<map-db-password>@10.136.0.3:5432/map?sslmode=require` — the pod reaches the
private IP directly, the same path `gcp-db-init` takes, and `require` encrypts without
verifying the server certificate. After it, the host is the proxy's loopback socket and
`sslmode` is `disable`; re-composing the address form then would quietly undo the cutover,
and nothing would complain, because the direct path still works.

The five **mode-1** secrets — `postgres-password`, `minio-root-user`, `minio-root-password`,
`openbao-seal-key`, `openbao-platform-token` — are no longer read by this deploy. They are
deliberately left in place rather than deleted: mode 1 is still the documented manual path
above, and a secret with a version is a secret that can still decrypt something.

**`model-providers` has a version, and that version is a placeholder.** It is a real endpoint
(`https://api.anthropic.com`) with a fake key, stored so the pipeline could be proven end to
end without inventing a credential. An unreachable host would have been a *different* failure
— the brain retrying a dead name — from the one this is honest about, which is an invalid
key: the platform comes up, `/v1/agents` answers, the deploy gate passes, and the first
session that calls a model fails with an auth error. Replace it in one line:

```sh
printf '%s' '[{"model":"*","protocol":"anthropic","base_url":"https://api.anthropic.com","api_key":"sk-ant-REAL"}]' \
  | gcloud secrets versions add model-providers --project="$GCP_PROJECT_ID" --data-file=-
```

The workflow still fails **by name** if that secret ever has no readable version — no
automation may mint a live model API key, so the run stops printing the command above rather
than installing a brain that crash-loops on an empty config.

**What the smoke step proves, and what it does not.** It waits for the LoadBalancer's
external IP, then requires `GET /v1/agents?limit=1` to answer 200 with the management key —
which exercises the key end to end and takes a round trip through Cloud SQL, so it is a real
check rather than a readiness probe restated — and to answer something *other* than 200
without it, which is the check that an address on the public internet is not simply open. It
does **not** run a session, so it says nothing about the model route (which is a placeholder
today), the sandbox pool or the egress gate. Those are the acceptance battery's job, not the
pipeline's.

**That address is plain HTTP on a bare IP**, and
[docs/deploy-gcp.md](../../docs/deploy-gcp.md#exposing-the-control-plane) tells you not to
do exactly this. It is a deliberate staging exception with a stated reason — there is no
domain yet, so there is no name for a certificate to be issued against and nothing for a
GKE Gateway to route — and a stated cost: the management key, every prompt and output, and
the whole SSE stream cross the internet in cleartext. The moment this environment gets a
domain, the fix is the Gateway in that section and `service.type` back to `ClusterIP`.

The **web console** does not use that address. It is deployed from its own repository into
this same cluster and this same namespace, and reaches the control plane over cluster DNS at
`http://map-managed-agent-platform-controlplane.map.svc.cluster.local:8080` — so the
management key never leaves the cluster, which is the one thing the cleartext external
address would otherwise put on the wire on every console page load. What the console *does*
publish is a second LoadBalancer of its own, for browsers, and its login gate
(`CONSOLE_PASSWORD`, the project's `console-password` secret) is mandatory precisely because
*that* address is public: an ungated console is an open door in front of a full-power
management key it holds server-side.

## Five settings that are not the provider's defaults

Four are staging choices and the fifth is not — it is correct everywhere, and is listed
here because it is equally easy to leave at the default and equally expensive to discover:

| Setting | Why | Why not in production |
| --- | --- | --- |
| `deletion_protection = false` on the GKE cluster | teardown must be one command | it exists to stop exactly this |
| `deletion_protection = false` **and** `settings.deletion_protection_enabled = false` on Cloud SQL | two separate flags, both of which block a destroy | ditto |
| `force_destroy = true` on the bucket | the acceptance battery leaves objects in it by construction | it deletes the objects |
| zonal cluster (`location = var.zone`) | on a *regional* cluster `node_count` is per zone across three zones, so the defaults would provision 12 nodes where the variables promise 4 | production wants the zone redundancy — and should then read `node_count` as per-zone |
| `connection_pool_config { connection_pooling_enabled = false }` | **not** staging-only — keep it off everywhere | transaction-mode pooling breaks a persistent `LISTEN`, the failure mode `internal/events/broker.go` names; SSE delivery depends on one pooled connection holding `LISTEN` for the life of the subscription |

## State, and recovering it

State is **local**, deliberately. A remote backend for `foundation/` would have to live in a
bucket, and a bucket is the kind of thing the foundation exists to own — the bootstrap is
circular, and the usual fix (a hand-made state bucket outside Terraform) reintroduces the
untracked resource this split exists to avoid.

**No secret is in either state file.** That is the point of `bootstrap.sh` and of
`password_wo`: the two database passwords exist in Secret Manager and nowhere else Terraform
can reach. Since #240 they are the only such values — object storage authenticates with
Workload Identity and has no credential to keep anywhere. (The model key is pasted from its own provider into the Helm values
in mode-1; in mode-2 it rides `model-providers.json` inside the pre-created Secret, so it
never reaches a values file either.) So a lost state file costs you bookkeeping, not
credentials.

Losing `foundation/`'s state is recoverable, because every resource in it is adoptable — but
adopt **every one of them**, not just the key ring. A partial import fails on the first
already-existing resource it did not import:

```sh
cd deploy/gcp/foundation
P=projects/your-project; L=us-central1; R=map-keyring
terraform import google_kms_key_ring.map                   "$P/locations/$L/keyRings/$R"
terraform import google_kms_crypto_key.cipher              "$P/locations/$L/keyRings/$R/cryptoKeys/map-credentials"
terraform import google_service_account.controlplane       "$P/serviceAccounts/map-controlplane@your-project.iam.gserviceaccount.com"
terraform import google_service_account.brain              "$P/serviceAccounts/map-brain@your-project.iam.gserviceaccount.com"
terraform import google_service_account.executor           "$P/serviceAccounts/map-executor@your-project.iam.gserviceaccount.com"
terraform import google_secret_manager_secret.db_password       "$P/secrets/map-db-password"
terraform import google_secret_manager_secret.db_admin_password "$P/secrets/map-db-admin-password"
for api in cloudbuild cloudkms iam iamcredentials secretmanager; do
  terraform import "google_project_service.required[\"$api.googleapis.com\"]" "your-project/$api.googleapis.com"
done
```

The same commands are how a project that **already** has a key ring adopts it on the first
apply, instead of colliding on the name. Nothing here imports a secret *version*, and nothing
needs to: `bootstrap.sh` is not Terraform-managed, and re-running it after an import is a
no-op because the versions already exist.

Do not trust that list to have kept pace with the configuration — a resource added to
`foundation/` after this was written would be missing from it, and the failure arrives as a
name collision partway through the recovery apply. `terraform plan` after the imports is the
authoritative check: every resource it still reports as *will be created* already exists in
the project and is one more `terraform import` away.

## Checking it without touching GCP

```sh
make gcp-fmt gcp-validate gcp-split-check gcp-lint \
     gcp-bootstrap-test gcp-split-check-test gcp-dbinit-test gcp-power-test
```

None of them needs credentials, state, or a project, and CI runs all eight on every PR — so
neither the configuration nor the tooling can rot silently between the rare runs that
actually provision anything. `gcp-dbinit-test` is the one with a host requirement: it needs
Docker, because it starts a real PostgreSQL.

`gcp-split-check` is the structural enforcement of the two-configuration split:
`environment/` may not *own* a resource of an unrecoverable kind, and every one
`foundation/` declares must carry both guards it can — `prevent_destroy` and, where the kind
has it, `deletion_policy = "PREVENT"`.

The last four **run** the tooling rather than reading it, because the first four are static
and this is a place where static checking has already been insufficient. `gcp-lint` is
shellcheck, and shellcheck cannot know that `gcloud secrets versions describe` rejects
`--filter` — it exited 0 on a `bootstrap.sh` that aborted on its first call in every project,
with the whole documented deploy path dead behind a green gate. So `gcp-bootstrap-test` runs
the script against a fake `gcloud` (including states no live run would reproduce on demand:
a write that commits and *then* reports failure, a create whose response never parses, a
rollback whose own calls fail), and `gcp-split-check-test` plants violations in a scratch
copy and requires each to come back red — a guard that had silently stopped reading its
input would pass a run against the real tree, and twice did.

`gcp-dbinit-test` is the third, and it exists because neither `terraform validate` nor
shellcheck can execute SQL: without it the only thing that ever runs `dbinit.sql` is a
billable cluster talking to a billable database, which is a slow and expensive way to find a
typo in a `\gexec`. It starts a real PostgreSQL 16 with TLS on and runs the file the way the
Job does — then sets up every state its assertions guard and the DDL does not repair, and
requires the run to go red for that reason, because an assertion that cannot fail is a
comment. That split matters: LOGIN, INHERIT, CREATEDB, CREATEROLE and database ownership are
*corrected* by the file, so their assertions cannot fire and the repair cases cover them
instead; SUPERUSER, BYPASSRLS and REPLICATION cannot be corrected by a Cloud SQL
administrator at all, so those are asserted-only and each has a negative case of its own. It also runs the
whole file under a deliberately **non-superuser** administrator, which is what Cloud SQL's
actually is: it holds CREATEDB and CREATEROLE — the two attributes that matter here — but
it is **not** a PostgreSQL SUPERUSER, and a local `postgres` is, so a local `postgres`
bypasses the very checks a Cloud SQL run has to satisfy.

Writing it paid for itself four times over. Two ordinary defects: an unguarded `pg_has_role`
that raised on any PostgreSQL without a `cloudsqlsuperuser` role, and a `\getenv` that turned
a missing password into a syntax error rather than the intended complaint. And two that would
have failed **only** on the real instance — `ALTER ROLE ... NOSUPERUSER` is refused outright
(PostgreSQL lets only a superuser change SUPERUSER, REPLICATION and BYPASSRLS, even to turn
them off), and `ALTER DATABASE ... OWNER TO` failed with `must be able to SET ROLE` because
the membership PostgreSQL 16's `CREATE ROLE` implicitly grants its creator does not carry the
SET option.

`gcp-power-test` is the fourth, and what it checks is a **sequence**, which is precisely what
shellcheck and a careful read both miss. Two of `env-power.sh`'s claims cost real money if
they quietly stop holding. `start` must restore the node counts `stop` saved rather than any
plausible constant — so its scenarios park pools of *different* sizes, which no single number
can satisfy. And both orderings have to survive refactoring: nodes drained before the
database stops, Cloud SQL `RUNNABLE` before any node returns.

The fake `gcloud` dies on any invocation it does not recognise, and checks each `describe`'s
`--format` as well, because a fake that answers the right question when the wrong one was
asked cannot catch the script asking it. Four real behaviours are modelled deliberately,
each one something the script would otherwise get wrong invisibly: `--update-labels`
**replaces** rather than merges; `--remove-labels` errors on a label that is not there; a
`value(resourceLabels.<key>)` projection on a missing key prints empty and exits 0 — which is
what lets `start` tell "no saved size" from "could not ask"; and gcloud on Windows terminates
its lines CRLF, which left a stray carriage return on every discovered pool name until the
script stripped it.

Eleven mutations were run against the suite, one per defect the reviews of this change found,
and every one turns it red: restoring a constant, inverting either ordering, skipping the
wait, sending only the script's own labels, clearing the marker from the pools this run
resized rather than from a fresh read, re-saving a resumed stop's zero, dropping the CR
strip, reading the pool list through a process substitution that cannot see its exit status,
swallowing the discovery error, accepting `NODES=0`, and resolving sizes pool-by-pool after
the mutations have already begun.
