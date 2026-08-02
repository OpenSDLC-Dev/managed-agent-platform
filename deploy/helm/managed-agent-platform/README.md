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
`protocol`, `base_url`, and `api_key`, plus optional `upstream_model` / `headers` /
`stall_timeout` — no other keys. `stall_timeout` is a Go duration bounding how long
that endpoint may send nothing at all before the turn is abandoned (default 10
minutes; every byte received buys the budget back, so it never ends a healthy
turn — [#121](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/121)). `base_url` is the API root — the adapter appends `/v1/messages`
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
minimal deployments only. Leaving all three unset deploys without a cipher: the
platform runs, vault credential storage is unavailable.

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
| `executor.gateImage` | `""` (gate off) | per-session egress-gate sidecar image (`--target gate` build); setting it opts `limited` / vault-attached sessions into the gate — allowed_hosts enforcement plus vault-credential substitution at egress. The sidecar needs `CAP_NET_ADMIN` (no `restricted` Pod Security on the namespace) and, as a native sidecar, Kubernetes >= 1.29 (the render fails on older clusters); unset keeps the fail-closed route-flush |
| `executor.sandboxHardening.*` | `""` (executor defaults) | containment applied to every sandbox Pod (#65): `cpuMillis`, `memoryBytes`, `ephemeralStorageBytes`, `capDrop`, `readOnlyRootfs`, `runAsUser`. Empty keeps the executor's own defaults — 2 CPUs and the `NET_RAW,SETUID,SETGID` drops. Turning one **off** is per field, not one rule: `0` for a numeric cap, `none` for `capDrop`, `false` for `readOnlyRootfs` — and for `runAsUser` only an **empty** value, because `0` is a valid uid meaning **root** and `none` fails executor startup |
| `sandboxPlacement.nodeSelector` / `.tolerations` | `{}` / `[]` | where sandbox Pods may run: node labels they require, and taints they tolerate. Written as ordinary Kubernetes shapes (a map and a list of Toleration objects); the chart encodes them for the executor. **Not** `executor.nodeSelector`/`executor.tolerations`, which place the executor's own Deployment |
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
