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

- **KMS key rings and crypto keys cannot be deleted — and destroying the *resource* is
  worse than leaving it.** The provider's default `deletion_policy` is `DELETE`, which
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
  agreement with them: the database password is reapplied to the new Cloud SQL instance,
  and the HMAC pair is carried forward untouched. (Decision 6 also wants that pair *proven*
  against the new bucket by an authenticated call rather than assumed valid from the presence of
  a secret version. `bootstrap.sh` does not do that yet — slice 4b's acceptance battery is where
  it belongs, because it needs a live bucket to authenticate against.)

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
half, an unterminated heredoc, unbalanced braces, or a multi-line `${...}` interpolation
(quote tracking is per line, so a string left open would desync the brace depth for the rest
of the file). Silently skipping any of those would print `ok` over a partial scan, which is
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
make gcp-env-apply
```

**The order is load-bearing.** `environment/` reads the database password ephemerally at
apply time; run it before `gcp-bootstrap` and it fails naming the secret that has no
version. `gcp-bootstrap` is idempotent by *skipping*: a secret that already has a version is
left exactly as it is, because that version may be the only thing that can decrypt
something. Re-running it after an `environment/` teardown is the normal case and is a no-op.

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

**Cloud Build's identity is a variable** (`cloud_build_service_account`) because the answer is
bimodal: older projects build as `PROJECT_NUMBER@cloudbuild.gserviceaccount.com`, newer ones as
the Compute Engine default service account. Empty means the legacy default. If the first push
fails on a permission, that is this — `gcloud builds describe` names the account your project
actually uses, and setting the variable grants it.

**Rotating the database password**: add a version by hand, then bump `db_password_version`
and re-apply `environment/`. Write-only arguments leave Terraform nothing to diff, so that
counter is the only signal that it should push the new value.

```sh
pw="$(openssl rand -hex 32)"
if [[ "$pw" =~ ^[0-9a-f]{64}$ ]]; then
  printf '%s' "$pw" | gcloud secrets versions add map-db-password \
    --project your-project --data-file=-
else
  echo "generator failed — nothing written" >&2
fi
unset pw
```

(An `if`, not `... || { …; exit 1; }`: this is meant to be pasted into a shell, where `exit`
would close the terminal and `return` is an error outside a function.)

Generate and **check** before writing, exactly as `bootstrap.sh` does, rather than piping
`openssl` straight into `gcloud`. In a pipeline a failure on the left does not stop the
right: `gcloud` starts anyway, reads EOF, and stores an *enabled* zero-byte version. Here
that version becomes `latest` and then rides `db_password_version` into Cloud SQL.

The command substitution also strips the newline `openssl` prints, which `--data-file=-`
would otherwise store as a 65th byte — and a raw control character in `DATABASE_URL` makes
the DSN unparseable, long after the apply that accepted it. Keep the value on **stdin** via a
shell builtin: passing it as an argument would put it in `ps` output and shell history.

## Handover to Helm

The acceptance runs in two phases the chart's own render-time exclusivity dictates
(plan 20, Decision 4). **Mode-1** is bundled Postgres/MinIO/OpenBao with inline values —
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

The sandbox-placement values in that fragment are a **pair, and neither half works alone**.
Without the tolerations every sandbox pod stays `Pending` forever — the pool's taint has no
other tolerator. Without the node selector sandbox pods land on the platform pool, and the
dedicated pool buys nothing.

Mode-2's inputs are emitted individually, for slice 5 to assemble into the pre-created
Secret:

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
in mode-1; mode-2 gives it a secret of its own, in slice 5.) So a lost state file costs you bookkeeping, not
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
terraform import google_secret_manager_secret.db_password      "$P/secrets/map-db-password"
terraform import google_secret_manager_secret.blob_access_key  "$P/secrets/map-blob-access-key"
terraform import google_secret_manager_secret.blob_secret_key  "$P/secrets/map-blob-secret-key"
for api in cloudkms iam iamcredentials secretmanager; do
  terraform import "google_project_service.required[\"$api.googleapis.com\"]" "your-project/$api.googleapis.com"
done
```

The same commands are how a project that **already** has a key ring adopts it on the first
apply, instead of colliding on the name. Nothing here imports a secret *version* or the HMAC
key, and nothing needs to: `bootstrap.sh` is not Terraform-managed, and re-running it after
an import is a no-op because the versions already exist.

## Checking it without touching GCP

```sh
make gcp-fmt gcp-validate gcp-split-check gcp-lint gcp-bootstrap-test gcp-split-check-test
```

None of them needs credentials, state, or a project, and CI runs all six on every PR — so
neither the configuration nor the tooling can rot silently between the rare runs that
actually provision anything.

`gcp-split-check` is the structural enforcement of the two-configuration split:
`environment/` may not *own* a resource of an unrecoverable kind, and every one
`foundation/` declares must carry both guards it can — `prevent_destroy` and, where the kind
has it, `deletion_policy = "PREVENT"`.

The last two **run** the tooling rather than reading it, because the first four are static
and this is a place where static checking has already been insufficient. `gcp-lint` is
shellcheck, and shellcheck cannot know that `gcloud secrets versions describe` rejects
`--filter` — it exited 0 on a `bootstrap.sh` that aborted on its first call in every project,
with the whole documented deploy path dead behind a green gate. So `gcp-bootstrap-test` runs
the script against a fake `gcloud` (including states no live run would reproduce on demand:
a write that commits and *then* reports failure, a create whose response never parses, a
rollback whose own calls fail), and `gcp-split-check-test` plants violations in a scratch
copy and requires each to come back red — a guard that had silently stopped reading its
input would pass a run against the real tree, and twice did.
