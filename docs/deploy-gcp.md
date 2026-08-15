# Deploying on Google Cloud

How to run this platform on GKE with Cloud SQL, Cloud Storage and Cloud KMS — what the
Terraform in [`deploy/gcp/`](../deploy/gcp/) builds, what it deliberately does not, and the
settings that are **required** rather than recommended.

Two documents sit next to this one and are not repeated here.
[`deploy/gcp/README.md`](../deploy/gcp/README.md) is the operating manual for the Terraform
itself — the foundation/environment split, the exact run order, state recovery, and the
credential-free checks. [`docs/plan/20_gcp-deployment.md`](./plan/20_gcp-deployment.md) is
the design record and carries the reasoning behind every decision referenced below.

## What this is, and what it is not

The configuration in `deploy/gcp/` builds a **staging-grade** deployment. "Production
shape" throughout this guide means the shape of the topology — private nodes, a private
database, a non-superuser database role — never a readiness verdict.

Three prerequisites stand between this and a production deployment, and none of them is in
the Terraform:

1. **Transport security.** `controlplane` serves plain HTTP and terminates no TLS of its
   own. Nothing here exposes it publicly, and you must not either until you have read
   "Exposing the control plane" below.
2. **Host-level isolation of untrusted code.** Sandboxes run on GKE Standard's default
   runtime. gVisor and the current in-pod egress gate are mutually exclusive — see "gVisor"
   below for the exact boundary.
3. **A backup and restore procedure you have rehearsed.** See "Backups, and two lifecycles
   that lose data".

## The two modes

**Mode 1** runs the chart's bundled Postgres, MinIO and OpenBao as StatefulSets in the
cluster. It is the fastest way to see the platform work and is what
`terraform output -raw helm_values_mode1` produces. It is not what to run in production, and
its PersistentVolumes are the thing the teardown section warns about.

**Mode 2** points the platform at Cloud SQL, Cloud Storage and Cloud KMS. Everything below
assumes mode 2 unless it says otherwise. The switch is `existingSecret`: set it and the
chart creates no Secret of its own and reads every credential from the one you pre-created,
which must carry `controlplane-api-key`, `model-providers.json`, `database-url`,
`blob-backend` (`gcs`) plus `blob-bucket`, and `secrets-backend` plus `gcpkms-key-name`.
Object storage needs no credential key of its own since #240: it authenticates as the
workload through Workload Identity. `existingSecret` is
incompatible with `postgresql.enabled`, `minio.enabled` and `openbao.enabled` — the chart
fails the render rather than deploying two of anything.

Both modes have now been stood up and exercised end to end on GKE; the two acceptance
records are in [docs/HISTORY.md](./HISTORY.md). Where an acceptance measurement appears in
this guide it comes from one of those runs. Not every number here is one — configured
values, published prices and documented platform limits appear too, and none of those is
run evidence. The sizing figures below are still the mode-1 ones: mode 2 changes what backs
the platform, not what a sandbox pod costs to schedule.

## Building it

