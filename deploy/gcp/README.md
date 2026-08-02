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
| Holds | KMS key ring + crypto key, the three service accounts, the GCS HMAC key, the Secret Manager secrets | GKE cluster and node pools, Artifact Registry, Cloud SQL, the GCS bucket, all IAM bindings |
| Idle cost | cents a month | a running cluster and database |
| `terraform destroy` | not supported — resources carry `prevent_destroy` | `make gcp-env-destroy` |

Three things make the split necessary rather than tidy:

- **KMS key rings and crypto keys cannot be deleted.** `terraform destroy` removes them
  from state and leaves them in the project, so a single-configuration setup collides on
  the name at the next apply. For the crypto key the stakes are higher than a name
  collision: the vault ciphertext in Postgres is decryptable by that key and nothing else,
  so a teardown that scheduled its key versions for destruction would be silent,
  irreversible data loss discovered at the next credential read.
- **Deleting a service account deletes its HMAC keys.** An identity in the disposable half
  would strand the once-readable HMAC secret in Secret Manager — valid-looking, and dead.
- **The secrets are the reconciliation source.** A rebuilt environment is brought back into
  agreement with them: the database password is reapplied to the new Cloud SQL instance,
  and the HMAC pair is proven against the new bucket by an authenticated call.

`environment/` reads the foundation through `data` sources, by name, never by remote state.
So the two apply independently, and a destroy in the disposable half structurally cannot
reach anything in the durable one. CI enforces both halves of that: `environment/` may not
declare a `resource` of the three undeletable-or-load-bearing kinds, and every
`foundation/` resource whose loss is unrecoverable must carry `prevent_destroy`.

## What a destroy actually costs

`environment/` owns Cloud SQL, so **`make gcp-env-destroy` destroys the staging database**,
and vault ciphertext lives only in Postgres. Retaining the KMS key does not bring a deleted
row back. That is the right trade for a staging environment and this configuration makes it
deliberately — but it means a rebuild is proven with a *fresh* credential round-tripping on
the rebuilt stack, never with an old one surviving.

## Prerequisites

- A GCP project with billing enabled, and `gcloud auth application-default login`.
- Terraform ≥ 1.5 — `brew install hashicorp/tap/terraform`. It is not in Homebrew core.
- `kubectl` and `helm` for the deploy that follows.

## Running it

```sh
cat > deploy/gcp/foundation/terraform.tfvars <<'EOF'
project_id = "your-project"
EOF
cp deploy/gcp/foundation/terraform.tfvars deploy/gcp/environment/terraform.tfvars

make gcp-foundation-apply   # once, ever
make gcp-env-apply
```

`terraform.tfvars` is gitignored. `name_prefix`, `region` and `kms_location` must match
between the two configurations — `environment/` finds the foundation's resources by name,
so a mismatch surfaces as a missing data source rather than as a wrong-looking value.

Neither target passes `-auto-approve`. Read the plan before the first one.

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
  --region "$(terraform output -raw region)"

terraform output -raw helm_values_mode1 > values-gcp.yaml
```

That fragment carries exactly what mode-1 needs from Terraform — where the images live and
where sandboxes run — and nothing else. The bundled services still need their own
credentials, which the chart deliberately does not generate (a generated credential is
unstable under `helm template` and GitOps), so `openbao.staticSealKey`,
`openbao.platformToken`, `minio.*` and `controlplane.apiKey` are yours to supply alongside
it.

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

## Four settings that are not the provider's defaults

Three are staging choices and the fourth is not — it is correct everywhere, and is listed
here because it is equally easy to leave at the default and equally expensive to discover:

| Setting | Why | Why not in production |
| --- | --- | --- |
| `deletion_protection = false` on the GKE cluster | teardown must be one command | it exists to stop exactly this |
| `deletion_protection = false` **and** `settings.deletion_protection_enabled = false` on Cloud SQL | two separate flags, both of which block a destroy | ditto |
| `force_destroy = true` on the bucket | the acceptance battery leaves objects in it by construction | it deletes the objects |
| `connection_pool_config { connection_pooling_enabled = false }` | **not** staging-only — keep it off everywhere | transaction-mode pooling breaks a persistent `LISTEN`, the failure mode `internal/events/broker.go` names; SSE delivery depends on one pooled connection holding `LISTEN` for the life of the subscription |

## State, and recovering it

State is **local**, deliberately. A remote backend for `foundation/` would have to live in a
bucket, and a bucket is the kind of thing the foundation exists to own — the bootstrap is
circular, and the usual fix (a hand-made state bucket outside Terraform) reintroduces the
untracked resource this split exists to avoid.

Losing `foundation/`'s state does not lose the resources, because every resource in it is
adoptable:

```sh
cd deploy/gcp/foundation
terraform import google_kms_key_ring.map projects/P/locations/L/keyRings/map-keyring
terraform import google_kms_crypto_key.cipher projects/P/locations/L/keyRings/map-keyring/cryptoKeys/map-credentials
```

The same commands are how a project that **already** has a key ring adopts it on the first
apply, instead of colliding on the name.

One consequence of Terraform owning the HMAC key: **its secret is in the state file.** GCS
returns an HMAC secret exactly once, at creation, so any tool that creates one holds it.
Treat `foundation/terraform.tfstate` as a credential — it is gitignored, and Secret Manager
holds the authoritative copy the deployment actually reads.

## Checking it without touching GCP

```sh
make gcp-fmt gcp-validate
```

Neither needs credentials, state, or a project. CI runs both on every PR, so the
configuration cannot rot silently between the rare runs that actually provision anything.
