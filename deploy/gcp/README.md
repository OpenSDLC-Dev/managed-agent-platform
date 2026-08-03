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

Three things make the split necessary rather than tidy:

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
- **Deleting a service account deletes its HMAC keys.** An identity in the disposable half
  would strand the once-readable HMAC secret in Secret Manager — valid-looking, and dead.
  (The HMAC *key* is not a Terraform resource at all; `bootstrap.sh` creates it. GCS returns
  its secret exactly once, so a resource holding it would hold it in state.)
- **The secrets are the reconciliation source.** A rebuilt environment is brought back into
  agreement with them, and by two different mechanisms now that there are two database
  credentials: the **administrator's** password is reapplied to the new Cloud SQL instance by
  the apply itself, through `password_wo`; the **platform's** is reapplied by re-running
  `make gcp-db-init`, which is the same run that re-creates its role. The HMAC pair is
  carried forward untouched. (Decision 6 also wants that pair *proven*
  against the new bucket by an authenticated call rather than assumed valid from the presence of
  a secret version. `bootstrap.sh` does not do that; slice 4b's acceptance battery did,
  because it needs a live bucket to authenticate against — PUT, GET and DELETE in the
  rebuilt bucket with the surviving pair, recorded in docs/HISTORY.md.)

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

## Prerequisites

- A GCP project with billing enabled, and `gcloud auth application-default login`.
- Terraform ≥ 1.11 — `brew install hashicorp/tap/terraform`. It is not in Homebrew core.
  (1.11 for `password_wo`; `foundation/` alone needs only 1.5.)
- `python3` — `bootstrap.sh` parses one JSON response with it.
- `kubectl` and `helm` for the deploy that follows.

## Running it

```sh
cat > deploy/gcp/foundation/terraform.tfvars <<'EOF'
project_id = "your-project"
EOF
cp deploy/gcp/foundation/terraform.tfvars deploy/gcp/environment/terraform.tfvars

make gcp-foundation-apply              # once, ever — creates the secrets EMPTY
PROJECT=your-project make gcp-bootstrap  # fills them, creates the GCS HMAC key

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
a `.tfvars` file: it derives every secret id and the storage service-account email from the
`NAME_PREFIX` environment variable, which defaults to `map`. If you set `name_prefix` in
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
| `image.tag` | the tag `cloudbuild.yaml` pushed. Empty falls back to the chart's `appVersion` (`0.1.0`), which nothing publishes, so all three platform pods sit in `ImagePullBackOff` |
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

The sandbox-placement values in that fragment are a **pair, and neither half works alone**.
Without the tolerations every sandbox pod stays `Pending` forever — the pool's taint has no
other tolerator. Without the node selector sandbox pods land on the platform pool, and the
dedicated pool buys nothing.

Mode-2's inputs are emitted individually, to be assembled into the pre-created Secret
`existingSecret` names — its full key list is in
[docs/deploy-gcp.md](../../docs/deploy-gcp.md#the-two-modes), and there is no script for it
because a script that touched every one of these would be a credential-handling tool of its
own. The mode-2 acceptance run built it with a single `kubectl create secret generic`:

```sh
terraform output -raw  kms_key_name                              # gcpKMS.keyName / GCPKMS_KEY_NAME
terraform output -json controlplane_service_account_annotation   # controlplane.serviceAccount.annotations
terraform output -json executor_service_account_annotation       # executor.serviceAccount.annotations
terraform output -raw  blob_endpoint                             # BLOB_S3_ENDPOINT
terraform output -raw  blob_bucket                               # BLOB_S3_BUCKET
terraform output -raw  sql_instance_connection_name              # Cloud SQL Auth Proxy argument
```

Workload Identity needs both halves and this configuration only applies one of them: the
IAM binding permitting a Kubernetes ServiceAccount to impersonate the Google service
account is created here; the **annotation** on that KSA comes from the chart values above.
`namespace` and `release_name` are what the binding names — install the chart somewhere
else and ADC fails at pod startup rather than denying a permission later.

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
`password_wo`: the database password and the GCS HMAC pair exist in Secret Manager and nowhere
else Terraform can reach. (The model key is pasted from its own provider into the Helm values
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
terraform import google_service_account.executor           "$P/serviceAccounts/map-executor@your-project.iam.gserviceaccount.com"
terraform import google_service_account.storage            "$P/serviceAccounts/map-storage@your-project.iam.gserviceaccount.com"
terraform import google_secret_manager_secret.db_password       "$P/secrets/map-db-password"
terraform import google_secret_manager_secret.db_admin_password "$P/secrets/map-db-admin-password"
terraform import google_secret_manager_secret.blob_access_key   "$P/secrets/map-blob-access-key"
terraform import google_secret_manager_secret.blob_secret_key   "$P/secrets/map-blob-secret-key"
for api in cloudbuild cloudkms iam iamcredentials secretmanager; do
  terraform import "google_project_service.required[\"$api.googleapis.com\"]" "your-project/$api.googleapis.com"
done
```

The same commands are how a project that **already** has a key ring adopts it on the first
apply, instead of colliding on the name. Nothing here imports a secret *version* or the HMAC
key, and nothing needs to: `bootstrap.sh` is not Terraform-managed, and re-running it after
an import is a no-op because the versions already exist.

Do not trust that list to have kept pace with the configuration — a resource added to
`foundation/` after this was written would be missing from it, and the failure arrives as a
name collision partway through the recovery apply. `terraform plan` after the imports is the
authoritative check: every resource it still reports as *will be created* already exists in
the project and is one more `terraform import` away.

## Checking it without touching GCP

```sh
make gcp-fmt gcp-validate gcp-split-check gcp-lint \
     gcp-bootstrap-test gcp-split-check-test gcp-dbinit-test
```

None of them needs credentials, state, or a project, and CI runs all seven on every PR — so
neither the configuration nor the tooling can rot silently between the rare runs that
actually provision anything. `gcp-dbinit-test` is the one with a host requirement: it needs
Docker, because it starts a real PostgreSQL.

`gcp-split-check` is the structural enforcement of the two-configuration split:
`environment/` may not *own* a resource of an unrecoverable kind, and every one
`foundation/` declares must carry both guards it can — `prevent_destroy` and, where the kind
has it, `deletion_policy = "PREVENT"`.

The last three **run** the tooling rather than reading it, because the first four are static
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
