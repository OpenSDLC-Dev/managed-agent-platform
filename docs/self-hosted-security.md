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
| **Sandbox image** | Requires `/bin/bash` + a POSIX userland, and wants a `stat` accepting `-c`; pulls the image you name | Building and pinning a hardened, minimal image; keeping it patched |
| **Resource limits** | **By default** caps every sandbox at 2 CPUs (`SANDBOX_CPU_MILLIS`) and every **Docker** sandbox at 512 processes (`SANDBOX_PIDS_LIMIT`); an optional memory cap, plus an optional **Kubernetes-only** disk cap (`SANDBOX_EPHEMERAL_STORAGE_BYTES`, enforced by evicting the pod). The gap runs both ways: Kubernetes has no per-pod process limit to set, so `SANDBOX_PIDS_LIMIT` does nothing there, and whether a Docker daemon enforces a disk quota depends on its storage driver, so the disk cap does nothing there rather than sometimes-nothing | Tuning the caps; on Kubernetes, the kubelet's `podPidsLimit`; on Docker, bounding the disk at the host |
| **Non-root execution** | Runs the image's default user, or the uid `SANDBOX_RUN_AS_USER` names | Shipping an image whose default user is unprivileged, and whose workdir that user can reach |
| **Linux capabilities** | **By default** drops `NET_RAW`/`SETUID`/`SETGID` from every sandbox and forbids privilege escalation (`SANDBOX_CAP_DROP`, `ALL` accepted); a **gated** sandbox drops those three whatever the config says — the `NET_ADMIN` holders are the gate container/sidecar and the K8s netsetup init container, below | Widening or narrowing the drop set; AppArmor/SELinux profiles |
| **Syscall filtering** | On **Kubernetes**, sets `seccompProfile: RuntimeDefault` on every sandbox pod, always — covering the sandbox container, the gate sidecar and the netsetup init container. Not configurable, and it is the runtime's own curated filter, not one the platform authors. Docker containers already receive their runtime's default | Not disabling it out of band (a node whose runtime has no default profile refuses the pod); AppArmor/SELinux, which are still yours |
| **Read-only root filesystem** | Set on request (`SANDBOX_READONLY_ROOTFS`), with writable mounts arranged over every path the platform itself writes (workdir, `/tmp`, the shell state root, the file-resource mount root) | Deciding to turn it on, and shipping an image that tolerates one |
| **Sandbox egress** | `limited` = only `allowed_hosts`, through the per-session egress gate (both backends, executor opt-in); without the gate `limited` **fails closed** (no route out); default networking is unrestricted | Firewalling / `NetworkPolicy` for the default (non-`limited`) case |
| **Runtime isolation** | Sets `runtimeClassName` on sandbox pods (`SANDBOX_K8S_RUNTIME_CLASS`; the chart's `sandboxRuntimeClass`) | Running gVisor/Kata on the nodes and naming it; on Docker, a daemon-level runtime or userns-remap |
| **Sandbox placement** | On **Kubernetes**, puts your `nodeSelector` and `tolerations` on every sandbox pod (`SANDBOX_K8S_NODE_SELECTOR` / `SANDBOX_K8S_TOLERATIONS`; the chart's `sandboxPlacement`), and refuses a malformed one at startup | Building the node pool, labelling and tainting it, and keeping the platform's own workloads off it |
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
  exists to record. Tool-time credentials (vaults) never enter the sandbox
  either: the sandbox sees only an opaque `vltph_` placeholder, and the
  per-session egress gate substitutes the real value on admitted plain-HTTP
  egress alone — in-sandbox HTTPS keeps its placeholders until the
  TLS-terminating phase ([#166](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/166)).
  Live on both gate-wired backends; BYOC delivery is a
  [Reserved seam](#reserved-seams-and-tracked-gaps).

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

- **`limited` networking is enforced.** A `limited` environment means "only the
  allowed hosts". With the egress gate opted in (`CONTROLPLANE_URL` +
  `EXECUTOR_GATE_IMAGE` on the executor — the stock compose stack ships this;
  Helm's `executor.gateImage` wires the same pair) that is enforced literally
  on both backends: the sandbox shares a network namespace with its
  per-session gate (on Docker a paired container the sandbox's netns joins, on
  Kubernetes a native sidecar in the sandbox pod whose startup probe blocks
  the sandbox container until the firewall is verified), egress rides the
  injected `HTTP(S)_PROXY` through it, the gate admits CONNECT and plain-HTTP
  requests only for the environment's `allowed_hosts`, and an iptables
  **owner-match** ruleset lets only the gate's dropped-to UID leave the
  namespace — traffic that ignores the proxy is dropped, not routed. The
  ruleset lives in a chain the gate owns (`MAP-GATE-EGRESS`), jumped to first
  from `OUTPUT`, applied by *reconciling* rather than flushing — pre-existing
  rules (a CNI's or service mesh's, in a shared Kubernetes pod netns) survive
  below the jump, where the chain's terminal verdicts make them unreachable.
  Where the gate does not run, the platform still refuses to guess: an
  un-opted-in `limited` Docker sandbox gets `NetworkMode: none` (no route out
  at all), and an un-opted-in `limited` Kubernetes sandbox gets an init
  container that flushes the pod netns routing table and **fails the pod** if
  any IPv4 route survives.
  It never silently falls open to unrestricted egress. (The K8s flush is not
  equivalent to `NetworkMode: none` on every cluster: raw `AF_PACKET` sockets
  can still reach the segment, and only the main IPv4 table is inspected, so
  policy-routing CNIs and IPv6 egress want the gate opt-in, whose owner-match
  DROP covers both families and does not depend on routes.)

- **The container is the boundary.** Tools run inside the per-session sandbox
  with no host filesystem access. The built-in file and search tools do **no**
  lexical path confinement — an absolute path or glob is accepted — because the
  container itself is the wall, and a lexical check a `bash` call could walk
  around would be theatre. This is only sound because the sandbox is genuinely
  isolated; hardening that isolation (below) is therefore load-bearing, not
  optional polish.

- **A sandbox is capped when it is created.** Every sandbox gets cgroup limits
  and capability drops at provision time, with no operator action:
  512 processes, 2 CPUs, and `NET_RAW`/`SETUID`/`SETGID` dropped with privilege
  escalation forbidden. This is not tidiness — the exec deadline kills a
  timed-out command by SIGKILLing its process *group*, so a child that calls
  `setsid` escapes the kill and outlives the deadline, and abandoning the exec's
  stream is not a kill primitive. The cgroup limits are the containment for
  exactly that process, and the cap on the process pressure a command would need
  to stall the daemon probe the deadline labels an overrun with. Every value is
  configurable (`SANDBOX_*`, §2–4 below), including off, and a malformed one
  fails startup rather than silently reverting to the default. One asymmetry:
  the process cap is Docker-only, because Kubernetes has no per-pod process
  limit ([docs/DIVERGENCES.md](./DIVERGENCES.md)).

## What you own

Self-hosting means you supply the sandbox image, run the container runtime, and
place the deployment on your network. The platform sets what it can at provision
time (§2–4 are configuration, not seams); what remains below lives at those
layers, where only you can decide it.

### 1. Sandbox image hardening

The sandbox image is **your** choice, not baked into the platform: the executor
and worker launch whatever image `EXECUTOR_IMAGE` / `WORKER_IMAGE` names, defaulting
to `debian:stable-slim` for local development (`cmd/executor/main.go`,
`cmd/worker/main.go`). The contract the platform imposes is a POSIX userland with
`/bin/bash`. The **Kubernetes** backend needs more, and needs it hard: `setsid`
for its exec wrapper, `tee`/`wc` for the write path's delivered-byte count, a
`stat` accepting `-c` (GNU or BusyBox), on which every file **read** exits, and
`tar`, which it extracts a bulk write's archive with inside the pod — an image
without one loses skill materialization outright, where Docker hands the same
archive to the daemon and needs nothing (#206). `internal/sandbox/k8s/client.go`
is the exact list. On **Docker** that same `stat` is only wanted, not required
(below). The `grep` built-in expects GNU
grep/coreutils — a busybox-only image gets a clear tool error, not degraded
behaviour.

Two things do degrade silently rather than fail, both about file **modes** and
neither about the correctness of a file's contents. A write preserves the target's
permission bits by reading them with `stat -c %a` (so a script stays executable
across an edit); where that cannot run — a Docker image whose `stat` does not
accept `-c` — the file is written `0644` instead, as every write did before this
existed. And on Docker the temporary file a write lands under is extracted by the
daemon rather than created by the sandbox user, so an image whose default user is
**not** root cannot re-apply the mode and also gets `0644`, where Kubernetes
preserves it
([#209](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/209)). If you
run a non-root image (recommended below) and depend on the executable bit surviving
a rewrite, that is the case to know about.

A file a write **creates** has no bits to preserve, and lands `0644` on both
backends — the platform's answer rather than the image's: Docker's tar header fixes
it, and the Kubernetes write script sets `umask 022` before creating the file
([#212](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/212)). That
path is not only the agent's `write`/`edit` tools — session-resource mounts,
uploaded skill files and the persistent shell's own state files all land through
it — so an image whose default umask is tighter does **not** make any of them
`0600`; a `chmod` inside the sandbox does, and a later rewrite then preserves it.
Any umask whose **write** bits differ from `022`'s moves, not only a tighter one
(the execute bits are inert — a file create asks for `0666`): a group-oriented `007`
image landed `0660` and now lands `0644`, which **adds** other-read as it drops
group-write. The **directories** on the way there are not covered: both backends
`mkdir -p` them under the image's umask, so a hardened image still gets `0700`
parents and its directory-level gating stands — which is where to put the
protection if you were relying on the file bits. A **default POSIX ACL** on a
directory does not move a created file's own bits either, on either backend: the
kernel would take them from the ACL and ignore the umask, so the Kubernetes write
script chmods the file it creates to `0644` outright rather than only lowering what
a create asks for, and a bulk write — the path every uploaded skill lands through —
chmods its delivered members the same way before renaming them
([#213](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/213)). The
batch needed it because a `tar` extracting as a **non-root** sandbox user does not
restore the header's mode, where a root one and Docker's daemon-side untar both do.

What a default ACL *does* still decide is the **directories** — which is the same
fallback the paragraph above points you at, so read the two together. A directory
the write or the batch creates inherits its parent's default ACL and takes the
ACL's bits rather than the umask's, on both backends and all the way down the tree
as the ACL propagates: measured, under `setfacl -d -m u::rwx,g::rwx,o::rwx` a
hardened `077` image gets `0777` parents, not the `0700` the umask alone would have
given. So where a default ACL is in play, set it to what you want those directories
to be; the image's umask will not answer for them.

For production, build a minimal, pinned image: only the interpreters and tools
your agents actually need, a non-root default user (below), no build toolchain or
package manager left in the final layer, and a patch cadence you control. The
platform will pull and run exactly what you specify — its security is your
responsibility.

### 2. Non-root execution

The sandbox runs as **your image's default user** unless you name a uid:
`SANDBOX_RUN_AS_USER=10001` sets it on both backends (Docker's container `User`,
Kubernetes' `securityContext.runAsUser`). It is numeric because that is all both
backends can express — a Kubernetes securityContext takes no user name.
`debian:stable-slim` defaults to root; a hardened image does not have to.

**The platform runs nothing in your container as anyone but that user** — no
privileged exec, anywhere, and one place had to be designed around to keep it
that way. On Docker the daemon extracts a write's archive as root, so when the
rename that would finish the write is refused, the temporary is a root-owned
file your sandbox uid cannot unlink from a directory it cannot write (#310).
Cleaning it up with a `docker exec -u 0` was tried and abandoned: such an exec
starts with `AT_SECURE=0` and runs a binary and libraries your *image* supplies,
so what it really does is decided by the image — and where an agent can write
part of that image's filesystem, by the agent. Four channels were found and
measured before the approach was dropped: `bash -c` sourcing an `ENV BASH_ENV`
file, the loader honouring `ENV LD_PRELOAD`, `ENV LD_DEBUG_OUTPUT` writing
root-owned files at a path the image names, and `/etc/ld.so.preload`, which no
environment setting can neutralize. So the daemon takes back what it landed
instead: the same archive endpoint that extracted the temporary extracts an
empty file over it, executing nothing. That covers every failed write whose
temporary the daemon landed, a transfer that died part way included — there the
residue is a partial payload rather than an empty name. The refused payload is
gone; where your sandbox cannot unlink, an empty file under the platform's own
`.map-write-` name remains until the container is destroyed.

The catch is the **workdir**. The container's entrypoint runs `mkdir -p
<workdir>` as whatever user the container runs as, and a uid the image did not
plan for usually cannot create a directory at the root of the filesystem — so the
sandbox dies at startup. Two ways out, and the first is still the better one:

- **Ship the workdir in the image**, owned by an unprivileged default user (a
  `USER 10001` layer plus a `chown`ed `/workspace`). This is the one hardening
  dimension you can fully own through the image alone, with no platform
  configuration at all — so do it.
- **Turn on the read-only root filesystem** (§4) alongside the uid. It mounts
  writable space over the workdir, so the directory exists whatever the uid.
  On Kubernetes the kubelet creates that `emptyDir` world-writable, so the uid
  can write it too; on Docker the mount is an anonymous volume, which inherits
  the image directory's ownership when the image ships one and is otherwise
  root-owned — so on **Docker the image still decides** whether a non-root uid
  can write the workdir.

### 3. Capability drops and cgroup limits

The platform drops capabilities from **every** sandbox, not only a gated one:
`NET_RAW`, `SETUID` and `SETGID` by default, with privilege escalation forbidden
alongside them (Docker `no-new-privileges`, Kubernetes
`allowPrivilegeEscalation: false`) so a setuid binary cannot hand a dropped
capability straight back. `SANDBOX_CAP_DROP` changes the set: a comma-separated
list of bare capability names, `ALL` to drop everything, `none` to drop nothing
and run with the runtime's default set. A name that is not a Linux capability
fails the executor's (or worker's) startup rather than every container create, so
a typo is a process that will not start, not a deployment that starts and cannot
provision a session.

What the default costs, so it is not a surprise: a tool that changes uid loses
the ability. `apt-get` is the one worth knowing — it warns that it cannot drop
privileges for downloading and continues as root; `su` and `sudo` fail outright.

A **gated** sandbox (plan 12) drops those same three whatever `SANDBOX_CAP_DROP`
says — they are what keeps a tool from crafting raw packets past the gate's
owner-match firewall or becoming the gate's UID *at runtime*, so they are not
configurable away. One caveat the drops cannot cover: a sandbox **image** whose
`USER` directive is already the gate's dropped-to UID (`GATE_UID`, default 65532
— notably distroless's `nonroot` user) starts every tool process as the UID the
owner-match firewall permits, silently bypassing `allowed_hosts` on both
backends. Do not use sandbox images that run as the configured gate UID;
enforcement for the *image* is tracked in #196. The half the platform itself
opens is closed: `SANDBOX_RUN_AS_USER` set to the gate's uid on a gated session
**fails the provision** on both backends, rather than starting a sandbox whose
egress policy is silently void. The `NET_ADMIN` holders are the gate
container/sidecar itself (it installs the firewall, then drops privileges) and,
on Kubernetes, the short-lived `netsetup` init container that enforces gate-less
`limited` networking — never the sandbox container.

Alongside the drops, cgroup limits — the containment for a process that escaped
the exec deadline's process-group kill:

| Variable | Default | Docker | Kubernetes |
|---|---|---|---|
| `SANDBOX_PIDS_LIMIT` | `512` | `HostConfig.PidsLimit` | **not expressible** — see below |
| `SANDBOX_CPU_MILLIS` | `2000` (2 CPUs) | `HostConfig.NanoCpus` | `resources.limits.cpu`, with a 100m request so a limit does not become a per-pod reservation |
| `SANDBOX_MEMORY_BYTES` | off | `HostConfig.Memory` | `resources.limits.memory` (request = limit) |
| `SANDBOX_EPHEMERAL_STORAGE_BYTES` | off | **ignored** — storage-driver dependent, see below | `resources.limits.ephemeral-storage` (request = limit) |

`0` turns one off. A malformed value fails executor/worker startup rather than
falling back to the default — a deployment that meant to cap a sandbox must not
run believing it had.

One row there is not a cgroup limit despite the heading, and behaves unlike the
rest. `SANDBOX_EPHEMERAL_STORAGE_BYTES` caps the node-local disk a sandbox may
consume — its writable layer and every `emptyDir` mounted over it — and
Kubernetes enforces it by **eviction**, not by refusing the write: a tool that
fills the disk does not get `ENOSPC`, the kubelet notices the pod is over its
allowance and kills the whole pod, ending the session's sandbox mid-call.

It enforces it only where it can measure it, which is a property of your nodes
rather than of this setting. The kubelet measures local ephemeral storage on the
three node layouts it supports — a single filesystem, a separate runtime
filesystem, or a split image filesystem — and on any other layout it *"does not
apply resource limits for ephemeral local storage"*: the pod accepts the fields
and can exceed them without ever being evicted. Check your nodes' layout before
treating this as a bound. What still fires there is node-pressure eviction — a node short
on local storage stops accepting new pods and the kubelet starts terminating
running ones to reclaim space, chosen by usage against their requests and by
priority, never by which pod filled the disk — which is the arbitrary-victim
outcome this cap exists to replace. That
is why this cap is off by default where the CPU cap is not, and why the request
is set equal to the limit — the kubelet ranks eviction candidates by usage
against the *request*, so a limit the scheduler never reserved would push the
enforcement onto whichever pod is over its own. Set it in **bytes**:
`21474836480`, never `20Gi` — a Kubernetes quantity string is a malformed value
like any other and fails startup.

Two runtime-shaped edges are worth knowing before you tune these, both Docker's:

- The daemon refuses a container whose CPU cap exceeds the host's CPU count, so
  the provider **clamps** the cap to what the daemon reports, and logs when it
  does. Without that the two-CPU default would fail every provision on a
  one-CPU host — per session, not at startup. Kubernetes has no such ceiling: a
  limit above the node's capacity simply never throttles.
- The daemon's minimum memory limit is 6 MB, so a smaller `SANDBOX_MEMORY_BYTES`
  is refused at provision. The knob is off by default, so only setting it
  reaches this.

What is still yours at the runtime layer:

- **A process cap on Kubernetes.** The Pod API has no per-pod pids limit; it is
  the kubelet's `podPidsLimit` node setting, so it is node configuration, not
  chart or platform configuration
  ([docs/DIVERGENCES.md](./DIVERGENCES.md)).
- **A disk cap on Docker.** The Engine API takes a writable-layer quota, but
  whether it means anything depends on the daemon's storage driver, and the
  daemons disagree: btrfs, zfs and overlay2 over XFS with `pquota` can enforce
  it, classic overlay2 without `pquota` refuses the option outright, and Docker
  Desktop's `overlayfs` driver accepts it and enforces nothing (measured — the
  container's rootfs still reports the whole host filesystem). Passing it
  through blindly would therefore break provisioning on one daemon and report a
  cap that does not exist on another, so the Docker backend ignores
  `SANDBOX_EPHEMERAL_STORAGE_BYTES` and warns once in the executor's log
  ([docs/DIVERGENCES.md](./DIVERGENCES.md)). Bound the disk at the host — a
  dedicated filesystem or volume behind the daemon's data root — or, if your
  daemon is one of the drivers that can enforce a quota, at the daemon's own
  default (`dockerd --storage-opt`).
- **AppArmor/SELinux.** The platform authors no profile for either; keep the
  runtime's defaults enabled, and never run sandboxes `--privileged` or with
  `--security-opt seccomp=unconfined`. **Seccomp is no longer in this list on
  Kubernetes** — see the *Syscall filtering* row in the shared-responsibility
  table above.
- **Pod Security Admission.** A namespace **enforcing** `restricted`
  (`pod-security.kubernetes.io/enforce=restricted`) still rejects the sandbox
  pod, in every configuration; the `audit` and `warn` modes admit it and record
  or surface the violation instead. Two of the three reasons it
  used to reject an *unrestricted* pod are now closed — `SANDBOX_CAP_DROP=ALL`
  makes `capabilities.drop` contain `ALL`, and the pod carries a
  `seccompProfile` of the shape `restricted` accepts — leaving one:
  `runAsNonRoot: true`, a *different* field from `SANDBOX_RUN_AS_USER`, which
  the provider does not set.
  <br>That accounting covers the sandbox container of a pod that has no init
  container at all. **`restricted` evaluates every container in the pod, init
  containers included**, and every pod that carries one fails outright on it:
  the gate sidecar, or the `netsetup` init container. Both *add* `NET_ADMIN`
  (which `restricted` permits only for `NET_BIND_SERVICE`), neither drops
  `ALL`, and neither sets `allowPrivilegeEscalation: false`.
  <br>Which pods those are is wider than the networking type suggests, and is
  the boundary to get right: the gate rides **`limited` networking *or* any
  attached vault**, so a vault-attached *unrestricted* session gets the sidecar
  too. A `limited` session without a gate configured gets `netsetup` instead.
  The only shape left with no init container is unrestricted **and**
  vault-less — and that one is still rejected for `runAsNonRoot`. So no
  configuration produces a `restricted`-acceptable sandbox pod, and no amount
  of `SANDBOX_*` tuning changes it.

### 4. Read-only root filesystem

`SANDBOX_READONLY_ROOTFS=true` mounts the sandbox's root filesystem read-only on
both backends and — the part that used to make this a runtime-level change
rather than a flag — arranges the writable space the sandbox still needs itself.
That set is every path the platform writes inside a sandbox, and it is one list
in the code (`sandbox.WritablePaths`) precisely so neither backend can forget one:

- the **session workdir**;
- **`/tmp`**, where the Kubernetes backend keeps each exec's state file;
- **`/var/lib/map-shell`**, where the persistent shell keeps each session's cwd
  and environment — without it the *first* `bash` call of every session fails,
  and fails as a backend fault rather than an answer the model can see;
- **`/mnt`**, the parent of the default mount point for a session's file
  resources (`/mnt/session/uploads/<file_id>`).

Kubernetes mounts an `emptyDir` over each; Docker an anonymous volume, which its
existing container removal takes away with the container.

Docker uses a volume rather than a `tmpfs` deliberately, and the reason is
measured rather than stylistic: the daemon refuses `PUT
/containers/{id}/archive` on a read-only-rootfs container when the destination is
a tmpfs (`container rootfs is marked read-only`) and allows it when the
destination resolves into a volume. Every file that backend writes goes through
that endpoint, so a tmpfs workdir would give you a sandbox that runs commands but
can never receive a file — no skills, no files, no `write` tool.

What is still yours: an **image that tolerates a read-only root elsewhere**. A
tool that writes outside the set above — a package manager, a language runtime
with a cache under `$HOME` — fails. That is the "where the image allows" half of
this dimension, and only you can judge it. One platform-shaped case falls the
same way: a session file resource given an **explicit `mount_path`** outside that
set cannot be materialized under a read-only root (it is logged, and the session
continues without the file), so leave `mount_path` unset or put it under the
workdir.

### Rolling out a change to any of this

Containment is bound when a sandbox is **created**, exactly as `Env` and the
networking mode are. `Provision` is idempotent: it adopts a session's existing
container or pod rather than replacing it — but what adoption does with a
changed setting depends on which setting changed. Hardening and `Env` are
**silently kept**: a session that was already running when you rolled the
executor keeps the containment and environment it was created with until its
sandbox is destroyed, and new sessions get the new settings immediately. This
is deliberate — replacing a live session's sandbox to apply a setting would
discard the workdir, its uploaded file resources and the persistent shell's
state mid-task, which is a worse outcome than an already-running session
finishing under the containment it started with.

The networking mode, the sandbox image, and the workdir are different: both
backends **refuse** to adopt a sandbox whose fixed-at-create value no longer
matches the request (`ErrSpecMismatch`, #29 docker / #296 k8s), because
silently keeping, say, an open-egress container under a session that now asks
for `limited` would be a containment lie. The refusal deletes nothing — the
stale sandbox stays, and every tool call for that session fails for as long
as the request keeps carrying the new value: the check is per-request, so
rolling the setting back makes the sandbox adoptable again with its state
intact, and otherwise it must be removed by hand (`docker rm` /
`kubectl delete pod`). The
platform's automatic teardowns target broken sandboxes — a wedged gated pod,
a gate-shape change — never a healthy one that merely mismatches, so nothing
will clear a refused sandbox for you. So for these three
settings, drain the running sessions **before** the roll — a gate-less
networking flip or an executor image/workdir roll turns every still-running
session's next tool call into an error, not a quiet downgrade. For a
configuration change in the silently-kept group, draining before or after the
roll both work.

### 5. Egress restriction

For a `limited` environment, egress is already enforced by the platform
(above) — only `allowed_hosts` through the per-session gate where it runs,
no-route-out elsewhere, with the CNI caveats noted. **For the default
(non-`limited`) case, egress is unrestricted**: a default Docker sandbox
gets `NetworkMode: bridge`, and the Kubernetes sandbox pod carries no
`NetworkPolicy`. If your agents should not reach the open internet or your
internal network, you must restrict it yourself:

- **Kubernetes:** apply a default-deny egress `NetworkPolicy` (plus explicit
  allows) to the namespace the sandbox pods run in. The platform ships none.
- **Docker / hosts:** firewall the sandbox network, or front outbound traffic
  with an egress proxy you control.

On gate-wired deployments (either backend), `allowed_hosts` per-host
allowlisting for `limited` environments is enforced in-platform (above).
Everywhere else — un-opted-in deployments and every non-`limited` environment —
network-layer controls remain the mechanism. The built-in `web_fetch` /
`web_search` tools are the deliberate exception: their egress originates in the
**executor process**, not the sandbox, and deliberately not through the gate —
the reference documents that the environment's networking policy does not
govern these tools — so firewalling the sandbox network never constrains them;
bound them with `WEBTOOL_ALLOWED_DOMAINS` on the executor (a comma-separated
allowlist in the wire's allowed_hosts grammar, entries validated at startup; a
bare entry admits only the apex and `*.example.com` only subdomains — list
both to get both —
`web_fetch` refuses hosts outside it and search hits from outside it are
dropped; note it judges the URL the model *names*, not where the reader
backend's server-side fetch or an allowed host's redirect ultimately lands),
at the executor's own network, or by the backend endpoints you configure
(`WEBSEARCH_BASE_URL`/`WEBFETCH_BASE_URL` — point them at a proxy you control)
([#47](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/47),
[#225](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/225)).

### 6. Environment-key rotation

The platform owns the *primitive*; you own the *lifecycle*. `EnsureEnvironmentKey`
makes a supplied value the one live worker credential for an environment: it
stores only the hash, and registering a fresh value **revokes the prior one**
(rotation-by-re-mint). A key value is bound to one environment for life; it is
never silently re-pointed. The schema enforces the invariant rather than trusting
the helper: `environment_keys_one_live` (migration 0013) admits one unrevoked row
per environment, so a second live credential — from concurrent mints, or from a
hand-written `INSERT` — is rejected outright. There is **no expiry or TTL** — a
key is live until re-minted or revoked — and there is **no automatic rotation**.

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
OpenBao/Vault's own storage (`SECRETS_BACKEND=openbao`, the transit engine), in
Cloud KMS (`SECRETS_BACKEND=gcpkms`, where it never leaves the service at all), or
in the `SECRETS_MASTER_KEY` you configured (`SECRETS_BACKEND=local`). That split is
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
- **Under `gcpkms` the pairing is the same shape with a sharper edge.** There is
  no key material to back up and none in the release — authentication is Workload
  Identity, and the CryptoKey resource name is not a secret — but what replaces
  a lost key file is a lost key *version*, and the two failure modes are not the
  same. **Disabling** a CryptoKeyVersion is a reversible outage: decryption fails
  until you re-enable it. **Destroying** one is not — after the
  scheduled-destruction window elapses the version reaches `DESTROYED`, its key
  material is gone, and every credential sealed under it is unrecoverable. Note
  what the boundary is NOT: Cloud KMS does not let you delete a key ring or a
  CryptoKey at all, so no `terraform destroy` can take them; only a version
  destruction can, which is why the scheduled-destruction window is the thing to
  watch and why the key belongs **outside** the lifecycle of anything you rebuild
  routinely. One
  behavioural consequence to know before choosing it: KMS's raw `Encrypt` bounds
  plaintext where OpenBao's transit engine does not, so a credential whose sealed
  secrets exceed the bound is refused with a `400` naming the limit rather than
  stored (docs/DIVERGENCES.md). The bound is the key's, not a constant — 65536
  bytes for a software-protected key and 8192 for an HSM one — and the platform
  reads it from the key at startup.

### Host and runtime isolation

The sandbox runs untrusted, model-directed commands, so the strength of the
container boundary is the strength of your isolation. The platform does not pin a
hardened runtime, so choose one: a sandboxing runtime such as **gVisor** or
**Kata Containers**, or at minimum user-namespace remapping alongside the
hardening in §2–4.

On **Kubernetes** this is a `RuntimeClass` decision, and the platform wires it:
`SANDBOX_K8S_RUNTIME_CLASS` puts a `runtimeClassName` on every sandbox pod, and
the Helm chart exposes it as `sandboxRuntimeClass.name` — with an opt-in
`sandboxRuntimeClass.create` that also creates the cluster-scoped `RuntimeClass`
object. Naming a RuntimeClass the cluster does not define makes the kubelet
refuse the pod, which is the fail-closed direction. What remains yours is running
the handler on the nodes (gVisor's `runsc`, Kata's) — nothing the chart can do
for you. On **Docker** there is no per-container equivalent: the runtime is a
daemon-level choice (`--default-runtime`, or userns-remap).

One combination to test before you rely on it: the RuntimeClass applies to the
whole sandbox pod, **including a gated session's gate sidecar**, which holds
`CAP_NET_ADMIN` and installs an iptables owner-match ruleset. gVisor's netfilter
support is partial, so a gate under `runsc` is not something this platform has
verified — run a `limited` session end to end on your own cluster before
combining the two.

### Sandbox node placement (Kubernetes)

A hardened runtime bounds what a sandbox can do on its node. Which node it lands
on is a separate question, and on a shared cluster it is the one that decides
what a container escape reaches: a sandbox scheduled beside the control plane, a
database, or a CI runner is a sandbox one bug away from them.

The platform puts your placement on every sandbox pod —
`SANDBOX_K8S_NODE_SELECTOR` (comma-separated `key=value`) and
`SANDBOX_K8S_TOLERATIONS` (a JSON array of Kubernetes `Toleration` objects), the
chart's `sandboxPlacement.nodeSelector` / `.tolerations`, which take ordinary
Kubernetes shapes and encode them for you. Empty applies nothing, which is the
default and today's behaviour.

Building the pool is yours, and the isolating configuration is the **pair**: a
node pool labelled so the selector finds it, **and** tainted so other workloads
do not land there. A label alone keeps sandboxes on the pool but does not keep
anything else off it; a taint alone keeps others off but then needs the matching
toleration here, or the sandboxes cannot schedule onto the pool either. Note what
a taint is and is not: it repels pods that carry no matching toleration, so a
workload with a cluster-wide wildcard toleration still lands on the pool. Keeping
those off it is yours as much as the taint is. Keep the platform's own Deployments elsewhere — the chart's
`executor.nodeSelector`/`executor.tolerations` place the executor, and are
deliberately a different setting from `sandboxPlacement`.

Both values are parsed when the executor (or BYOC worker) starts, against the
rules the API server itself applies at pod-create time, and a value that would
fail there **fails that startup** instead — an ill-formed selector entry, a label
key or value outside the syntax, a toleration the pod-create validator refuses.
Left to the pod, each of those fails every session's Provision for the life of
the deployment rather than once at boot.

Three boundaries on that, stated rather than left to be discovered. A
*well-formed* selector naming a label no node carries is accepted: its pods stay
`Pending`, and only the cluster could have answered, which the parse runs too
early to ask. The `Lt` and `Gt` toleration operators are accepted although a
cluster without the alpha `TaintTolerationComparisonOperators` feature gate — the
default, including GKE — refuses them at pod create; they are real fields of the
pinned Kubernetes type, so refusing them here would break a cluster that turns
the gate on. Their *values* are held to the server's rule even so: a canonical decimal
integer that fits in 64 bits, so `5`, `0` and `-5` pass while `0100`, `+5` and
`-0` do not. And placement binds when a sandbox pod is **created** — like `RuntimeClass` and the
`SANDBOX_*` containment, it is not re-applied to a pod the executor adopts, so
sandboxes already running when you enable a pool stay where they are until their
sessions end.

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

- **`web_fetch` / `web_search` egress** — implemented (executor-process
  execution, deliberately outside the session gate — see §5 above), and
  in-platform bounding now exists: `WEBTOOL_ALLOWED_DOMAINS` (#225) fences the
  URL the model *names*, with entries validated at startup. What it cannot
  judge is where the reader backend's own fetch lands — a remote reader
  resolves the target and any redirects server-side (our adapters refuse to
  follow 3xx themselves), so an open redirect on an allowed host can still
  return a fenced-off domain's content. Network-layer controls at the executor
  remain the hard boundary.
  [#47](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/47),
  [#225](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/225)
- **Environment-key issuance UX** — no operator wire endpoint yet; keys are seeded
  directly.
  [#43](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/43)
- **Sandbox `securityContext` / `runtimeClassName`** — **closed (#65).** The
  platform now sets cgroup limits and capability drops on every sandbox by
  default, and non-root, a read-only root filesystem and a hardened
  `runtimeClassName` on request (§2–4 above), and on Kubernetes a
  `seccompProfile` of `RuntimeDefault` on every sandbox pod (plan 20 slice 2 —
  the runtime's own filter, selected by the platform; it still authors none).
  Three limits remain, stated rather than papered over: Kubernetes has no
  per-pod process limit to set (the kubelet's `podPidsLimit`), Docker has no
  disk quota the platform can rely on across storage drivers (so the opt-in
  `SANDBOX_EPHEMERAL_STORAGE_BYTES` is Kubernetes-only), and AppArmor/SELinux
  profiles are still the runtime's defaults.
- **Tool-time credential injection (vaults)** — delivered on both gate-wired
  backends: the sandbox sees only opaque placeholders, substituted at the gate
  on admitted plain-HTTP egress (in-sandbox HTTPS keeps its placeholders until
  the TLS-terminating phase,
  [#166](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/166)).
  BYOC worker delivery is
  [#165](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/165).

## See also

- [docs/ARCHITECTURE.md → Security invariants](./ARCHITECTURE.md#security-invariants)
  — the design-level invariants this model rests on.
- [docs/DIVERGENCES.md](./DIVERGENCES.md) — the single registry of deliberate
  divergences from the reference, including the egress, issuance, and K8s-fidelity
  entries cited above.
