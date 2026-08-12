# managed-agent-platform Helm chart

Deploys the platform's three server processes into a Kubernetes namespace:

| Process | Kind | Scales | Role |
|---|---|---|---|
| **controlplane** | Deployment + Service | independently | the wire-compatible REST API + event log |
| **brain** | Deployment | independently | the model-turn orchestration pool |
| **executor** | Deployment + RBAC | independently | runs tools in a per-session **Kubernetes sandbox Pod** |

An optional in-cluster **Postgres** (StatefulSet) is included for a batteries-included
install, likewise an optional in-cluster **MinIO** (StatefulSet) — S3-compatible
object storage for skill archives (consumed by the skills registry as
docs/plan/06_skills.md lands) — and an optional in-cluster **OpenBao**
(StatefulSet) — the transit cipher for vault credential material
(docs/plan/12_vaults-credentials.md). All three follow the same rule: bundled
single-node instances for dev/POC, hand-written templates rather than subcharts
(air-gap self-hosting must not require pulling an external chart), and a
production recommendation to disable them and point `externalDatabase` /
`externalObjectStorage` / `externalOpenBao` at services with their own backup
and upgrade lifecycle. The platform speaks plain S3 — any compatible store
(AWS S3, Ceph RGW, …) works — and the plain Vault-compatible transit HTTP API.
On Google Cloud there is a third object-storage option, `gcsObjectStorage`,
which reaches Cloud Storage natively and carries no credential at all (#240).

The **BYOC worker is deliberately not in this chart** — it runs on the customer's own
compute, outside the platform cluster, and reaches the control plane only over the wire.

## Prerequisites

- Kubernetes ≥ 1.26 and Helm ≥ 3.
- **Container images.** From v0.2.0 onward, releases publish `controlplane`,
  `brain`, and `executor` images to `ghcr.io/opensdlc-dev/managed-agent-platform`
  (one build, three names — same digest; see docs/RELEASING.md), and the chart's
  defaults resolve to them with the tag following `appVersion`. For a chart
  predating the first published release, or to run your own build, push images a
  cluster can pull and point `image.registry` / `image.repository` / `image.tag`
  at them. Each process is expected at `{registry}/{repository}/{component}:{tag}`
  and started with `command: ["/<component>"]`.
- A model endpoint the brain can reach (an Anthropic-protocol endpoint or an
  OpenAI-compatible gateway), configured via `brain.modelProviders`.

## Install

Minimum required values: a bootstrap API key, at least one model provider, and — with
the bundled Postgres, MinIO, and OpenBao — a database password, MinIO root
credentials, and the OpenBao seal key + platform token (none is auto-generated: a
generated credential is unstable under `helm template`/GitOps; MinIO requires a
root password of at least 8 characters).

```bash
helm install map ./deploy/helm/managed-agent-platform \
  --namespace map --create-namespace \
  --set controlplane.apiKey=$(openssl rand -hex 24) \
  --set postgresql.password=$(openssl rand -hex 24) \
  --set minio.rootUser=map \
  --set minio.rootPassword=$(openssl rand -hex 24) \
  --set openbao.staticSealKey=$(openssl rand -base64 32) \
  --set openbao.platformToken=$(openssl rand -hex 24) \
  --set-json 'brain.modelProviders=[{"model":"*","protocol":"anthropic","base_url":"https://gateway.internal","api_key":"sk-..."}]'
```

`brain.modelProviders` is a **list** of model routes, rendered verbatim as a JSON
array to the file the brain reads (`MODEL_PROVIDERS_PATH`); its `api_key` is stored
in the chart's Secret. Each entry is `model` (route key, `"*"` = default),
`protocol`, `base_url`, and `api_key`, plus optional `upstream_model` / `headers` /
`stall_timeout` / `max_tokens` — no other keys. `stall_timeout` is a Go duration bounding how long
that endpoint may send nothing at all before the turn is abandoned (default 10
minutes; every byte received buys the budget back, so it never ends a healthy
turn — [#121](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/121)).
`max_tokens` is the default output cap for turns that set none themselves (omitted,
the anthropic adapter sends 8192 and the openai adapter defers to the endpoint); it
must be positive — zero or negative fails the brain at startup rather than silently
taking the default — and worth sizing up for routes whose agents write whole files
through tool calls. `base_url` is the API root — the adapter appends `/v1/messages`
(anthropic) or `/v1/chat/completions` (openai), so omit a trailing `/v1`. (The loader
also accepts `api_key_env`, but the chart injects no extra
env into the brain, so supply `api_key` here.) See `internal/provider` for the schema.

The install above — a `"*"` route with no `upstream_model`, the common shape in front
of a gateway — **passes the caller's own model string through** to the endpoint and
into the `gen_ai.request.model` metric attribute. Metric attributes are aggregation
keys, so anyone who can supply a model string (creating an agent, or a session with an
`agent_with_overrides` block) then controls your metrics backend's series count. Set
`upstream_model`, or use per-model routes, if those paths are exposed to untrusted
callers — see the `brain.modelProviders` comment in `values.yaml` and
[#88](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/88).

## The executor and the Kubernetes sandbox

The executor is wired to the **k8s** sandbox backend (`SANDBOX_BACKEND=k8s`). It launches,
execs into, and tears down one sandbox Pod per session in the release namespace, using
**in-cluster config** (its `SANDBOX_K8S_KUBECONFIG` / `_CONTEXT` are intentionally unset).
The chart grants its ServiceAccount a namespaced Role with exactly the pod lifecycle and
`pods/exec` verbs the provider calls — nothing cluster-wide.

## Database

`postgresql.enabled=true` (default) runs a single-replica in-cluster Postgres and builds
`DATABASE_URL` for you. You must set `postgresql.password` — the chart does **not**
auto-generate it (a generated password is unstable under `helm template`/GitOps, where it
would churn on every render and drift from the initialized database). The password is
embedded in `DATABASE_URL`, so it must be URL-safe; the chart rejects a value containing
`@ : / ? # %` or spaces. Postgres listens on the standard `5432` (not configurable for the
bundled instance).

**For production, disable the bundled Postgres and point at your own managed database:**

```bash
--set postgresql.enabled=false \
--set externalDatabase.url='postgres://user:pass@host:5432/db?sslmode=require'
```

This is a deliberate divergence from bundling a Postgres subchart: a self-hostable,
air-gap-friendly platform should not require pulling an external chart from a repo, and
production operators run their own database anyway.

### Cloud SQL Auth Proxy (`cloudSQLProxy.enabled`)

On GKE, Google's documented way to reach a **private-IP** Cloud SQL instance is the Cloud
SQL Auth Proxy running in the same pod as the workload. Turning it on adds that container to
the controlplane, brain and executor deployments:

```bash
--set postgresql.enabled=false \
--set cloudSQLProxy.enabled=true \
--set cloudSQLProxy.instanceConnectionName=my-project:us-central1:my-instance \
--set externalDatabase.url='postgres://user:pass@127.0.0.1:5432/db?sslmode=disable'
```

Four things about it are worth knowing before you turn it on.

**The DSN is yours to point at the proxy.** The chart does not rewrite it, and under
`existingSecret` it never sees one — so a sidecar with a DSN still naming the instance's own
address is a proxy nothing connects through. It does not refuse that in the
`externalDatabase.url` path either, where it *does* hold the DSN, and the reason is not
reach but grammar: a DSN through the sidecar can legitimately say `127.0.0.1`, `localhost`,
`[::1]`, or a unix socket path, so a host check would refuse working deployments.

**`sslmode=disable` is correct here and nowhere else.** The proxy terminates the encrypted
leg itself and hands the process a loopback socket, so the hop the DSN describes never
leaves the pod. That is also why this is a sidecar rather than a shared proxy Deployment:
the tidier-looking shape puts the password and every query on the pod network in cleartext,
on the near side of the encryption, where neither the instance's `ssl_mode` setting nor
`pg_stat_ssl` can see it.

**One switch, three deployments.** All three read a single `database-url` Secret key, so a
DSN cannot name a loopback socket for some pods and an address for the others.

**Each pod needs an identity.** The proxy authenticates as the pod's Google service account,
which needs `roles/cloudsql.client` — so all three `serviceAccount.annotations` blocks come
into play, including `brain.serviceAccount.annotations`. Give the brain an account of its
own rather than the control plane's, which carries KMS decrypt it has no business holding.
[`deploy/gcp/`](../../gcp/) creates all three accounts and their Workload Identity bindings,
and [docs/deploy-gcp.md](../../../docs/deploy-gcp.md) documents the simpler alternative:
a Pod reaching the private IP directly with `sslmode=require`, no sidecar and no Google
identity, trading certificate verification and IAM authorization for having nothing to set
up.

It renders as a **native sidecar** — an `initContainer` with `restartPolicy: Always` — with
a startup probe on the proxy's own health endpoint, so the socket is listening before the
process that dials it starts. That needs **Kubernetes >= 1.29**; older clusters fail the
render. So do a missing `instanceConnectionName`, one of the wrong shape, and the bundled
Postgres left enabled alongside it.

The shape check is exactly this: **three or four non-empty colon-separated segments** —
`PROJECT:REGION:INSTANCE`, or `DOMAIN:PROJECT:REGION:INSTANCE` for a legacy domain-scoped
project. It refuses an address such as `10.1.2.3` or `db.internal:5432`, and a name with an
empty part; it does not look inside the segments. A well-formed name that names nothing
therefore renders, and the proxy rejects it at startup — deliberately, because encoding
Google's own naming rules in a template would refuse a valid name the day Google widens one.

## Credential cipher (OpenBao)

`openbao.enabled=true` (default) runs a single-replica in-cluster OpenBao whose
**transit** engine encrypts vault credential material
(docs/plan/12_vaults-credentials.md): ciphertext lives in Postgres, only the key
material lives in OpenBao's storage. The StatefulSet self-initializes — an init
sidecar performs `bao operator init` on first boot (root token and recovery keys
land on the data PVC, a documented dev-grade convenience), mounts transit, and
mints the transit-scoped periodic token the controlplane and executor
authenticate with (`openbao.platformToken`). `openbao.staticSealKey` (base64 of
exactly 32 random bytes) drives the KMS-free static auto-unseal; changing it
after first boot bricks the instance.

**Back up in pairs, restore in order:** a Postgres backup restores ciphertext
that only the matching transit key can open — back up OpenBao's storage
alongside Postgres **and preserve `openbao.staticSealKey`** (it lives in your
values/Secret, not on the PVC, and a restored instance cannot unseal without
the exact key it was sealed with), restore OpenBao first, and treat losing the
transit key as losing every secret encrypted under it (credential metadata
survives; secrets must be re-entered). See docs/self-hosted-security.md.

**For production, point at your own OpenBao/Vault** — any endpoint speaking the
Vault-compatible transit HTTP API:

```bash
--set openbao.enabled=false \
--set externalOpenBao.address='https://bao.internal:8200' \
--set externalOpenBao.token='<transit-scoped token>'
```

The token needs `update` on `transit/encrypt/<transitKey>` and
`transit/decrypt/<transitKey>`, plus `create`/`read`/`update` on
`transit/keys/<transitKey>` (the platform POSTs that path at startup to ensure
the key exists — an update once the key does), mirroring the bundled init
policy.

With the bundled instance disabled and no `externalOpenBao.address`, setting
`localCipher.masterKey` (base64, 32 bytes) selects the AES-256-GCM local cipher —
minimal deployments only. Leaving all four unset deploys without a cipher: the
platform runs, vault credential storage is unavailable.

### Google Cloud KMS (`gcpKMS.keyName`)

On GCP, set `openbao.enabled=false` and `gcpKMS.keyName` to a full CryptoKey
resource name — `projects/P/locations/L/keyRings/R/cryptoKeys/K` — and the
platform seals credential material through Cloud KMS. This is the one cipher
option that puts **no key material in the release**: KMS needs a key name, which
is not a secret, and an identity, which is an annotation.

That identity is the half the chart cannot supply for you. Authentication is
Application Default Credentials, which on GKE means Workload Identity, which
means binding a Google service account holding
`roles/cloudkms.cryptoKeyEncrypterDecrypter` on the key to the two Kubernetes
ServiceAccounts that hold a cipher:

```
--set openbao.enabled=false \
--set gcpKMS.keyName='projects/my-project/locations/us-central1/keyRings/map/cryptoKeys/credentials' \
--set 'controlplane.serviceAccount.annotations.iam\.gke\.io/gcp-service-account=map-kms@my-project.iam.gserviceaccount.com' \
--set 'executor.serviceAccount.annotations.iam\.gke\.io/gcp-service-account=map-kms@my-project.iam.gserviceaccount.com'
```

and, on the Google side, granting those two Kubernetes ServiceAccounts the right
to impersonate it. **Use the names the chart actually created** — they are
`<fullname>-controlplane` and `<fullname>-executor`, where `<fullname>` is the
release name only when it already contains the chart name and `RELEASE-CHARTNAME`
otherwise, so `helm install map ...` yields
`map-managed-agent-platform-controlplane`. Read them off the cluster rather than
constructing them:

```
# Select by THIS release's instance label, never by a name suffix: a namespace
# holding another release — or any unrelated ServiceAccount ending in
# -controlplane — would otherwise be granted the right to impersonate the KMS
# identity and inherit its key permissions.
kubectl get sa -n NAMESPACE \
  -l app.kubernetes.io/instance=RELEASE \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'   # confirm the two names

for ksa in $(kubectl get sa -n NAMESPACE -l app.kubernetes.io/instance=RELEASE \
               -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'); do
  gcloud iam service-accounts add-iam-policy-binding map-kms@my-project.iam.gserviceaccount.com \
    --role roles/iam.workloadIdentityUser \
    --member "serviceAccount:my-project.svc.id.goog[NAMESPACE/$ksa]"
done
```

**The two need different key-level roles**, and granting both the same one
over-privileges the executor. The control plane encrypts on write and decrypts
for `mcp_oauth_validate`, so it needs
`roles/cloudkms.cryptoKeyEncrypterDecrypter`. The executor constructs the cipher
**only to fail fast on misconfiguration** — egress substitution decrypts
control-plane side, at the gate-config endpoint, never in the executor — so the
one KMS call it ever makes is the startup probe's encrypt, and
`roles/cloudkms.cryptoKeyEncrypter` is enough:

```
gcloud kms keys add-iam-policy-binding credentials \
  --keyring map --location us-central1 \
  --member serviceAccount:map-kms@my-project.iam.gserviceaccount.com \
  --role roles/cloudkms.cryptoKeyEncrypterDecrypter
```

(with a second Google service account bound only to
`roles/cloudkms.cryptoKeyEncrypter` if you want the executor separated; a single
account for both is simpler and is what the example above configures.)

The brain never gets the key name: it holds no cipher, so on KMS grounds it has
no reason to hold the identity either. It does have a **ServiceAccount** of its
own, for reasons that have nothing to do with KMS — reading the blob bucket
(`gcsObjectStorage`, #240) and the Cloud SQL Auth Proxy below — and keeping that
account separate is what stops the two from merging. **Do not annotate the
brain's KSA onto the KMS account above**: if you do, the brain inherits that
account's KMS decrypt, which is precisely the privilege it has no use for. Point
it at its own Google service account, the one carrying
`roles/storage.objectViewer` on the bucket and `roles/cloudsql.client` — and no
cipher role. It is also why each component has a ServiceAccount rather than falling
back to the namespace's `default` — annotating `default` would hand one Google
identity to every pod in the namespace that also defaults.

Two refusals are worth knowing before you deploy. A **CryptoKeyVersion** name
(`.../cryptoKeys/K/cryptoKeyVersions/1`) fails the render: `Encrypt` accepts one
where `Decrypt` refuses it, so a release configured with a version would seal
credentials it could never unseal. And Cloud KMS's raw `Encrypt` bounds
plaintext where OpenBao's transit engine does not — so a vault credential whose
sealed secrets exceed the bound is answered with a `400` naming the limit where
another cipher would have stored it
([docs/DIVERGENCES.md](../../../docs/DIVERGENCES.md)).

That bound depends on the key you point at, which is why the platform reads it
from the key at startup rather than assuming: **65536 bytes** for a
software-protected key, but only **8192** for an HSM one, where the service
bounds plaintext plus AAD together. Nothing in a resource name says which you
have. Real credential material — OAuth and bearer tokens, environment-variable
values — sits orders of magnitude below either.

## Single sign-on and roles (`identity.*`, `casdoor.*`)

Humans authenticate through an OIDC provider and the control plane resolves each
request to a principal holding one of three roles — `admin`, `developer`,
`viewer` — enforced per route (docs/plan/31_console-sso-rbac.md,
[#56](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/56)). The
`identity` block is that configuration and the chart injects it into the
controlplane Deployment always; `casdoor.enabled` additionally deploys a bundled
provider for clusters that have none.

**Off is the default and means exactly today's platform.** With `identity.mode`
empty the chart renders no `IDENTITY_*` env at all — `IDENTITY_MODE` unset and
`IDENTITY_MODE=disabled` are one state to the control plane — so upgrading an
existing release does not change its controlplane pod spec, no Bearer JWT is
accepted anywhere, and `x-api-key` remains the only management credential. The
machine lanes never change: the management key keeps full authority with no role
model, and worker environment keys are untouched.

```bash
# Any compliant provider — Keycloak, Entra ID, Cognito, accounts.google.com.
--set identity.mode=oidc \
--set identity.oidc.issuer=https://idp.example.com/realms/platform \
--set identity.oidc.audience=<the console's client id> \
--set 'identity.roleMap.platform-admins=admin' \
--set 'identity.roleMap.platform-devs=developer'
```

`identity.roleMap` is written as a **map** — claim value to role — and the chart
encodes it into the flat `value=role,...` the verifier parses; a claim value or
role containing `,` or `=` fails the render, because it would encode as a
different, still-valid map. A principal whose claims map to nothing holds **no
role** and is denied on every route, so this map is the whole grant.
`identity.claims.roles` names the claim those values come from (default `roles`).

Everything else the platform refuses, it refuses at **startup** and by variable
name — an unreachable issuer, an empty role map, a malformed pair, a role outside
the three. Both URLs must be `https`: the verifier requires an https key set and
its dial guard refuses loopback, so a plain-HTTP provider cannot be wired to this
platform at all, and the control plane must be able to reach the issuer from
inside the cluster with a certificate its image's trust store already accepts.

Behind a cloud vendor's authenticating proxy, use `trusted_proxy` instead — the
shipped preset is Google's IAP, which supplies the header, issuer, key set and
algorithms itself and takes only the backend-service audience:

```bash
--set identity.mode=trusted_proxy \
--set identity.proxy.preset=gcp-iap \
--set identity.proxy.audience=/projects/123456789/global/backendServices/987654321
```

### The bundled provider (`casdoor.enabled`, default `false`)

For the private cluster with no cloud IdP — the deployment this project exists
for — `casdoor.enabled=true` renders a pinned Casdoor from **first-party**
templates: a Deployment, a **ClusterIP** Service, a Secret holding its DSN, a
ConfigMap holding its seed, an Ingress, and a NetworkPolicy. First-party rather
than the upstream `casdoor-helm` subchart, whose QA shipped a release that
ignored its Postgres values and silently fell back to SQLite. Owning the
templates means owning their lifecycle — image tag, database, seed, CVE tracking.

```bash
--set casdoor.enabled=true \
--set casdoor.ingress.host=idp.example.com \
--set casdoor.ingress.tls.secretName=idp-tls \
--set casdoor.adminPassword=$(openssl rand -hex 24) \
--set casdoor.console.clientSecret=$(openssl rand -hex 24) \
--set 'casdoor.console.redirectURIs[0]=https://console.example.com/callback' \
--set identity.mode=oidc \
--set identity.oidc.issuer=https://idp.example.com \
--set identity.oidc.audience=map-console \
--set identity.claims.roles=groups \
--set 'identity.roleMap.map/platform-admins=admin' \
--set 'identity.roleMap.map/platform-devs=developer' \
--set 'identity.roleMap.map/platform-read=viewer'
```

Four of those values are load-bearing in ways a plausible guess gets backwards,
each measured against a running 3.152.0 rather than read off its structs:

- **`identity.claims.roles` is `groups`, not `roles`.** Casdoor's `roles` claim
  carries role **objects**, and the platform collects only string values out of a
  claim — so mapping `roles` gives every human no role and denies them
  everywhere. `groups` arrives as strings spelled `organization/name`, which is
  why the map keys carry the seeded organization, `map`.
- **The audience is the application's client id**, because that is what Casdoor
  puts in `aud`.
- **The issuer is `https://` + `casdoor.ingress.host`, byte for byte**, because
  the chart configures that as Casdoor's `origin` and Casdoor stamps it into
  every token as `iss`, which the verifier compares exactly.
- **The host must be served over HTTPS**, by `tls.secretName` or by the
  controller's own default certificate, for the reason above.

The last two are the pair the chart owns both sides of, so a mismatch on either
**fails the render** rather than leaving every login to end in the same uniform
401 a forged token gets. So do a missing `casdoor.ingress.host`, `adminPassword`,
or `console.clientSecret`, and a missing `casdoor.database.dataSourceName` when
the bundled Postgres is off.

**Hardened by default**, because of CERT/CC VU#780781 — nine Casdoor CVEs, no
vendor statement and no named fixed version:

- The image is **pinned** (`casbin/casdoor:3.152.0`) past the release that
  silently fixed the worst of them, and bumping it is a deliberate change.
- The seed configures **zero upstream providers** (the binding path two
  9.1-severity ones live on), **one populated organization** beside Casdoor's own
  `built-in` (which voids the cross-org escalation), **no self-service signup**,
  and a grant list **without token exchange** (CVE-2026-9097, 9.8).
- The **Ingress refuses the SAML and CAS surfaces** — `/api/acs`,
  `/api/get-saml-login`, `/api/saml/metadata`, `/api/saml/redirect`, `/cas` — by
  routing them to a Service with no endpoints. They cannot be turned off in
  Casdoor: its router registers them whatever is configured, and one process
  serves the API and the login UI from one port, so "keep it internal" is not
  available as a control. CI reads that path list out of the compose proxy's
  config, so the two enforcement points cannot drift.
- The **NetworkPolicy admits pod ingress only from the ingress controller**,
  which is what makes those refusals a control rather than advice — reach the pod
  another way and they are simply not on the path. Its default peer names the
  namespace `ingress-nginx`; point `casdoor.networkPolicy.from` at your own
  controller, or set `enabled: false` if your CNI does not enforce policy.

Two operational facts worth knowing before you turn it on. **The chart owns
Casdoor's own administrator**: `casdoor.adminPassword` replaces the documented
default `123` on the `built-in/admin` account, which Casdoor creates itself
before reading the seed — that account is a Casdoor global administrator, so
treat the value as a bootstrap credential and change it at first login. It rides
the seed ConfigMap in plaintext (Casdoor hashes it on ingest and the plaintext
never reaches its database), next to `console.clientSecret`, which has always
been there on the same terms — anyone who can read ConfigMaps in the namespace
can read both. And **the seed is re-applied on every restart**, which is the only
setting under which it can own that password: entities the seed names — the
organization, the `map-console` application, the three groups, that account — are
restored from your values on each boot, so change them in values rather than in
the admin UI. Accounts you create for your people are not named by the seed and
survive.

The three groups the seed creates — `map/platform-admins`, `map/platform-devs`,
`map/platform-read` — are what `identity.roleMap` keys on. Create your operators
in the `map` organization and put each in one of them; the compose bundle's three
demo accounts are deliberately **not** seeded here, since they carry documented
passwords and this IdP is published.

## Security notes

- **Sandbox Pod network isolation.** The executor launches sandbox Pods in the release
  namespace, alongside the control plane, brain, and Postgres. The chart ships **no
  NetworkPolicy**, so a tool running in an unrestricted-networking sandbox can reach those
  in-cluster services. On a cluster with a policy-enforcing CNI, apply a NetworkPolicy that
  denies sandbox Pods (label `app.kubernetes.io/part-of: managed-agent-platform` is **not**
  set on them; select by the provider's `dev.opensdlc.managed-agent-platform.session-id`
  label) egress to the control-plane and Postgres Services. Setting `executor.gateImage`
  gives `limited` and vault-attached sessions a first-class egress gate (below), which
  enforces `allowed_hosts` but deliberately does not police in-cluster reachability for
  unrestricted sessions — the NetworkPolicy advice stands either way.
- **Pod Security Admission and the gate/limited paths.** With `executor.gateImage` unset, a
  `networking.type: limited` session gets a sandbox Pod with a `NET_ADMIN` init container
  (it flushes the Pod's routing table). With it set, `limited` **and vault-attached**
  sessions instead get the `NET_ADMIN` **gate sidecar** (no route flush) — note that this
  includes vault-attached sessions with unrestricted networking. Either shape is rejected at
  admission by a namespace enforcing the `baseline` or `restricted` Pod Security Standard,
  failing every tool call in those sessions. Install into a namespace that permits
  `NET_ADMIN` if you use limited networking or the gate; only the plain
  unrestricted-no-vaults path needs no added capability.

## Managing your own Secret

To keep credentials out of Helm values, pre-create a Secret with keys
`controlplane-api-key`, `model-providers.json`, and `database-url` (plus the
`blob-*` keys for object storage and, for the credential cipher if used, the
`secrets-backend` key plus that backend's own — `bao-*` for OpenBao, `secrets-*`
for the local cipher, `gcpkms-key-name` for Cloud KMS), then set
`existingSecret=<name>`. In
this mode the chart creates no Secret and does not manage in-cluster backing
services (`postgresql.enabled`, `minio.enabled`, and `openbao.enabled` must be
false).

## Observability

Set `otlp.endpoint` (OTLP/gRPC) to ship traces, metrics, and logs from all three
processes; `otlp.insecure=true` to export without TLS.

## Notable values

| Key | Default | Meaning |
|---|---|---|
| `image.registry` / `image.repository` / `image.tag` | `ghcr.io` / `opensdlc-dev/managed-agent-platform` / chart `appVersion` | image coordinates |
| `controlplane.apiKey` | `""` (required) | bootstrap management `x-api-key` |
| `brain.modelProviders` | `[]` (required) | list of model routes (JSON array) |
| `otlp.endpoint` | `""` | OTLP/gRPC collector; empty disables export |
| `postgresql.enabled` | `true` | run the bundled Postgres |
| `postgresql.password` | `""` (required when enabled) | URL-safe DB password; not auto-generated |
| `externalDatabase.url` | `""` | DSN used when `postgresql.enabled=false` |
| `openbao.enabled` | `true` | run the bundled OpenBao (credential cipher) |
| `openbao.staticSealKey` / `openbao.platformToken` | `""` (required when enabled) | static-unseal key (base64, 32 bytes) and platform token; not auto-generated |
| `externalOpenBao.address` | `""` | external OpenBao/Vault URL when `openbao.enabled=false` |
| `localCipher.masterKey` | `""` | AES-256-GCM fallback when no OpenBao is configured |
| `gcpKMS.keyName` | `""` | Cloud KMS CryptoKey resource name; selects the KMS cipher (exclusive with the OpenBao options and `localCipher`). Needs the Workload Identity annotations below — no key material rides the Secret |
| `gcsObjectStorage.enabled` / `.bucket` | `false` / `""` (required when enabled) | reach Google Cloud Storage natively (#240), exclusive with `minio.enabled` and `externalObjectStorage.endpoint`. The bucket name is the whole configuration: no endpoint, no credential — authentication is Application Default Credentials, so it needs the Workload Identity annotations below. The bucket must already exist |
| `controlplane.serviceAccount.annotations` / `brain.serviceAccount.annotations` / `executor.serviceAccount.annotations` | `{}` | annotations on each component's ServiceAccount; `iam.gke.io/gcp-service-account` is how the Google-native backends authenticate. Which ones you need depends on the backend: `gcsObjectStorage` needs **all three**, since every process reaches object storage; `gcpKMS` needs only the controlplane and executor, the two that receive the cipher env; the brain otherwise needs one only for the Cloud SQL Auth Proxy |
| `cloudSQLProxy.enabled` / `.instanceConnectionName` | `false` / `""` (required when enabled) | run the Cloud SQL Auth Proxy as a native sidecar in all three deployments (below). The name is `PROJECT:REGION:INSTANCE`, never an address |
| `cloudSQLProxy.image` / `.privateIP` / `.resources` | `gcr.io/cloud-sql-connectors/cloud-sql-proxy:2.24.1` / `true` / `{}` | proxy image, whether to pass `--private-ip` (what an instance with no public address needs), and its resources |
| `existingSecret` | `""` | reference a pre-created Secret instead of inlining |
| `executor.sandboxImage` | `debian:stable-slim` | base image for sandbox Pods |
| `executor.gateImage` | `""` (gate off) | per-session egress-gate sidecar image (`--target gate` build); setting it opts `limited` / vault-attached sessions into the gate — allowed_hosts enforcement plus vault-credential substitution at egress. The sidecar needs `CAP_NET_ADMIN` (no `restricted` Pod Security on the namespace) and, as a native sidecar, Kubernetes >= 1.29 (the render fails on older clusters); unset keeps the fail-closed route-flush |
| `executor.sandboxHardening.*` | `""` (executor defaults) | containment applied to every sandbox Pod (#65): `cpuMillis`, `memoryBytes`, `ephemeralStorageBytes`, `capDrop`, `readOnlyRootfs`, `runAsUser`. Empty keeps the executor's own defaults — 2 CPUs and the `NET_RAW,SETUID,SETGID` drops. Turning one **off** is per field, not one rule: `0` for a numeric cap, `none` for `capDrop`, `false` for `readOnlyRootfs` — and for `runAsUser` only an **empty** value, because `0` is a valid uid meaning **root** and `none` fails executor startup |
| `sandboxPlacement.nodeSelector` / `.tolerations` | `{}` / `[]` | where sandbox Pods may run: node labels they require, and taints they tolerate. Written as ordinary Kubernetes shapes (a map and a list of Toleration objects); the chart encodes them for the executor. **Not** `executor.nodeSelector`/`executor.tolerations`, which place the executor's own Deployment |
| `sandboxImagePullSecrets` | `null` (inherits `imagePullSecrets`) | imagePullSecrets for the sandbox Pods the executor creates (#199) — same `{name: ...}` list shape as the top-level `imagePullSecrets`, which covers only the platform's own Pods. `null` inherits that top-level list (the sandbox Pods run in the release namespace, where the same Secrets answer); an explicit `[]` opts sandbox Pods out. Pod-level, so it also covers a limited session's net-setup image and a gated session's gate sidecar |
| `sandboxRuntimeClass.name` | `""` (cluster default) | `runtimeClassName` set on every sandbox Pod — a hardened runtime such as gVisor |
| `sandboxRuntimeClass.create` / `.handler` | `false` / `runsc` | also create the cluster-scoped `RuntimeClass` object named above |

See [`values.yaml`](./values.yaml) for the full set.

> **gVisor:** `sandboxRuntimeClass.name` puts a `runtimeClassName` on every sandbox Pod, so
> a cluster whose nodes run gVisor can isolate the sandbox with it — the strongest lever an
> operator has over a container that runs untrusted, model-directed commands. Point it at a
> `RuntimeClass` the cluster already has, or set `sandboxRuntimeClass.create=true` to have the
> chart create one (off by default: the object is cluster-scoped, so it collides with another
> release creating the same name, the install needs cluster-scope RBAC, and the nodes must
> already run the handler — and, being Helm-managed, `helm uninstall` takes it away again,
> which breaks anything else pointing at it).

<!-- Two separate notes: a bare blank line between blockquotes renders as one. -->

> **Process limits:** `executor.sandboxHardening` has no pids knob because Kubernetes has no
> per-pod one — it is the kubelet's `podPidsLimit` node setting, which you configure on the
> nodes, not in this chart. The Docker backend does cap it per container
> ([docs/DIVERGENCES.md](../../../docs/DIVERGENCES.md)).

<!-- Two separate notes: a bare blank line between blockquotes renders as one. -->

> **Disk limits:** `ephemeralStorageBytes` is the asymmetry pointing the other way — a
> Kubernetes-only cap on the node-local disk a sandbox may consume (its writable layer and
> every `emptyDir` under it), with no Docker counterpart, because whether a Docker daemon
> enforces a writable-layer quota depends on its storage driver — some do, some refuse the
> option outright, and some accept it and enforce nothing. Give it **bytes** —
> `21474836480`, not `20Gi`; a Kubernetes quantity string fails executor startup. Off by
> default, and worth understanding before turning on: Kubernetes enforces this one by
> **evicting the pod**, not by failing the write, so a cap set too low ends sessions
> mid-call rather than making a tool call fail — and it enforces it only on the node
> layouts whose local ephemeral storage the kubelet can measure, so check yours before
> treating the number as a bound
> ([docs/self-hosted-security.md](../../../docs/self-hosted-security.md) §3).

<!-- Two separate notes: a bare blank line between blockquotes renders as one. -->

> **Sandbox placement:** `sandboxPlacement` is a different pod from
> `executor.nodeSelector`/`executor.tolerations`, and the distinction is the reason it is
> top-level rather than nested under `executor`. The `executor.*` pair places the executor
> **Deployment**; `sandboxPlacement` places the per-session **sandbox Pods** the executor
> creates. A dedicated, tainted sandbox node pool needs the pair together — the selector
> reaches the pool, the tolerations get admitted onto its taint — and needs the executor
> itself to stay somewhere else. Both are validated at executor startup, against the rules the
> API server enforces at pod-create time — a malformed selector entry, an invalid label key or
> value, or a toleration the pod-create validator refuses stops the process, instead of failing
> every Provision for the life of the deployment. Two things it cannot check: whether the
> labels exist — a well-formed selector matching no node starts fine and leaves its Pods
> `Pending`, because only the cluster can answer that and the parse runs before there is a
> client to ask — and whether your cluster enables the alpha
> `TaintTolerationComparisonOperators` gate, so the `Lt`/`Gt` toleration operators are accepted
> here and refused at pod create by any cluster (including GKE) that has it off. Their values
> are still held to the server's rule — a canonical decimal integer fitting in 64 bits, so `5`,
> `0` and `-5` pass and `0100`, `+5` and `-0` do not.