The run order is in [`deploy/gcp/README.md`](../deploy/gcp/README.md#running-it) and is
load-bearing; this is only the shape of it:

```sh
make gcp-foundation-apply    # never destroyed — durable identities, KMS, empty secrets
                             # (re-apply it whenever foundation/ gains a resource)
make gcp-bootstrap           # fills the secrets
make gcp-env-apply           # network, cluster, Cloud SQL, bucket, registry
make gcp-db-init             # the platform's database role — see below
gcloud builds submit ...     # the component images
helm install ...             # the platform
```

The **last two lines are the only ones above that this project automates**.
`.github/workflows/deploy.yml` runs them against its own staging environment — in **mode 2**
— on every push to `main` and on `workflow_dispatch`, reading
[`deploy/gcp/staging-values.yaml`](../deploy/gcp/staging-values.yaml), assembling the
pre-created Secret from Secret Manager, rolling the pods when that Secret changed, ending
with an authenticated smoke check against the LoadBalancer (200 with the management key, 401
without), and authenticating throughout by Workload Identity Federation rather than by any
stored GitHub secret. Dispatch is for what a push cannot express: redeploying an unchanged
commit onto a rebuilt cluster, or finishing a rotation a human made in Secret Manager.

**All four `make` targets above it stay human-driven, and all four are prerequisites of the
pipeline rather than steps in it** — including `gcp-db-init`, which mode 2 genuinely depends
on: the DSN in the pre-created Secret names the `map` role, and `dbinit.sql` is the only thing
that creates it. Terraform stays out of CD because its applies are interactive by design and
its state is local (plan 20, Decision 9), which leaves the pipeline owning exactly
build → push → deploy → smoke. The reasoning, the two out-of-band IAM grants it needs, and
what an operator still owes a rebuilt environment are in
[`deploy/gcp/README.md`](../deploy/gcp/README.md#continuous-delivery). Nothing about that
workflow is required to deploy this platform; it is how *this* repository runs *its* staging
environment, and the manual path above remains the supported one.

`gcloud builds submit` has one requirement worth knowing before the first run on a fresh
project: the images build under **BuildKit** (`env: ["DOCKER_BUILDKIT=1"]` in
`cloudbuild.yaml`), because the Dockerfile's `FROM --platform=$BUILDPLATFORM` uses variables
the classic builder does not define — without it the build fails with
`"" is an invalid component of ""` rather than with anything about platforms.

`foundation/` is never destroyed; `environment/` is created and destroyed freely. Never
destroyed is not the same as applied once: the configuration is idempotent, and re-applying
it is how anything is ever *added* to it. That matters for an existing
deployment upgrading past
[#269](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/269), which adds the
brain's Google service account — `environment/` reads it by name through a data source, so
running `make gcp-env-apply` without re-applying the foundation first fails on the lookup
rather than creating it. That split is what makes a rebuild safe, and the reason is not tidiness: a KMS key
ring cannot be deleted in GCP at all, and destroying the key schedules every version for
destruction while the name stays taken — so a configuration that owned it could not be
re-applied, and the vault ciphertext encrypted under it would be gone.

## The database role is not a Cloud SQL superuser

Cloud SQL grants `cloudsqlsuperuser` — which carries `CREATEDB` and `CREATEROLE` — to every
built-in user created through its Admin API. That is the path `gcloud sql users create` and
Terraform's `google_sql_user` both take, so the obvious way to create the platform's
credential hands it those privileges. What the platform actually needs is narrower:
`store.Open` migrates only the database its DSN names, and all migrations are ordinary
schema DDL that creates no role and no database.

Google documents one escape: name one or more **custom database roles** at creation
(`--database-roles` / `databaseRoles` / `database_roles`) and the `cloudsqlsuperuser` grant
is suppressed. The catch is that a custom role is an ordinary PostgreSQL role that must
already exist, and creating one takes a SQL session against an instance that does not exist
until Terraform has run — a circle that cannot be closed inside one apply.

So `deploy/gcp/` splits by owner. Terraform creates exactly one built-in user, the
administrator `postgres`, whose password lives in the `<prefix>-db-admin-password` secret
and which the platform never reads. `make gcp-db-init` then runs
[`deploy/gcp/dbinit.sql`](../deploy/gcp/dbinit.sql), which creates the custom role and the
platform's own role under it with plain `CREATE ROLE` — a path that never goes through the
Admin API's user creation, so there is no grant to suppress.

It runs as a **Kubernetes Job**, not as a local `psql`, because the instance has no public
address: it is reachable from inside the VPC and from nowhere else, including not from the
machine running Terraform.

The file **asserts** the result rather than the reasoning, and a failed assertion fails the
step:

| Asserted | Why this one |
| --- | --- |
| not `SUPERUSER` | the floor |
| no `CREATEDB`, no `CREATEROLE`, no `BYPASSRLS` | role attributes |
| no `REPLICATION` | a replication connection streams the WAL, which carries every database on the instance — so it reads around the ownership containment the rows below establish |
| `pg_has_role(user, 'cloudsqlsuperuser', 'member')` is false | **the one that settles it** — the attributes above are attributes and say nothing about membership, so a role with every flag false is still a superuser in effect if it is a member of one |
| owns the platform database | the migrations need it |
| owns no other database | scope |
| `pg_stat_ssl` reports the session encrypted | the property, not the request — `sslmode` states what the client asked for |

Re-run `make gcp-db-init` after every `environment/` rebuild. It is also the whole password
rotation procedure: rotate the secret, re-run, and the unconditional `ALTER ROLE` pushes the
new value.

One consequence of the administrator not being a real superuser is worth knowing before you
read the SQL and wonder about an omission. PostgreSQL lets only a `SUPERUSER` change the
`SUPERUSER`, `REPLICATION` and `BYPASSRLS` attributes — **even to turn them off** — and
the administrator is not one. So of the attributes it could revoke the corrective
`ALTER ROLE` names only `NOCREATEDB NOCREATEROLE` (alongside the `LOGIN INHERIT` it
restores and the password it sets), and none of those three: they are asserted but not
repaired, because this session genuinely cannot revoke them. A drift in them fails the step with a message rather than being silently
fixed. For the same reason the file grants the platform role to the administrator before
transferring database ownership — `ALTER DATABASE ... OWNER TO` requires the caller to be
able to `SET ROLE` to the new owner, and in PostgreSQL 16 the membership `CREATE ROLE`
implicitly gives its creator does not carry that. Both of these were found by running the
file under a non-superuser administrator locally; a superuser executes them without
complaint, which is exactly why the test does not use one.

### Connecting to a private-IP instance

The instance has `ipv4_enabled = false`. Google's documented path from GKE is the **Cloud
SQL Auth Proxy with `--private-ip`**, which terminates the encrypted leg itself and offers
the application a plain loopback socket — so an application behind the proxy uses
`sslmode=disable` in its DSN while the connection the server sees is TLS. That is not a
downgrade, and `ssl_mode = ENCRYPTED_ONLY` on the instance is what makes it checkable: the
server rejects an unencrypted connection whatever the client asked for.

**Run the proxy as a sidecar, in the same pod.** The `sslmode=disable` above is only safe
because the hop it describes is a loopback socket inside one pod. Run the proxy as a shared
Deployment and Service instead — which looks tidier and is the obvious workaround — and that
same `sslmode=disable` now sends the password, every query and every result across the pod
network in cleartext, on the near side of the encryption. `ssl_mode = ENCRYPTED_ONLY` and
`pg_stat_ssl` cannot see that hop at all: they describe the proxy's connection to Cloud SQL,
not your application's connection to the proxy. Google recommends colocation for exactly
this reason.

The chart templates it, off by default. `cloudSQLProxy.enabled` adds the sidecar to all
three deployments at once — one switch rather than three, because they read a single
`database-url` Secret key and a DSN cannot name a loopback socket for some pods and the
instance's address for the others:

```sh
--set cloudSQLProxy.enabled=true \
--set "cloudSQLProxy.instanceConnectionName=$(terraform output -raw sql_instance_connection_name)"
```

and then a `database-url` of
`postgres://USER:PASSWORD@127.0.0.1:5432/DATABASE?sslmode=disable`. **Pointing the DSN at
the proxy is yours to do**, and it is the mistake to watch for: a sidecar with a DSN still
naming the instance's own address is a proxy nothing connects through. In mode 2 the chart
renders no Secret at all and never sees the DSN, so it cannot check this — and it does not
check the one path where it *could* (`externalDatabase.url`) because there is nothing exact
to check against: a DSN through the sidecar can legitimately say `127.0.0.1`, `localhost`,
`[::1]`, or a unix socket path, so a host check would refuse working deployments.

It renders as a **native sidecar** (an `initContainer` with
`restartPolicy: Always`) with a startup probe on the proxy's own `/startup`, which is what
makes it ready before the process that dials it: all three open the database as they start,
so an ordinary container would let them race it. Four things fail the render rather than
deploying a pod that never becomes ready: a cluster older than Kubernetes 1.29 (which
predates native sidecars), a missing `instanceConnectionName`, one whose *shape* is wrong,
and the bundled Postgres left enabled alongside it.

The shape check is exactly this: **three or four non-empty colon-separated segments** —
`PROJECT:REGION:INSTANCE`, or `DOMAIN:PROJECT:REGION:INSTANCE` for a legacy domain-scoped
project. That refuses an address such as `10.1.2.3` or `db.internal:5432`, and a name with
an empty part. It does not look inside the segments, so a well-formed name that names
nothing renders and the proxy rejects it at startup — deliberately, because encoding
Google's own naming rules here would refuse a valid name the day Google widens one.

If you must use a shared proxy Service as an interim step, treat the
database credential as exposed to anything that can watch pod-network traffic, and say so in
your threat model rather than inheriting the `sslmode=disable` from this page as though it
were still local.

**There is a second, simpler path, and it is the one this repo already exercises.** A Pod in
this cluster can reach the private-IP instance *directly* — `make gcp-db-init` does exactly
that, with `PGSSLMODE=require`, no sidecar and no Google identity at all. So a `database-url`
of `postgres://USER:PW@$(terraform output -raw sql_private_ip):5432/DB?sslmode=require` works
for all three components with no IAM whatsoever. The trade is real and worth naming: `require`
encrypts but does not verify the server's certificate, which is what the proxy adds along with
IAM-based authorization. Prefer the proxy where you can; take this path when you want to skip
the proxy's own setup. It does **not** skip per-component identity — see below.

**All three annotations are required in mode 2, whichever database path you take**, and that
is a change #240 made:

```sh
terraform output -json controlplane_service_account_annotation
terraform output -json brain_service_account_annotation
terraform output -json executor_service_account_annotation
```

The controlplane and executor always needed theirs for the Cloud KMS cipher. The **brain's**
used to be conditional — it existed for the Cloud SQL Auth Proxy, and the direct path needs
no Google identity for the database — but object storage is now reached as the workload
itself, and the brain reads the bucket (rubric snapshots and deliverables), so its account
holds `roles/storage.objectViewer` there. Its own account rather than the control plane's
remains the point: annotating the brain onto that account would be the shortcut, and it would
hand the brain the KMS decrypt that account carries.

Miss the brain's annotation and nothing fails loudly. Its blob reads are written to degrade —
a deliverable that cannot be read is listed but not inlined, and an unreadable rubric snapshot
grades against the outcome description alone — so the symptom is worse grading, not an
outage. The mode-2 acceptance run predates all of this: it took the direct path and
[#269](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/269), which gave the
brain a ServiceAccount at all, was a follow-on rather than a blocker at the time.

Neither half of the proxy path has been exercised on a live GKE cluster. The chart renders
it, a real API server accepts the manifests, and the Terraform passes the credential-free
checks — but the mode-2 acceptance run predates all of it and took the direct path, so what
is established here is that the configuration is well-formed, not that a pod has connected
through it.

## Exposing the control plane

**Do not put the control-plane Service behind a public load balancer without terminating
TLS in front of it.** `controlplane` is an `http.Server` with no TLS configuration. An
external load balancer that terminates nothing carries the global `x-api-key`, every
session's prompts and outputs, and the entire SSE event stream in cleartext across whatever
network the balancer sits on. This is a prerequisite for any exposed deployment, not an
optional hardening step.

Both acceptance runs behind this guide used `kubectl port-forward` and exposed nothing.

**This project's own staging environment now breaks that rule on purpose, and the exception
is recorded here rather than only in the pull request that made it.**
[`deploy/gcp/staging-values.yaml`](../deploy/gcp/staging-values.yaml) sets
`controlplane.service.type: LoadBalancer`, so the CD pipeline's deployment answers on a bare
external IP over plain HTTP. The reason is that there is no domain yet: with no name, there
is nothing for a certificate to be issued against and nothing for the Gateway below to route,
so the alternative was not "HTTPS instead" but "unreachable". The cost is exactly the one
stated above, unmitigated — the global `x-api-key`, every prompt and output, and the entire
SSE stream in cleartext across the internet — and it is accepted only because that
environment is staging and holds nothing anyone would miss. Do not read it as a pattern; the
moment a domain exists the fix is the Gateway below and `service.type` back to `ClusterIP`.

When you do expose it, a GKE Gateway with a Google-managed certificate is the least
surprising way:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: map
spec:
  gatewayClassName: gke-l7-global-external-managed
  listeners:
    - name: https
      protocol: HTTPS
      port: 443
      hostname: map.example.com
      tls:
        mode: Terminate
        certificateRefs:
          # A kubernetes.io/tls Secret. For a Google-MANAGED certificate
          # instead, drop this whole `certificateRefs` block and annotate the
          # Gateway with `networking.gke.io/certmap: <your-certificate-map>`,
          # having created the map in Certificate Manager. Do not reach for the
          # `ManagedCertificate` CRD here — that one belongs to Ingress, not to
          # Gateway.
          - name: map-tls
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: map
spec:
  parentRefs:
    - name: map
  hostnames:
    - map.example.com
  rules:
    - backendRefs:
        - name: map-managed-agent-platform-controlplane
          port: 8080
```

Terminating TLS at the Gateway leaves the hop from the Gateway to the pod in cleartext
inside the VPC. Encrypting *that hop* is a `BackendTLSPolicy` and an HTTPS backend, which
`controlplane` cannot serve — it is a plain `http.Server` with no TLS config — so the
honest options today are to accept the in-VPC cleartext or to not expose the service at
all, reaching it over a VPN or `kubectl port-forward`.

Identity-Aware Proxy is **not** an answer to that hop, and is listed here only because it
is the obvious thing to reach for: IAP is an access-control policy on the Gateway's
backend service, so it changes who may send a request, not how the request travels. Adding
it does put a second authentication layer in front of the platform's own `x-api-key` — and
note that it consumes the `Authorization` header, which `internal/api/envauth.go` uses for
worker authentication, so a BYOC worker behind IAP needs its credential moved to
`Proxy-Authorization`. With a bare Gateway and no IAP, `x-api-key` is the only
authentication there is: the Gateway adds none.

## Observability

The chart has an `otlp.endpoint` and nothing to point it at; it ships no collector. Deploy
an OpenTelemetry Collector with the `googlecloud` exporter and set `otlp.endpoint` to it.

**Declare all three pipelines, not just traces.** `internal/telemetry/telemetry.go` builds
an `otlptracegrpc`, an `otlpmetricgrpc` **and** an `otlploggrpc` exporter against that one
endpoint, so a collector offering only a `traces` pipeline answers the other two with gRPC
`Unimplemented` and every process prints

```
rpc error: code = Unimplemented desc = unknown service opentelemetry.proto.collector.logs.v1.LogsService
```

on a loop. Adding `logs` and `metrics` pipelines silences it.

**Then the logs pipeline needs a log name.** The `googlecloud` exporter refuses the logs
signal — `no log name provided` — until `log.default_log_name` is set, and drops the batch
rather than failing loudly at startup. Traces need no equivalent setting, so this is the
second of two traps and only becomes visible once the first is fixed:

```yaml
exporters:
  googlecloud:
    project: YOUR_PROJECT
    log:
      default_log_name: managed-agent-platform
```

Note that the platform logs sparsely — startup and errors, not a line per request — and on
GKE the containers' stdout already reaches Cloud Logging through the node's own logging
agent. So the OTLP logs path is a second route to the same place, and a quiet one: the
mode-2 acceptance run saw no platform log record travel it after startup. Do not read
silence there as breakage.

Two more things cost real time to discover, and both are properties of GKE rather
than of the platform:

- **On a Workload Identity cluster, the node service account's project roles do not reach
  pods.** GKE stops serving the node identity through the metadata server, so the collector
  needs its own Kubernetes ServiceAccount, annotated with
  `iam.gke.io/gcp-service-account`, bound to a Google service account that holds
  `roles/cloudtrace.agent`. Granting the node account `roles/editor` changes nothing. This
  applies to anything you add to the cluster, not only the collector.
- **Cloud Trace's v1 `traces.list` shards its results.** The first page can come back empty
  with a `nextPageToken`; following it produced 22 traces on a run that had just been told
  it had none. An empty first page is not an empty result — follow the token before
  concluding that tracing is broken.

## Networking

### NetworkPolicy and node-local DNS

If you write a NetworkPolicy for sandbox pods, the usual "allow egress to the kube-dns
pods" carve-out — a `podSelector` matching kube-dns in `kube-system` — **does not match** on
GKE. GKE enables node-local DNS alongside FQDN network policy, and the node intercepts the
kube-dns VIP, so the traffic never reaches a pod the selector could match. Write DNS
carve-outs in the port-53 / `ipBlock` form instead.

### The sandbox needs no external DNS

Where the egress gate is in use, the sandbox resolves nothing: an HTTPS `CONNECT` tunnel
hands the hostname to the gate, and the gate resolves it. That is a property of the
topology and worth knowing before adding DNS rules the sandbox does not need.

## Sizing the sandbox node pool

Measured on two `e2-standard-4` nodes with the shipped defaults:

- **68 sandbox pods** were scheduled before the pool returned `Unschedulable — Insufficient
  cpu`. The binding constraint is **CPU requests, not the 110-pod-per-node cap**.
- A session that wrote a small Python project and ran its tests used **0.43 MiB** of
  ephemeral storage; trivial sessions used 0.04–0.09 MiB. At those rates the 100 GiB boot
  disk is dominated by the image, not by workspaces.

The number that matters is 68, and how long each pod holds its 100m of it is the
reaper's answer (`internal/executor/reaper.go`, plan 24). A **cloud** session's sandbox is
destroyed once the session is deleted, archived or terminated, and — where the idle tier is
armed — after `EXECUTOR_SANDBOX_IDLE_TTL` of inactivity (default 24h; `0` disables the
tier). Size for **concurrent** sessions plus whatever has not yet aged out.

Two configurations keep the cumulative arithmetic instead, and both are worth checking
before sizing. The idle tier needs an object store to hold the checkpoint it takes before
destroying a sandbox, so an executor with no blob backend runs with that tier off — it logs
the disablement once at startup — and idle sessions then hold their pods until they reach a
terminal state. And a `self_hosted` session's sandbox belongs to the customer's BYOC worker,
which the platform does not destroy in any tier.

## Required node settings

**`podPidsLimit` is required, not optional.** `Hardening.PidsLimit` is Docker-only: the Pod
API carries no per-pod process limit, so the Kubernetes sandbox provider ignores a
configured value and logs that it did (recorded in
[docs/DIVERGENCES.md](./DIVERGENCES.md)). On GKE the equivalent control is the kubelet's
`podPidsLimit`, which is **node configuration** — so it belongs to the node pool. Without
it, a fork bomb in a sandbox is bounded by nothing the platform sets.

`deploy/gcp/environment/` sets it on the sandbox pool through
`var.sandbox_pod_pids_limit` (default 4096; GKE accepts 1024–4194304). If you build node
pools by hand, set it yourself.

What the sandbox provider *does* apply by default: a CPU limit, the
`NET_RAW`/`SETUID`/`SETGID` capability drops with privilege escalation forbidden alongside
them, and — on Kubernetes — a pod-level `seccompProfile: RuntimeDefault`, set
unconditionally and deliberately not configurable. Note that the escalation guard rides on
the drop set: `SANDBOX_CAP_DROP=none` removes it too, while the seccomp profile stands on
its own and is unaffected. A memory cap, a read-only rootfs and a non-root uid are opt-in.

Two consequences of the seccomp default worth knowing before you meet them: on a cluster
whose kubelet already runs with `--seccomp-default` it changes nothing, and an image that
needs a syscall the runtime's curated filter blocks has no exception to ask for.

## gVisor

The chart can place sandbox pods on a hardened runtime: `sandboxRuntimeClass.name` sets
`runtimeClassName` on every sandbox pod. Before you set it, know the boundary.

**gVisor and the in-pod egress gate are mutually exclusive.** Under GKE Sandbox the gate
cannot program iptables at all — the nft backend fails with `Protocol not supported` and
the legacy backend with `can't initialize iptables table 'filter'`. The gate **fails
closed**: `gaterun.Setup` errors and the process exits before anything listens, so the
HEALTHCHECK that admits the sandbox never passes.

The consequence, stated precisely because the imprecise version is alarming and wrong:
**gated sessions on gVisor are unusable, not unprotected.** A session with a `limited`
network policy or an attached vault will not start. Sessions that are not gated run on
gVisor normally. So `sandboxRuntimeClass.name: gvisor` is safe to set if you do not use
gated sessions, and breaks them if you do.

NetworkPolicy *does* bind on gVisor pods — raw-IP egress times out at the TCP layer, so
that enforcement is non-cooperative — and an HTTPS `CONNECT` tunnel from inside gVisor to a
proxy pod on the default runtime works. That combination is the shape a future standalone
gate would take; it is not what ships today.

## Upgrading past #240: retiring the GCS HMAC key

A deployment stood up before #240 reached Cloud Storage over its S3-interop XML API, under
an HMAC key pair belonging to a `map-storage` service account. The platform now reaches it
natively as the workloads themselves, so that key, that account and the two Secret Manager
containers holding the key's halves are all dead weight — and dead weight holding a live
credential. Nothing breaks if you leave them: the old `blob-*` keys still select the S3
path. But the credential is the thing #240 exists to delete, so leaving it is leaving the
problem.

The order matters, because the resources are deliberately hard to remove — `prevent_destroy`
**and** `deletion_policy = "PREVENT"`, the latter read from state, so simply pulling the
blocks out of the configuration makes the next apply *fail* rather than destroy them. That
is the guard working as designed; it just means removal is explicit.

**The first grant has to happen outside Terraform, and that is not fussiness.** Before
#240 the only bucket bindings in `environment/iam.tf` were the two held by `map-storage`;
the three workload identities had none. `make gcp-env-apply` on the new configuration adds
their three bindings and *removes* the old two in the same apply — so running it while the
deployment is still on the S3 path breaks object storage from that moment until the chart
is rolled, and rolling the chart first breaks it the other way, because the workloads
cannot yet read the bucket. There is no gap-free order using the two Terraform states
alone. Adding the three bindings by hand first costs one command and closes the window;
the later `apply` then finds them already present and only drops the two it should.

```sh
# 0. Both of these are REQUIRED here, with no default. Every command below names
#    a real account or secret by prefix, and two of them delete. A `:-map`
#    fallback on an unset variable would silently address a different
#    deployment's resources — granting bucket access to someone else's
#    identities, or deleting their secrets.
#    (No apostrophes in these messages: bash parses quotes inside ${VAR:?...}
#    even within double quotes, so one would open a string that never closes.)
: "${PROJECT:?set PROJECT to the project this deployment lives in}"
: "${NAME_PREFIX:?set NAME_PREFIX to the prefix this foundation was applied with}"

# 1. Grant the three workloads object access, out of band and before anything moves.
#    Terraform will converge on exactly these in step 3.
BUCKET="$(cd deploy/gcp/environment && terraform output -raw blob_bucket)"
for pair in "controlplane:objectUser" "executor:objectUser" "brain:objectViewer"; do
  SA="${NAME_PREFIX}-${pair%%:*}@$PROJECT.iam.gserviceaccount.com"
  # Confirm the account is the one you mean before handing it the bucket.
  gcloud iam service-accounts describe "$SA" --project "$PROJECT" >/dev/null
  gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" \
    --member="serviceAccount:$SA" \
    --role="roles/storage.${pair##*:}" --project "$PROJECT"
done

# 2. Move the deployment onto the native backend, and confirm it works.
#    The Secret loses blob-endpoint/blob-access-key/blob-secret-key/blob-region and
#    gains blob-backend=gcs, keeping blob-bucket. Roll the three Deployments; each
#    logs `object storage configured  backend=gcs` at startup.
#
#    REMOVE those keys, do not merely stop selecting them. The chart's env block
#    references every blob-* key with `optional: true`, so a key left in the Secret
#    is still injected into all three pods — a live HMAC pair sitting in the
#    environment of processes that no longer use it is exactly what #240 deletes.
#
#    Check the brain's ServiceAccount carries its Workload Identity annotation
#    before rolling. It reads the bucket now, and a brain that cannot degrades
#    silently: rubric and deliverable reads fall back rather than failing.

# 3. Reconcile Terraform. This is the apply that drops the two map-storage bindings.
make gcp-env-apply

# 4. Only once reads and writes are proven on the new path, deactivate and delete
#    the key. Deactivate first — a key must be INACTIVE before GCS will delete it,
#    and an inactive key is a reversible way to prove nothing still depends on it.
gcloud storage hmac list --project "$PROJECT"
gcloud storage hmac update  "$ACCESS_ID" --deactivate --project "$PROJECT"
gcloud storage hmac delete  "$ACCESS_ID" --project "$PROJECT"

# 5. Drop the three foundation resources from state, then delete the real ones.
#    `state rm` rather than `destroy`: deletion_policy = "PREVENT" lives in state,
#    so Terraform refuses, and that refusal is the point.
cd deploy/gcp/foundation
terraform state rm google_service_account.storage \
                   google_secret_manager_secret.blob_access_key \
                   google_secret_manager_secret.blob_secret_key
gcloud secrets delete "${NAME_PREFIX}-blob-access-key" --project "$PROJECT"
gcloud secrets delete "${NAME_PREFIX}-blob-secret-key" --project "$PROJECT"
gcloud iam service-accounts delete \
  "${NAME_PREFIX}-storage@$PROJECT.iam.gserviceaccount.com" --project "$PROJECT"

# 6. Confirm the configuration and the world agree.
terraform plan   # no changes
```

Step 4 before step 5, always: deleting the service account deletes its HMAC keys with it,
which is fine once the key is unused and awkward if it is not — the secret GCS returned at
creation is not recoverable, so a key you delete by accident cannot be put back.

`state rm` is the path written here because it needs no version floor and no second apply,
not because it is the only one. Terraform offers two alternatives for exactly this shape,
and either would work: a `removed` block with `lifecycle { destroy = false }` (Terraform
≥ 1.7) makes Terraform forget a resource without asking the provider to delete it, so
`deletion_policy` never comes up; or setting `deletion_policy = "ABANDON"` and applying
before deleting the blocks reaches the same place in two applies. They are not used here
because this is a one-time migration for deployments that predate #240, and both would
leave permanent configuration in `foundation/` to serve it.

## Backups, and the lifecycle that loses data

Back up **Cloud SQL**. It holds the event log, which is the single source of truth for
every session, and it holds vault credential ciphertext. `environment/` sets no automated
backup configuration — that is a staging choice, and it is wrong for anything you would
miss.

One credential lifecycle is not covered by a database backup, and it fails badly. (There
were two until #240 retired the GCS HMAC key, whose secret GCS returned exactly once and
which a deleted service account took with it. Object storage has no credential now.)

- **The Cloud KMS key. This is a data-loss hazard, not a rotation chore.** Vault
  credential ciphertext in Postgres is decryptable by that key version and by nothing else.
  A key ring can never be deleted in GCP; a key can be, but only after every one of its
  versions is destroyed and deleted, so that route runs *through* the data loss rather
  than around it. The everyday hazard is smaller and closer: a key *version* can be
  destroyed on its own, and destroying the version that encrypted live ciphertext makes
  it permanently unreadable. A database backup does not help, because the backup contains the same
  ciphertext. Treat the key version as part of the backup set, and do not destroy one to
  save the ~$0.06/month an enabled version costs.

## Tearing it down

```sh
make gcp-env-destroy
```

destroys the cluster, Cloud SQL, the bucket and the registry, and leaves `foundation/`
alone. Three settings make that possible and are deliberately non-default: the cluster's
`deletion_protection`, Cloud SQL's two separate deletion-protection flags, and the bucket's
`force_destroy`. All three are correct for staging and wrong for anything holding data
someone would miss.

A fourth setting is there for a reason worth knowing, because it is the direct cost of the
non-superuser database role: `google_sql_database.map` carries
`deletion_policy = "ABANDON"`. The Admin API's database-delete runs as `cloudsqlsuperuser`,
and `make gcp-db-init` hands the database to the platform's own role, so the API cannot drop
it — `must be owner of database map`, and the destroy stops there with the instance still
running. Handing ownership back first is not an option: the instance is private, so only a
Pod in the cluster can reach it, and the destroy removes the cluster first. `ABANDON` drops
the database from Terraform's state without an API call and lets the instance take it. If
you copy this Terraform and drop that line, the first teardown after a `gcp-db-init` will
strand a Cloud SQL instance.

**Expect to run the destroy more than once, and not because anything is wrong.** Cloud SQL
releases the VPC peering it uses for its private address *asynchronously*, so the
`google_service_networking_connection` delete lands while the release is still in flight and
fails with `Producer services (e.g. CloudSQL, Cloud Memstore, etc.) are still using this
connection` — with the instance already gone from `gcloud sql instances list`. Re-run
`make gcp-env-destroy`.

**But do not sit and wait for it, because "asynchronously" here means days.** Google
documents the interval: *"if you delete a Cloud SQL instance, you receive a success
response, but the service waits for four days before deleting the service producer
resources"*
([private services access](https://cloud.google.com/vpc/docs/configure-private-services-access#delete-private-connection)).
So the destroy cannot succeed until those four days are up, however often you re-run it.
The mode-2 acceptance teardown retried 25 times over about four hours — five-minute
intervals, then ten — and the peering was still `ACTIVE / Connected` at the end, which is
the documented behaviour rather than a surprise. The manual `gcloud services vpc-peerings
delete` is subject to the same wait: it fails with
`FLOW_SN_DC_RESOURCE_PREVENTING_DELETE_CONNECTION`, meaning a resource inside Google's own
producer project still holds the connection.

**Which is fine, and this is the part worth remembering.** What that failure strands is the
VPC, the reserved peering range, the connection itself and the API enablements — **none of
them billable**. The run that hits this has already deleted the cluster, the instance, the
bucket and the registry, so the cost is settled and the remainder is bookkeeping. Stopping
here is a legitimate end state: re-run `make gcp-env-destroy` once the four days are up,
or leave it, since a later `make gcp-env-apply` adopts the same resources anyway.

**Do not reach for the consumer-side peering delete** (`gcloud compute networks peerings
delete servicenetworking-googleapis-com`). It is the obvious-looking way out and Google
warns against it in the same document — *"Don't attempt to delete a private connection by
deleting its associated VPC Network Peering connection directly"* — because creating a
private connection afterwards can then fail, and recovering means re-creating it with the
same allocated range names. This teardown did not try it, so there is no local evidence
either way; the documented warning is reason enough.

**Then check for orphaned disks. `terraform destroy` does not reclaim them.**

If you ran mode 1, the chart's bundled Postgres, MinIO and OpenBao each have a
PersistentVolumeClaim, and the GKE PD CSI driver creates a Compute Engine disk for each.
Those disks are not in Terraform's state, so the destroy takes the cluster and leaves them
behind, billing at the full provisioned rate with nothing able to re-attach them. After the
first real teardown of this configuration, **six** were left: 34 GiB, about $3.40/month,
one set per build-up.

```sh
gcloud compute disks list --project YOUR_PROJECT \
  --format='table(name,zone.basename(),sizeGb,status,users.list(),description)'
```

**Do not filter by label.** Only some of them carry `goog-k8s-cluster-name`; disks from a
later cluster generation had no labels at all, so a label-filtered sweep deletes half the
leak and reports success. The reliable handle is the PVC name in the disk's `description`.
An unattached disk shows an empty `users` column.

The OpenBao volume holds the vault's own state, so this is a data-remanence question as
well as a cost one.

**In mode 2 this trap cannot fire, and the reason is structural rather than lucky.** Note
what `existingSecret` does and does not do: it does not turn the bundled services off, it
makes leaving them **on** a render error — you set `postgresql.enabled`, `minio.enabled` and
`openbao.enabled` to `false` yourself, and the chart refuses to deploy if you forget. With
all three off the release renders no StatefulSet, and a release with no StatefulSet claims
no volume: the mode-2 acceptance run
finished with zero PersistentVolumeClaims and zero PersistentVolumes in the cluster, and
the only disks in the project were the four node boot disks the destroy reclaims. Check
anyway — the command above costs nothing and a mode-1 experiment in the same project would
still leave its own — but a clean mode-2 teardown genuinely has nothing to sweep.

Also worth checking, because none of it is in Terraform's state either: soft-deleted
buckets still inside their retention window (they bill until they hard-delete, and they
self-clear), and Secret Manager versions for infrastructure that no longer exists. A
deployment that predates #240 also has an HMAC key that nothing uses any more — see the
migration note above, since `gcloud storage hmac list` is the only place it appears.

What is retained by design: the foundation's service accounts, its Secret Manager secrets,
and its KMS key ring and key. A key ring can never be deleted at all; a key can only be
deleted once every version of it is destroyed and deleted, which is the data loss rather
than an escape from it — and the name stays retired afterwards either way. One enabled key
version costs about $0.06/month, and that is the standing cost of being able to rebuild
`environment/` safely.

## What this guide does not cover

Deliberately out of scope, so a reader can tell deferred from forgotten: Binary
Authorization, Shielded Nodes secure boot, node auto-repair and auto-upgrade, a dedicated
node service account, CMEK on the bucket and registry, bucket versioning, Cloud SQL
automated backups, Postgres audit logging, a fully private control-plane endpoint (which
needs a bastion or VPN before anything can be deployed), and External Secrets Operator as
an alternative to the pre-created Secret. Each is a normal GKE or Cloud SQL setting; none
of them is a platform concern.
