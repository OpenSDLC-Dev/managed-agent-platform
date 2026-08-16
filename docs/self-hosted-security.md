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
| **Sandbox egress** | `limited` = `allowed_hosts`, plus the `host:port` endpoints the session's agent declares MCP servers at when the environment sets `allow_mcp_servers`, through the per-session egress gate (both backends, executor opt-in); without the gate `limited` **fails closed** (no route out); default networking is unrestricted | Firewalling / `NetworkPolicy` for the default (non-`limited`) case |
| **Runtime isolation** | Sets `runtimeClassName` on sandbox pods (`SANDBOX_K8S_RUNTIME_CLASS`; the chart's `sandboxRuntimeClass`) | Running gVisor/Kata on the nodes and naming it; on Docker, a daemon-level runtime or userns-remap |
| **Sandbox placement** | On **Kubernetes**, puts your `nodeSelector` and `tolerations` on every sandbox pod (`SANDBOX_K8S_NODE_SELECTOR` / `SANDBOX_K8S_TOLERATIONS`; the chart's `sandboxPlacement`), and refuses a malformed one at startup | Building the node pool, labelling and tainting it, and keeping the platform's own workloads off it |
| **Environment-key lifecycle** | Server-generated secrets, hash-only storage, one key per host with a one-year expiry, individual revocation, per-environment scope | Provisioning keys, rotation cadence, transport secrecy |
| **Management-key lifecycle** | Server-generated secrets, hash-only storage, a masked hint as the only surviving trace; a reversible `inactive` and a permanent `archived`; an expiry the caller sets and the database's clock enforces, derived and never stored; rotation-by-restart for `CONTROLPLANE_API_KEY` | Choosing an expiry at all (absent means never), one key per consumer, rotation cadence, and configuring SSO if "which human issued this" must have an answer |
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
  `list`/`delete` on `pods` — `list` is the reaper's enumeration of owned pods
  by label (plan 24) — and `create` on `pods/exec`, nothing cluster-wide;
  `deploy/helm/managed-agent-platform/templates/executor-rbac.yaml`). One
  consequence to place deliberately: the ownership label is what authorizes the
  reap — `Reap` deletes every pod in the namespace carrying the session's
  label, by design (a renamed or manually recreated pod is still the session's
  to reap). Within the executor's namespace, authority to set pod labels is
  therefore authority to have a pod deleted; the namespace is a single trust
  domain, and a principal who may patch labels there but must not delete pods
  is a boundary this platform does not draw. On Docker
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
empty file over it, executing nothing. That covers the refused write #310 is
about and every other failed **single** write whose temporary the daemon landed,
a transfer that died part way included — there the residue is a partial payload
rather than an empty name. Where your sandbox cannot unlink, what stays behind
afterwards is an empty file under the platform's own `.map-write-` name, until
the container is destroyed.

The **bulk** write (skill materialization) is covered the same way, and used not
to be (#316). Its shed is your sandbox user's own `rm` — in the rename script and
in the discard pass alike — so under a parent that user cannot write a failed
batch left *every* member's full payload, not one file but up to ten thousand.
What held it back from being reachable was only the caller: the one batch the
platform writes goes under the workdir, which your sandbox user owns. It was
never the platform creating a batch's directories inside the container — that
uses `mkdir -p`, which leaves a root-owned directory that already exists exactly
as it found it, so an image shipping one under the workdir opened the gap. Both
sheds now report which of the batch's own files they could not remove, and the
daemon empties those in a single archive — one round trip for a whole batch, and
still executing nothing. A batch that *succeeds* is asked too: its last act is to
remove the two bookkeeping files, which the same root extraction landed in your
workdir.

The two limits below apply to it unchanged, and it has a third of its own. The
single write's emptying asks the daemon directly about a path the platform
generated; a batch's is steered by a report the shed computes **inside** your
container, from a manifest that lives in your workdir. So a command in the
sandbox can delete that manifest, and an image can print to the same stream the
report arrives on. Neither is left to trust — a shed that lost its manifest says
so, and the branch that knows the members were delivered empties the platform's
own list instead; and only the lines after the shed's own opening marker are
read, so an `ENV BASH_ENV` file that prints before the script does is not
mistaken for it. What remains is narrower: the batch's emptying needs the shed
exec to have *run*. A sandbox too broken to exec at all keeps a failed batch's
payload where a single write's would still be taken back — closing that would
cost a round trip per member, ten thousand of them, to serve a container that is
about to be destroyed anyway.

Two limits are worth knowing rather than discovering. The emptying is best
effort: it reports nothing of its own, and a daemon that will not answer leaves
the payload where it was — the same place the sandbox user could not reach it
either. And one branch does not ask for it at all: when the rename's *exec*
fails outright, the script it was running may still be mid-`mv`, and emptying
the temporary under a live `mv` would put zero bytes onto the file the caller was
just told it had not touched. Keeping a payload the container will take away is
the lesser harm than destroying data the write promised to leave alone, so that
branch unlinks with your sandbox user's own `rm` and stops there.

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
- **`/mnt`**, the parent of the mount point for a session's file resources
  (`/mnt/session/uploads/…`, and `/mnt/session/uploads/<file_id>` when the
  caller supplies no `mount_path`) — since #323 every resolved mount path is
  under it, not just the default-placed ones.

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
this dimension, and only you can judge it. One platform-shaped case used to fall
the same way and now mostly does not: since #323 every `mount_path` **is resolved
under `/mnt/session/uploads` at create/add time**, so a resource created from now
on always names a path inside `/mnt` — inside the writable set — instead of
wherever the caller spelled.

Two limits on that, both worth knowing before you rely on it. **It is not
retroactive:** resolution happens when the resource is created, and both
materialization halves write the *stored* value verbatim, so a session created
before the upgrade keeps a literal `mount_path` like `/workspace/in.txt` and
still fails to materialize under a read-only root (logged, and the session
continues without the file). There is deliberately no backfill — re-rooting a
live session's mount would move a file the agent's system prompt has already been
told about. **And containment here is lexical**, over the stored string: the
uploads directory is agent-writable, so a symlink a tool plants inside it can
still point a later mount's bytes elsewhere. That is the same accepted
single-tenant tampering residual the mount sentinel carries
([docs/DIVERGENCES.md](./DIVERGENCES.md)), not a boundary this resolution claims
to enforce against a hostile in-sandbox actor.

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
(above) — `allowed_hosts` through the per-session gate where it runs, and the
endpoints the agent declares MCP servers at if you set `allow_mcp_servers`;
no-route-out elsewhere, with the CNI caveats noted. `allow_mcp_servers` is the
one place an *agent author* rather than you widens a `limited` environment, so
it is scoped to the exact `host:port` each declaration names and a request only
it admits is still held to the platform's own address floor (no loopback, no
link-local, no cloud metadata) on the resolved address. **For the default
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

**MCP is the second egress path that does not leave the sandbox, and it is the
one most easily missed.** A session's MCP servers are dialled **from the
executor process**, with no sandbox involved, on `cloud` and `self_hosted`
environments alike — so a firewall or `NetworkPolicy` around the sandbox network
constrains none of it. `allow_mcp_servers` is what the environment's networking
policy is consulted for on that path: under a `limited` policy the executor
admits a declared server's host only if the flag is set (or the host is in
`allowed_hosts`), and it is asked only about servers the agent itself declared.
Read the asymmetry deliberately — a `self_hosted` environment carries no
networking block at all by construction, so on that kind the dial is not
policy-restricted here in the first place. What the platform guarantees
here is a floor and not a policy: every such dial is checked against the
resolved IP at connect time and refused for loopback, link-local (cloud
metadata included), unspecified and multicast addresses, and redirects are not
followed — but **RFC 1918 private ranges are deliberately allowed**, because
reaching an MCP server on your own private network is the on-prem deployment
model this platform exists for. So the reachable set for a declared MCP server
is your executor's network, and bounding it is yours: run the executor where its
egress is governed the way you want, and treat `allow_mcp_servers` on an
environment as the grant that it is.

