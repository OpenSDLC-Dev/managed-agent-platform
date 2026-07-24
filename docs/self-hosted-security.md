# Self-hosted security: the shared-responsibility model

Self-hosting inverts the usual SaaS security split. In a hosted product the
vendor owns almost the entire perimeter; here **you run every process and own
the infrastructure they run on**, so most of the perimeter is yours. This
document draws the line precisely: what the platform enforces in code — behaviour
you get for free the moment you deploy — versus what you, the operator, must
configure to run it safely in production.

It is scoped to a self-hosted deployment of this platform. It is a *deployment
security model*, not a vulnerability-disclosure policy, and not a substitute for
your own threat model. Where the platform does **not** yet enforce something this
model would ideally own, this document says so plainly and links the tracking
issue, rather than implying coverage that does not exist. The design-level
invariants it builds on are stated in
[docs/ARCHITECTURE.md → Security invariants](./ARCHITECTURE.md#security-invariants);
deliberate divergences from the reference are in
[docs/DIVERGENCES.md](./DIVERGENCES.md).

## The split at a glance

| Concern | Platform enforces (in code) | You (the operator) own |
|---|---|---|
| **Sandbox image** | Requires only `/bin/bash` + a POSIX userland; pulls the image you name | Building and pinning a hardened, minimal image; keeping it patched |
| **Non-root execution** | Sets no user — the container runs as your image's default user | Shipping an image whose default user is unprivileged |
| **Linux capabilities** | Adds none to the sandbox (one init container adds `NET_ADMIN`, below) | Dropping capabilities at the runtime / orchestrator layer |
| **Read-only root filesystem** | Not set | Enforcing it at the runtime layer, with writable mounts for the workdir |
| **Sandbox egress** | `limited` networking **fails closed** (no route out); default networking is unrestricted | Firewalling / `NetworkPolicy` for the default (non-`limited`) case |
| **Runtime isolation** | Runs tools in a per-session container; no gVisor/Kata wired yet | Choosing a hardened container runtime (gVisor, Kata, userns-remap) |
| **Environment-key lifecycle** | Hash-only storage, one live key per environment, revoke-on-re-mint, per-environment scope | Provisioning keys, rotation cadence, transport secrecy |
| **Model / tool credentials** | Never enter the sandbox; redacted from error events | Securing the brain's provider config and any egress-time secrets |
| **Auth transport** | Hashes `x-api-key` and environment keys at rest; scopes each | Terminating TLS; keeping keys off logs and out of images |
| **Single-tenant daemon trust** | The `ours` label guards *accidents*, not a hostile co-tenant | Treating the Docker daemon / cluster as a single trust domain |

The rest of this document expands each row: first what the platform enforces,
then what you own.

## What the platform enforces

These hold without any operator action. They are the invariants the codebase
tests and the reference design commits to.

- **Credentials never enter the sandbox.** Model API keys live in the brain's
  provider config; the sandbox — where untrusted tool commands run — never sees
  them. Provider adapters redact the credentials they were configured with (the
  API key, a `base_url` userinfo password, an auth header) out of any error that
  quotes an endpoint (`internal/provider/redact.go`), so an endpoint that echoes
  the request's auth header back cannot land the key in a `session.error` event
  (which is append-only and re-served to clients). A model's *successful* output
  is a trusted boundary and is never redacted — it is the content the session
  exists to record. Tool-time credentials (vaults, egress injection) are a
  reserved seam, deferred; see [Reserved seams](#reserved-seams-and-tracked-gaps).

- **Auth is scoped and hashed at rest.** Management calls carry `x-api-key`;
  workers carry an environment key as `Authorization: Bearer`. Both are stored as
  SHA-256 hashes, never in the clear (`internal/api/auth.go`,
  `internal/api/envauth.go`; `environment_keys.key_hash` in migration `0001`). An
  environment key resolves to exactly one environment's work queue: a worker can
  neither drive another environment's queue nor read or write another
  environment's sessions. A worker probing a session id it does not own gets the
  **same 404** as a nonexistent id (`requireEnvironmentKeyForSession`), so it
  cannot even learn that another environment's sessions exist.

- **The sandbox is minimally privileged toward the orchestrator.** On
  Kubernetes the sandbox pod is created with `automountServiceAccountToken:
  false` — the agent's commands cannot inherit any RBAC, because the pod holds no
  ServiceAccount token; the provider drives the cluster with its own credentials,
  not the pod's (`internal/sandbox/k8s/k8s.go`). The pod uses `restartPolicy:
  Never`, and the executor's own RBAC is namespaced and minimal (`create`/`get`/
  `delete` on `pods`, `create` on `pods/exec`, nothing cluster-wide;
  `deploy/helm/managed-agent-platform/templates/executor-rbac.yaml`). On Docker
  the sandbox runs with `HostConfig.Init: true` so orphaned tool subprocesses are
  reaped rather than piling up as zombies (`internal/sandbox/docker/docker.go`).

- **`limited` networking fails closed.** A `limited` environment means "only the
  allowed hosts", which needs the reserved egress proxy. Until that proxy exists,
  the platform refuses to guess: a `limited` Docker sandbox gets
  `NetworkMode: none` (no route out at all), and a `limited` Kubernetes sandbox
  gets an init container that flushes the pod netns routing table and **fails the
  pod** if any IPv4 route survives — enforced before the sandbox container starts.
  It never silently falls open to unrestricted egress. (`allowed_hosts` is
  accepted but not yet honoured — tracked in [#50](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/50).
  The K8s flush is not equivalent to `NetworkMode: none` on every cluster: raw
  `AF_PACKET` sockets can still reach the segment, and only the main IPv4 table is
  inspected, so policy-routing CNIs and IPv6 egress still need the egress proxy.)

- **The container is the boundary.** Tools run inside the per-session sandbox
  with no host filesystem access. The built-in file and search tools do **no**
  lexical path confinement — an absolute path or glob is accepted — because the
  container itself is the wall, and a lexical check a `bash` call could walk
  around would be theatre. This is only sound because the sandbox is genuinely
  isolated; hardening that isolation (below) is therefore load-bearing, not
  optional polish.

## What you own

Self-hosting means you supply the sandbox image, run the container runtime, and
place the deployment on your network. The controls below live at those layers,
not in the platform's request path — the platform reserves the seams but does not
set them for you today.

### 1. Sandbox image hardening

The sandbox image is **your** choice, not baked into the platform: the executor
and worker launch whatever image `EXECUTOR_IMAGE` / `WORKER_IMAGE` names, defaulting
to `debian:stable-slim` for local development (`cmd/executor/main.go`,
`cmd/worker/main.go`). The only contract the platform imposes is a POSIX userland
with `/bin/bash`; the `grep` built-in additionally expects GNU grep/coreutils
(a busybox-only image gets a clear tool error, not degraded behaviour).

For production, build a minimal, pinned image: only the interpreters and tools
your agents actually need, a non-root default user (below), no build toolchain or
package manager left in the final layer, and a patch cadence you control. The
platform will pull and run exactly what you specify — its security is your
responsibility.

### 2. Non-root execution

The platform sets no user on the sandbox container, so it **runs as your image's
default user**. `debian:stable-slim` defaults to root; a hardened image does not
have to.

To run tools unprivileged, ship an image with an unprivileged default user (e.g.
a `USER 10001` layer) and make sure that user can create and write the session
workdir (the container's entrypoint does `mkdir -p <workdir>`). This is the one
hardening dimension you can fully own through the image alone, with no platform
change — so do it.

### 3. Dropping Linux capabilities

The platform does not drop capabilities from the sandbox, so it runs with the
container runtime's **default capability set**. (The sole `SecurityContext` in
the codebase *adds* `NET_ADMIN` to the short-lived `netsetup` init container that
enforces `limited` networking — never to the sandbox container itself.)

Dropping capabilities is not a per-container toggle the platform exposes yet, so
enforce it at the layer you control:

- **Docker:** set a restrictive daemon default (`--cap-drop`), and keep the
  default seccomp and AppArmor/SELinux profiles enabled (do not run sandboxes
  `--privileged` or with `--security-opt seccomp=unconfined`).
- **Kubernetes:** since the sandbox pod ships no `securityContext` of its own
  today, dropping capabilities means a mutating admission webhook that injects
  one, or a locked-down node/runtime — Pod Security Admission can *reject* a
  non-conforming pod but cannot add a `securityContext` for you. Be aware that a
  strict Pod Security `restricted` label will in fact reject the current sandbox
  pod (it sets neither `runAsNonRoot` nor a dropped-capability list) until the
  provider sets one — the reserved seam tracked below.

### 4. Read-only root filesystem

The platform sets neither `ReadonlyRootfs` (Docker) nor `readOnlyRootFilesystem`
(Kubernetes). Enable it at the runtime layer if your threat model calls for it —
but note it is not a free toggle here: the sandbox writes the session workdir
(and, on Kubernetes, per-exec state under `/tmp`), so a read-only root needs
writable `tmpfs`/`emptyDir` mounts over those paths, which the platform's
provider does not currently arrange. Treat it as a deliberate runtime-level
change, not a flag.

### 5. Egress restriction

For a `limited` environment, egress is already enforced closed by the platform
(above) — you get no-route-out for free, with the CNI caveats noted. **For the
default (non-`limited`) case, egress is unrestricted**: a default Docker sandbox
gets `NetworkMode: bridge`, and the Kubernetes sandbox pod carries no
`NetworkPolicy`. If your agents should not reach the open internet or your
internal network, you must restrict it yourself:

- **Kubernetes:** apply a default-deny egress `NetworkPolicy` (plus explicit
  allows) to the namespace the sandbox pods run in. The platform ships none.
- **Docker / hosts:** firewall the sandbox network, or front outbound traffic
  with an egress proxy you control.

Until the reserved egress proxy lands, `allowed_hosts`-style per-host allowlisting
is not available in-platform; network-layer controls are the mechanism. The
built-in `web_fetch` / `web_search` tools are deferred for the same reason and
return an error if enabled ([#47](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/47)).

### 6. Environment-key rotation

The platform owns the *primitive*; you own the *lifecycle*. `EnsureEnvironmentKey`
makes a supplied value the one live worker credential for an environment: it
stores only the hash, and registering a fresh value **revokes the prior one**
(rotation-by-re-mint). A key value is bound to one environment for life; it is
never silently re-pointed. There is **no expiry or TTL** — a key is live until
re-minted or revoked — and there is **no automatic rotation**.

What you own:

- **Provisioning.** There is no operator wire endpoint that mints an environment
  key yet; today a key is seeded into the `environment_keys` table directly (via
  `EnsureEnvironmentKey`). Issuance UX is tracked in
  [#43](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/43). Treat
  key creation as a privileged, audited operation on your control-plane database.
- **Rotation cadence.** Because there is no TTL, rotation is a policy you enforce:
  on a schedule, and immediately on suspected worker compromise, re-mint the key
  (which revokes the old hash) and roll the new value out to the worker's
  `ANTHROPIC_ENVIRONMENT_KEY`. Revocation takes effect on the next request.
- **Transport secrecy.** The key travels as a Bearer token — terminate TLS in
  front of the control plane, and keep the value out of images, logs, and shell
  history. (The management `x-api-key` follows the same model: hashed at rest,
  rotation-by-restart via `EnsureAPIKey`.)

### 7. Credential-cipher key material and backup pairing

The vaults feature ([#50](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/50),
docs/plan/12_vaults-credentials.md — landing incrementally, cipher and deployment
first) encrypts vault credential material through `internal/secrets`: ciphertext
lives in Postgres, but the key that decrypts it lives **outside** — in your
OpenBao/Vault's own storage (`SECRETS_BACKEND=openbao`, the transit engine) or in
the `SECRETS_MASTER_KEY` you configured (`SECRETS_BACKEND=local`). That split is
the point — a Postgres dump alone cannot leak secrets — and it is also a
restore-ordering constraint you own:

- **Back up the pair together.** A Postgres backup restores ciphertext that only
  the matching transit key (or master key) can open. Back up the bao storage
  backend alongside Postgres — and, for the bundled instances, the static seal
  key too (compose `BAO_STATIC_KEY` / helm `openbao.staticSealKey` live outside
  the data volume, and a restored bao cannot unseal without the exact key it was
  sealed with); for `local`, escrow the master key.
- **Restore bao before anything that must decrypt.** Metadata CRUD works without
  the cipher; egress substitution and credential validation do not.
- **Losing the key loses every secret encrypted under it.** There is no recovery
  path; credential metadata survives and secrets must be re-entered.
- **The bundled dev instances store their own bootstrap material.** The compose
  `openbao` service and the chart's `openbao.enabled` StatefulSet self-initialize
  and keep the root token beside the data (compose `baoinit` volume / the chart's
  data PVC) — a documented dev-grade convenience. Production points
  `externalOpenBao` / `BAO_ADDR` at an instance whose unseal and audit story you
  run yourself.

### Host and runtime isolation

The sandbox runs untrusted, model-directed commands, so the strength of the
container boundary is the strength of your isolation. The platform does not pin a
hardened runtime, so choose one: a sandboxing runtime such as **gVisor** or
**Kata Containers**, or at minimum user-namespace remapping and the runtime
hardening from sections 3–4. On Kubernetes this is a `RuntimeClass` decision; the
Helm chart intentionally does not ship a gVisor `RuntimeClass` yet, because the
sandbox provider does not set `runtimeClassName` (documented in
[docs/DIVERGENCES.md](./DIVERGENCES.md)).

### Single-tenant daemon trust (Docker backend)

The Docker backend drives the host daemon to run sibling sandbox containers, which
means the executor mounts the Docker socket (`/var/run/docker.sock`) — **full
daemon access**. This is a local-development convenience; the production path is
the Kubernetes backend. The `ours` label the provider checks when adopting an
existing container guards against *accidents* on a single-tenant daemon (a
name collision, a container left by an earlier deployment); it is explicitly
**not** a trust boundary against a hostile actor with daemon access, who already
controls every sandbox on the host. If you run the Docker backend, treat the
daemon and every container on it as one trust domain.

## Reserved seams and tracked gaps

This model is honest about what is not yet enforced. These are reserved seams
with tracking issues, not silent omissions:

- **Egress proxy / `allowed_hosts`** — per-host allowlisting for `limited`
  environments; until then `limited` fails fully closed.
  [#50](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/50)
- **`web_fetch` / `web_search`** — deferred pending the egress policy above; return
  an error if enabled.
  [#47](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/47)
- **Environment-key issuance UX** — no operator wire endpoint yet; keys are seeded
  directly.
  [#43](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/43)
- **Sandbox `securityContext` / `runtimeClassName`** — the platform does not yet set
  capability drops, non-root, read-only rootfs, or a hardened RuntimeClass on the
  sandbox; these are operator-configured at the runtime layer today, as above.
- **Tool-time credential injection (vaults)** — the egress-time credential seam is
  reserved and deferred; the sandbox sees no tool credentials in v1.
  [#50](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/50)

## See also

- [docs/ARCHITECTURE.md → Security invariants](./ARCHITECTURE.md#security-invariants)
  — the design-level invariants this model rests on.
- [docs/DIVERGENCES.md](./DIVERGENCES.md) — the single registry of deliberate
  divergences from the reference, including the egress, issuance, and K8s-fidelity
  entries cited above.
