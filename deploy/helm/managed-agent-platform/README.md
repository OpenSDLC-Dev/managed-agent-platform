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

The **BYOC worker is deliberately not in this chart** — it runs on the customer's own
compute, outside the platform cluster, and reaches the control plane only over the wire.

## Prerequisites

- Kubernetes ≥ 1.26 and Helm ≥ 3.
- **Container images.** This repository does not publish images yet. Build and push
  `controlplane`, `brain`, and `executor` images to a registry your cluster can pull,
  then point `image.registry` / `image.repository` / `image.tag` at them. Each process
  is expected at `{registry}/{repository}/{component}:{tag}` and started with
  `command: ["/<component>"]`.
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
  --set image.registry=your-registry.example.com \
  --set image.repository=your-org/managed-agent-platform \
  --set image.tag=0.1.0 \
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
`protocol`, `base_url`, and `api_key`, plus optional `upstream_model` / `headers` —
no other keys. `base_url` is the API root — the adapter appends `/v1/messages`
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
alongside Postgres, restore OpenBao first, and treat losing the transit key as
losing every secret encrypted under it (credential metadata survives; secrets
must be re-entered). See docs/self-hosted-security.md.

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
minimal deployments only. Leaving all three unset deploys without a cipher: the
platform runs, vault credential storage is unavailable.

## Security notes

- **Sandbox Pod network isolation.** The executor launches sandbox Pods in the release
  namespace, alongside the control plane, brain, and Postgres. The chart ships **no
  NetworkPolicy**, so a tool running in an unrestricted-networking sandbox can reach those
  in-cluster services. On a cluster with a policy-enforcing CNI, apply a NetworkPolicy that
  denies sandbox Pods (label `app.kubernetes.io/part-of: managed-agent-platform` is **not**
  set on them; select by the provider's `dev.opensdlc.managed-agent-platform.session-id`
  label) egress to the control-plane and Postgres Services. A first-class egress proxy is a
  reserved seam (see the plan), not yet built.
- **Pod Security Admission and limited networking.** A session whose environment sets
  `networking.type: limited` gets a sandbox Pod with a `NET_ADMIN` init container (it flushes
  the Pod's routing table). A namespace enforcing the `baseline` or `restricted` Pod Security
  Standard will reject that Pod at admission, failing every tool call in the session. Install
  into a namespace that permits `NET_ADMIN` if you use limited networking; the default
  unrestricted-networking path needs no added capability.

## Managing your own Secret

To keep credentials out of Helm values, pre-create a Secret with keys
`controlplane-api-key`, `model-providers.json`, and `database-url` (plus the
`blob-*` keys for object storage and the `secrets-backend`/`bao-*`/`secrets-*`
keys for the credential cipher, if used), then set `existingSecret=<name>`. In
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
| `existingSecret` | `""` | reference a pre-created Secret instead of inlining |
| `executor.sandboxImage` | `debian:stable-slim` | base image for sandbox Pods |

See [`values.yaml`](./values.yaml) for the full set.

> **gVisor:** the plan lists an optional gVisor `RuntimeClass`. It is deferred here — the
> Kubernetes sandbox provider does not yet set `runtimeClassName` on the Pods it creates,
> so shipping a RuntimeClass would be unwired. It lands with provider support.