### 6. Environment-key lifecycle

The platform owns the *primitive*; you own the *lifecycle*. `IssueEnvironmentKey`
mints a worker credential for an environment: the platform **generates** the
secret (256 bits of CSPRNG behind an `sk-map-env01-` prefix), returns it once,
and stores only its SHA-256 hash — nothing can render the value again, so a lost
key is reissued rather than recovered. Each key carries a **name** and expires
**one year** after issue, and an environment holds as many live keys as it has
hosts: issue one per host, and `RevokeEnvironmentKey` retires that host alone
without disturbing the others. Revocation is immediate (it takes effect on the
worker's next request), idempotent, and scoped to the owning environment — a key
id cannot be revoked, or even confirmed to exist, through another environment.
A key value is bound to one environment for life: `key_hash` is UNIQUE, so the
same secret can never authenticate two queues.

An **expired** key fails exactly as a revoked or unknown one does — the same 401
with the same message — so the auth lane leaks nothing about which it was. Keys
minted before expiries existed (before migration 0021) carry no expiry and stay
live until revoked; the migration deliberately does not backfill one, which
would have retro-expired credentials already in use. Treat those as a migration
debt rather than a supported state: such a key is a value **you** chose, so it
never had the platform's 256-bit generation guarantee, and it now never expires
either. Issue a replacement for each host and revoke the old key — the list
shows them with an empty name and no expiry, which is how you find them.

**Reissue them before you turn on single sign-on (§9), not after.** With OIDC
identity enabled, two different credentials arrive in the same `Authorization:
Bearer` header, and the platform tells them apart by shape: a compact-JWS
silhouette goes to the human lane, anything else stays a worker's environment
key. The predicate is exact — **three non-empty segments separated by two dots,
every byte of all three in the base64url alphabet** (`identity.LooksLikeJWT`) —
so most operator-chosen values are unaffected even if they contain dots. Every
key this platform minted is `sk-map-env01-` plus base64url and carries no dot
at all, so it can never be misread. A grandfathered key is the residual case:
if the value **you** chose happens to satisfy that predicate, it is routed to
the human lane and fails verification there. That is a 401 — fail-closed, never
an over-authorization — and it is what a worker that authenticated yesterday
would start returning the moment SSO goes live. Reissuing removes the case
entirely, and you cannot tell by eye which grandfathered values qualify.

What you own:

- **Provisioning.** Treat key creation as a privileged, audited operation. The
  operator surface is the console API — off the `/v1` wire, under
  `/api/oauth/organizations/default/environments/{environment_id}/tokens`, and
  authenticated with the management `x-api-key` like any other management call.
  The managed-agent-console drives it from an environment's page, so a key is
  generated, shown once and copied without anyone touching the control-plane
  database; a headless operator calls it directly:

  ```sh
  # issue — the access_token is the only copy that will ever exist
  curl -sX POST "$CONTROLPLANE/api/oauth/organizations/default/environments/$ENV/tokens" \
    -H "x-api-key: $MANAGEMENT_KEY" -H 'content-type: application/json' \
    -d '{"name":"build-host-1"}'
  # list (never shows a secret) and revoke one host's key
  curl -s "$CONTROLPLANE/api/oauth/organizations/default/environments/$ENV/tokens" \
    -H "x-api-key: $MANAGEMENT_KEY"
  curl -sX POST "$CONTROLPLANE/api/oauth/organizations/default/environments/$ENV/tokens/$KEY_ID/revoke" \
    -H "x-api-key: $MANAGEMENT_KEY"
  ```

  Only a `self_hosted` environment gets a key: a cloud environment's work is run
  by the platform's own executor, which holds no environment key, so issuing one
  there is refused rather than handing you a credential nothing can use. **This
  is a management-credential surface, not a separate permission tier** — anyone holding
  the management `x-api-key` can mint worker keys, so guard that key
  accordingly, and let the console's BFF hold it server-side rather than
  shipping it to a browser.
- **Rotation cadence.** The one-year expiry is a backstop, not a policy: rotate
  on your own schedule, and immediately on suspected worker compromise, by
  issuing a fresh key for that host, rolling it out to the worker's
  `ANTHROPIC_ENVIRONMENT_KEY`, and revoking the old one. Because keys are
  per-host, that is a rolling operation — the fleet's other workers keep running
  throughout, which the previous rotate-on-mint model could not offer.
- **Transport secrecy.** The key travels as a Bearer token — terminate TLS in
  front of the control plane, and keep the value out of images, logs, and shell
  history. (The management `x-api-key` is hashed at rest the same way, and has a
  lifecycle of its own — §10 below.)

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

### 8. Principal retention

Human sign-in ([#56](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/56),
docs/plan/31_console-sso-rbac.md — landing incrementally, off unless you set
`IDENTITY_MODE`) records each person who has authenticated in a `principals` row:
their issuer, subject, email, display name, first-seen and `last_seen_at`. It holds
**no roles and no tokens** — authorization is re-read from the provider's claims on
every request — so the row is a record that someone appeared, never a grant. Deleting
one revokes nothing and creating one grants nothing; revoking the person at your IdP
is what ends their access, as soon as the token they hold expires.

**No retention timer ships, and that is deliberate.** An erasure regime wants the row
gone quickly; an audit regime wants it stable for as long as a `sessions.created_by`
value still needs to resolve to a name. Either default is silently wrong for the
other, so the schedule is yours:

```sql
-- Everyone who has not signed in for 180 days. Pick your own interval.
DELETE FROM principals WHERE last_seen_at < now() - interval '180 days';
```

Two things to know before you run it. `sessions.created_by` is **plain text, not a
foreign key**, so deleting a principal never cascades into session history — the
audit trail survives, with the creator as an opaque `principal_…` id that no longer
resolves to a person, which is usually exactly what an erasure request wants.
And a deleted person who signs in again is simply provisioned afresh, with a **new**
id: older sessions keep pointing at the old one. If your audit obligations need
`created_by` to stay resolvable, retain rows at least as long as you retain sessions,
and delete the sessions first.

### 9. Single sign-on and the bundled identity provider

The platform is a **relying party and never an identity provider**: it verifies a
token against the issuer you name and mints none of its own. So the IdP is yours to
choose, and there are two shapes. Point `IDENTITY_OIDC_ISSUER` at the provider your
organization already runs — Keycloak, Entra ID, Cognito, Okta, `accounts.google.com`,
anything standards-compliant — or, for a deployment that has no IdP at all, run the
**bundled Casdoor** the compose `iam` profile ships
([deploy/compose/README.md](../deploy/compose/README.md)).

**If you already have an IdP, point the platform straight at it.** The bundled Casdoor
is a *local-account* IdP — users, groups, MFA, an admin UI, an OIDC OP — and
deliberately **not a federation hub**. Casdoor can federate to around a hundred
upstream OAuth/OIDC/SAML/LDAP providers, and this bundle configures **zero**. Adding
your corporate IdP as an upstream provider inside it would route every one of your
users through the exact code path the posture below exists to avoid; naming that IdP
as the platform's issuer routes them through none of it.

**One transport rule applies to whichever you pick.** `internal/identity` requires an
`https` key-set URL, and the guarded dialer beneath it then refuses loopback addresses
outright — so a plain-HTTP IdP cannot be wired to this platform at all, and there is no
flag to relax it. Terminate TLS in front of your IdP, and if that certificate comes
from a private CA, give the control plane `SSL_CERT_FILE` pointing at the root — and
know what that variable does, because it is narrower than it reads: it **replaces** Go's
default certificate-FILE list rather than adding to it, and public roots keep working
only because the separate scan of the certificate DIRECTORY still runs. On an image
whose roots live only in a file, setting it would drop every public CA. Set it when you
have a private CA to trust, and leave it unset otherwise — which is why the compose
bundle makes it part of the `iam` profile rather than a default. One failure
mode to recognize: Go ignores a CA file it cannot open without saying so, so a
`SSL_CERT_FILE` the control plane's user cannot read surfaces as a hard boot error out
of the verifier's warming key fetch, never as a permissions message.

#### The CERT/CC VU#780781 posture, stated plainly

Casdoor is bundled with its security record priced in rather than hidden. CERT/CC
VU#780781 (published 2026-05-28) covers **nine CVEs affecting versions ≤ 2.362.0**;
coordination with the vendor failed — *"we have not received a statement from the
vendor"* — and the advisories name no fixed version. So for eight of the nine it is the
**configuration**, not a patch note, that answers them, and that is why the hardening
is not optional decoration. Verified against Casdoor's source at v3.152.0:

| CVE | Where it lives | What answers it here |
|---|---|---|
| **CVE-2026-9090** (9.1) — signing certificate taken from the incoming `SAMLResponse` | SAML service provider | **The pinned version fixes it.** Patched silently upstream in v2.387.0 (commit `d14674e6`, 2026-04-05, seven weeks before disclosure); the bundle pins `casbin/casdoor:3.152.0`, far past it |
| **CVE-2026-9093 / 9095 / 9096 / 9098** — no audience check, no replay protection, time bounds ignored, unsolicited responses accepted | SAML service provider | **Voided twice over.** The path is reached only through a configured SAML upstream provider, and there are none; and the SP routes are **refused outright at the proxy**, so the code is not reachable over the network either |
| **CVE-2026-9091** (MFA bypass) and **CVE-2026-9092** (9.1, unverified-email account takeover) | The **upstream-provider binding** path — *any* social or OAuth upstream, not only SAML | **Voided by zero upstream providers.** Nothing else mitigates these two; they are unpatched and they are the reason the next paragraph is a requirement |
| **CVE-2026-9094** — cross-organization escalation | Multi-organization deployments | **Voided by keeping one populated organization.** The seed creates exactly one organization for people, and public signup is off, so there is no second tenant to escalate across. A Casdoor always also has its own `built-in`, which holds only Casdoor's administrator — see the organization note below, and the account note on why the seed owns that administrator's password rather than leaving Casdoor's documented default on it |
| **CVE-2026-9097** (9.8) — a revoked JWT accepted in token exchange | The token-exchange grant | **Voided by not enabling the grant.** The seeded application's `grantTypes` lists `authorization_code` and `refresh_token` and nothing else; token lifetime is a second bound (`expireInHours`, seeded at 24) |

**Zero upstream providers is a security requirement, not a default nobody got around
to changing.** Configuring one — a "sign in with Google" button, your corporate SAML —
re-opens CVE-2026-9091 and CVE-2026-9092, the highest-severity pair still unpatched, on
a path nothing in the deployment can then close. If you want corporate identity, use it
as the platform's issuer directly rather than federating it through Casdoor.

**The SAML and CAS surfaces are refused, not merely unused.** Casdoor serves its API
and its login UI from one port, so the browser must be able to reach it and
"keep it on an internal network" is not available as a control — which is why the
bundle publishes only a reverse proxy and leaves Casdoor's own port unpublished. The
proxy answers 404 to `/api/get-saml-login`, `/api/acs`, `/api/saml/metadata`,
`/api/saml/redirect/*` and the whole `/cas/*` tree, taken verbatim from Casdoor's
router; the authorization-code routes this deployment actually uses
(`/login/oauth/*`, `/api/login/oauth/*`) are deliberately left alone. If you front the
bundle with your own ingress instead, carry that block list across — it is the second
half of the SAML mitigation above.

**The organization those people live in is a security choice, not a label.** The seed
creates one of its own, `map`, and puts every human there rather than in Casdoor's
`built-in` — a user in `built-in` is a Casdoor **global administrator**, so the separate
organization is the whole difference between a platform `viewer` and someone who can
reconfigure the identity provider. It is also why every `IDENTITY_ROLE_MAP` key is
spelled `map/platform-admins` and friends: a Casdoor group reaches the token as
`organization/name`, so the organization's name is part of the authorization policy, and
renaming it without updating the map unmaps every human at once.

#### What rides inside a Casdoor token

Worth knowing before you decide where those tokens are allowed to travel, and measured
on a real token from the seeded stack rather than read off a struct. Casdoor's **`JWT`
token format embeds the user record** in the payload, and the bundle must use that
format: the alternative `JWT-Standard` drops `groups` from the token, and `groups` is
where the roles this platform maps actually ride. A JWT payload is base64url, not
encrypted, so whoever holds the token can read all of it.

What that record contains is better than the description sounds and not nothing:
`password`, `totpSecret` and `hash` all come back **blanked**, but `passwordSalt`
**does** ride in the token. A salt is not a secret the way a password is — its job is
to defeat precomputation across users, and it buys an attacker nothing without the hash
that is not there — but it is a per-user value out of the IdP's own user table that
leaves the IdP with every token issued. So treat one of these tokens as *the user
record*, not as a handful of claims: keep it out of logs and proxy traces, keep the
lifetime short, and do not hand it to a third party the way an opaque access token
could be handed over.

#### What you own

- **Bumping the image.** The bundle pins a version; keeping it current is yours.
  Casdoor ships several releases a week, and the posture above is version-sensitive —
  re-read it when you bump, because the mitigation for eight of the nine CVEs is
  configuration that a future release could rename or restructure.
- **Everything dev-shaped about the compose profile.** It seeds **well-known dev
  accounts**, publishes the proxy on loopback only, and keeps the IdP's users in a
  second database inside the same bundled Postgres — so `docker compose down -v`
  destroys the user store along with the platform's data, and the regenerated signing
  certificate invalidates every token issued before the wipe. None of that is a
  production posture; it is a laptop's. Real accounts, real secrets, and a database
  lifecycle you actually chose are the price of running this bundle for real.
- **Editing what the seed created — and knowing which edits survive.** The compose bundle
  keeps `initDataNewOnly=false` deliberately, because it is the only setting under which
  the seed owns Casdoor's own `built-in/admin`: Casdoor creates that global administrator,
  with its publicly documented default password, *before* it reads the seed file, so
  `true` would leave that credential in place on every boot. The cost is that every
  entity the seed **names** is re-applied on restart, so an edit to those four accounts,
  to the `map-console` application (its 24-hour token lifetime included) or to the three
  groups reverts. Entities the seed does not name are untouched, so a user you add in the
  admin UI survives. Change a seeded value in the seed file, not in the UI; change
  anything else wherever you like. The Helm chart shares that seed byte for byte and makes
  the same choice for the same reason, with two differences that follow from a cluster IdP
  being *published* where a laptop's is not: the three demo accounts above are dropped from
  the render entirely, and `built-in/admin`'s password comes from the required
  `casdoor.adminPassword` rather than this file — so on Kubernetes there is no committed
  credential to replace, only one you supply.
- **The role mapping itself.** Roles come from claims on every request, so the group
  membership in your IdP *is* the authorization policy — §8 above and
  [docs/ARCHITECTURE.md](./ARCHITECTURE.md#security-invariants) cover what the platform
  does and does not store. A claim value that maps to nothing yields no role and is
  denied everywhere, which is the fail-closed direction but also the shape a
  misconfiguration takes: if every human is refused, check the claim name and the map
  before you suspect the token.

### 10. Management-key lifecycle

Numbered last because it shipped last (plan 32,
[#378](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/378)); read it
beside §6, whose environment-key lifecycle it parallels.

The management `x-api-key` authenticates every **management** `/v1` route and the
console API behind it — the work API is the exception, taking a worker's
environment key instead — so it is the credential with the widest blast radius in
the platform. Two things write it, and they are deliberately different.

`CONTROLPLANE_API_KEY` is **env-var-managed**: `EnsureAPIKey` runs once at boot,
hashes the value you configured, and archives any other live **env-var-managed**
key sharing its name. That is **rotation-by-restart** — set a new value, restart,
and the previous one is dead — and `api_keys_one_live_unissued` makes "one live
key per name" a schema invariant for those rows, so two control-plane replicas
racing to adopt different values cannot both win. Read the qualifier literally: a
**console-issued** key that happens to share the name is left alone, because it is
not this lane's to retire and several live keys may share a name here. Restarting
with a new value therefore rotates the bootstrap credential and nothing else — if
you also meant to retire an issued key, archive it over the console API. Its inverse is worth knowing before you meet
it: rotation is by *value*, so putting a previously-archived value back into the
variable revives that row at the next boot. If the value had been issued from the
console, the adoption is logged with a warning naming its previous status; if it
was env-var-managed all along, it is not, having never left that lane.

Console-**issued** keys are the other writer. The platform generates the secret —
256 bits of CSPRNG behind an `sk-map-api01-` prefix — returns it exactly once in
the issuing response, and stores only its SHA-256 hash. Nothing can render the
value again, so a lost key is reissued, never recovered; what survives is a
`partial_key_hint`, masked by anchoring on the known prefix rather than on the
value's last separator, because base64url's alphabet contains `-` and a
last-separator rule would publish most of a minted key. Several live keys may
share a name here, matching the reference, so these rows sit outside the
one-live index above.

A key has three settable states and one derived one. `active` authenticates.
`inactive` is a **disable you can undo** — the reversible state, and the one to
reach for when you are not certain. `archived` is **permanent**, and permanent
here means an archived key cannot be patched at all: not un-archived, not
renamed, not archived a second time, and not even with an empty body. A retried
archive is an error rather than a no-op, which is worth knowing if you script
against this surface — as is the fact that an empty patch, which succeeds
harmlessly on a live key, becomes an error once the row is archived or lapsed.
`expired` is not settable — it is computed at read time from `expires_at`
**against the database's clock**, so a lapsed key stops authenticating the
instant it lapses and no sweeper can be down when it does.

Where those overlap, the precedence is **`archived` > `expired` > what you set**.
A key you disabled and then let lapse reads `expired`, not `inactive` — the clock
outranks your own action. A key you archived after it had already lapsed reads
`archived`, because retirement is the more final fact and the one you chose.

Expiry itself is the caller's choice: an absolute instant supplied at issue, with
no duration vocabulary, and **absent means never**. An instant already in the
past is accepted and mints a key that is born `expired` — useless, but refused by
the credential path from its first request, so it is inert rather than dangerous.
Once a key has lapsed, the **only** operation left is archiving it: re-activating,
disabling, renaming and the empty patch are all refused. So a lapsed key cannot
be tidied up by renaming it — retire it and issue a replacement.

Every rule in these two paragraphs was measured against the reference console
rather than inferred ([#389](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/389)).
Three of them contradict what this platform shipped first, which is why they are
stated this precisely.

What you own:

- **Who may mint one.** The three console routes are gated at the **admin** role —
  the tier that bounds writing a secret: environment-key issuance, and vault
  *credential* mutation and validation (a vault itself is `developer` to write and
  `viewer` to read; only what it holds is admin-gated). Read that gate precisely: it
  binds the *human* lane. `requireRole` applies only to identity-authenticated
  requests, so with `IDENTITY_MODE` unset or `disabled` — the default — anyone
  holding the management `x-api-key` reaches these routes, and **a management key
  can mint management keys**. That is not an escalation, since the holder already
  has every management capability, but it does mean the audit trail is only as
  good as the identity lane: a key minted on the machine lane records the issuing
  *key's* row id in `created_by`, not a human. If you want "which person issued
  this credential" to have an answer, configure SSO (§9) and let admins act
  through it.
- **Provisioning.** The operator surface is the console API — off the `/v1` wire,
  under `/api/console/organizations/default/workspaces/{workspace}/api_keys`.
  Note the prefix: `/api/console/`, not the `/api/oauth/` the environment-key
  surface uses. Both mirror the reference, which runs two dialects on two
  surfaces, and each of ours keeps the one it was recorded under.

  ```sh
  # issue — raw_key is the only copy that will ever exist
  curl -sX POST "$CONTROLPLANE/api/console/organizations/default/workspaces/default/api_keys" \
    -H "x-api-key: $MANAGEMENT_KEY" -H 'content-type: application/json' \
    -d '{"name":"ci-runner","expires_at":"2027-01-01T00:00:00Z"}'
  # list — a bare JSON array, archived rows included, never a secret
  curl -s "$CONTROLPLANE/api/console/organizations/default/workspaces/default/api_keys" \
    -H "x-api-key: $MANAGEMENT_KEY"
  # disable reversibly; swap for "archived" to retire one for good
  curl -sX POST "$CONTROLPLANE/api/console/organizations/default/workspaces/default/api_keys/$KEY_ID" \
    -H "x-api-key: $MANAGEMENT_KEY" -H 'content-type: application/json' \
    -d '{"status":"inactive"}'
  ```

- **Rotation cadence.** The two lanes rotate differently and you should not mix
  them. Rotate `CONTROLPLANE_API_KEY` by changing the variable and restarting;
  rotate an issued key by minting its replacement, rolling that value out, and
  archiving the old one — in that order, so nothing is ever without a live
  credential. Issue one key per consumer (a CI runner, a console BFF, an
  operator's laptop) rather than sharing one, for the same reason environment
  keys are per-host: the blast radius of a leak is then one consumer, and
  archiving is a rolling operation.
- **Expiry as policy, not backstop.** Unlike environment keys, which carry a
  fixed one-year expiry, a management key expires only if you say so. Set one on
  every key you issue to a machine — the credential that outlives the job it was
  minted for is the one that leaks quietly — and leave it absent only where you
  have a rotation process that does not depend on it.
- **Transport secrecy.** Terminate TLS in front of the control plane, and keep
  the value out of images, logs, and shell history. `raw_key` appears in exactly
  one response body and nowhere else, which makes the issuing call the one to
  keep out of a terminal recording.

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
- **Environment-key issuance UX** — **closed (#43); the console screens remain.**
  Keys are issued, listed and revoked through the console API (§6 above) — off
  the `/v1` wire, reached with either the management `x-api-key` or an SSO
  identity holding `admin`, one key per host — so no operator has
  to seed a key into the database any more, and the curl workflow above is the
  whole story for a headless deployment. Accepted end to end on 2026-08-10
  against a real `ant beta:worker` on a console-issued key, including per-host
  revocation taking one worker down while another kept polling
  ([docs/HISTORY.md](./HISTORY.md)). What remains is presentation, not
  capability: the managed-agent-console screens that drive those routes are that
  repo's plan 07. Three limits are stated rather than papered over: this is a
  management-credential surface, so it delegates no authority the management
  `x-api-key` did not already carry and there is no separate "can mint worker
  keys" role — a human reaching it over SSO needs the general `admin` role
  (plan 31, [#56](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/56)),
  the whole surface including the listing, while the management key itself
  remains ungated because it has always meant full authority; and a key still
  cannot be scoped to less than its whole environment. A third limit was lifted on 2026-08-11: the acceptance now also
  covers a worker *executing* a pulled tool call on such a key — a real
  `ant beta:worker` claimed a `tool_exec` item, ran `bash` in-process and settled
  the result back, closing
  ([#363](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/363)).
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
