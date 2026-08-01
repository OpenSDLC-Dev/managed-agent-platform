---
status: archived
issue: "#65"
---

# Sandbox hardening: cgroup limits, capability drops, non-root, read-only rootfs

Archived: completed — implemented by the PR that landed this file (a single-PR
plan: the same PR starts and finishes the work, so the file lands archived; the
delivery record is the CHANGELOG entry and docs/HISTORY.md).

## Problem

A session's sandbox runs untrusted, model-directed commands, and the platform
creates it with almost no containment
([#65](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/65)):

- **Docker** (`internal/sandbox/docker/docker.go` `sandboxConfig`) sets
  `HostConfig.Init` and nothing else. The `hostConfig` struct
  (`internal/sandbox/docker/api.go`) carries only `NetworkMode`, `Init`,
  `CapAdd`, `CapDrop` and `SecurityOpt` — there is no field for a pids limit, a
  CPU quota, a memory cap, a read-only rootfs, or a user. The capability drops
  that do exist (`NET_RAW`/`SETUID`/`SETGID` plus `no-new-privileges`) are
  applied **only to a gated sandbox**, where they exist to keep a tool from
  forging the gate's egress identity; an ungated sandbox runs with the runtime's
  full default capability set.
- **Kubernetes** (`internal/sandbox/k8s/k8s.go` `podSpec`) gives the sandbox
  container a `SecurityContext` only when the session is gated, sets no
  `Resources`, and sets no `runtimeClassName` on the pod. Because the provider
  never sets `runtimeClassName`, the Helm chart deliberately does not ship the
  optional gVisor `RuntimeClass` — shipping it would be unwired
  (`docs/DIVERGENCES.md`, "Helm chart — plan-sketch divergences").

The gap matters most for **process and CPU containment**, because the exec
deadline cannot reclaim what it fails to kill. Both backends kill a timed-out
command by SIGKILLing its process *group*
(`internal/sandbox/docker/docker.go`'s watchdog, `internal/sandbox/k8s/deadline.go`),
so a child that calls `setsid` escapes the kill and outlives the deadline;
abandoning the exec's stream is not a kill primitive. A process that escapes can
pin a core until the container is destroyed, and enough escaped processes can
slow the daemon probe (`top`/`ps`) the deadline uses to label an overrun. cgroup
limits are the designed containment for exactly that, and neither backend sets
any.

Today's split is documented honestly — `docs/self-hosted-security.md` puts
non-root, capability drops, read-only rootfs and runtime isolation on the
*operator*, at the runtime/orchestrator layer, and its "Reserved seams" section
names this gap. This plan moves the platform-enforceable half of that line onto
the platform. It does not close #49, which tracks the document itself; it
revises what that document reports.

## Design decisions

### 1. `sandbox.Hardening` on `Spec`, not on the provider config

The controls land as a `Hardening` struct on `sandbox.Spec`, alongside `Image`
and `Networking` — a platform deployment choice that is applied when the sandbox
is provisioned, which is what the issue's "configurable at provision time" asks
for. Putting them on the *provider* config instead would have been less code,
but the acceptance also asks that the shared `sandboxtest` contract suite cover
them, and a per-`Spec` knob lets one contract row provision a differently
hardened sandbox from the same provider. It also leaves the seam open for a
future per-environment sizing policy without another move.

Like `Env` and `Networking`, `Hardening` is **bound when the sandbox is
created**: `Provision` is idempotent and adopts a session's existing sandbox
without re-applying a changed `Hardening`.

### 2. The zero value is today's behavior; the defaults live in the deployment layer

`Hardening{}` means "apply nothing", so a provider constructed by a test or a
programmatic caller behaves exactly as it does today and the existing contract
rows are untouched. The **platform defaults** are resolved by
`sandbox.HardeningFromEnv`, which the two binaries that actually run sandboxes
(`cmd/executor`, `cmd/worker`) call — following `internal/secrets/env.go` and
`internal/blob/s3/env.go`, which already read their own environment. So every
real deployment gets the defaults, and no test acquires a limit it did not ask
for.

| Knob | Env var | Default | Rationale |
|---|---|---|---|
| Pids limit | `SANDBOX_PIDS_LIMIT` | `512` | Generous for an agent; caps a fork bomb and the process pressure that stalls the deadline's probe. `0` disables. **Docker only** — see decision 4. |
| CPU limit | `SANDBOX_CPU_MILLIS` | `2000` (2 CPUs) | The containment for a process that escaped the deadline's group kill. `0` disables. |
| Memory limit | `SANDBOX_MEMORY_BYTES` | `0` (off) | The third cgroup leg, but opt-in: an OOM kill mid-task is a worse failure than throttling, and the issue's acceptance names pids and CPU only. |
| Capability drops | `SANDBOX_CAP_DROP` | `NET_RAW,SETUID,SETGID` | Exactly the set a **gated** sandbox already drops, extended to every sandbox — a posture the default image is known to tolerate. `none` drops nothing; `ALL` is accepted. |
| Read-only rootfs | `SANDBOX_READONLY_ROOTFS` | `false` | "Where the image allows": needs writable mounts (decision 3) and an image that tolerates one. |
| Run as user | `SANDBOX_RUN_AS_USER` | unset | Same reason. The image's own `USER` remains the primary mechanism. |
| K8s RuntimeClass | `SANDBOX_K8S_RUNTIME_CLASS` | unset | A cluster-level choice: it goes on `k8s.Config`, not on `Spec`, next to `Namespace` and `NetSetupImage`. |

Two invariants are **not** configurable away:

- A **gated** sandbox always drops `NET_RAW`/`SETUID`/`SETGID` and runs with no
  privilege escalation, whatever `Hardening.CapDrop` says — those drops are what
  makes the gate's owner-match firewall hold, so the provider unions them in.
- `no-new-privileges` (Docker) / `allowPrivilegeEscalation: false` (Kubernetes)
  travels with any non-empty drop set: dropping capabilities while leaving
  setuid-binary escalation available defeats the drop.

### 3. Read-only rootfs arranges its own writable mounts

`docs/self-hosted-security.md` records why a read-only root is not a free
toggle: the sandbox writes the session workdir, and on Kubernetes per-exec state
under `/tmp` (`internal/sandbox/k8s/k8s.go`, `/tmp/.map-exec-<nonce>`). Review
found two more, and both were load-bearing: the persistent shell keeps every
session's cwd and environment under `/var/lib/map-shell`
(`internal/sandbox/shell`), so a read-only root without it fails the **first**
bash call of every session — as a *backend fault*, which leaves the tool
unanswered and the work item reclaiming rather than answering the model; and a
session's file resources land under `/mnt/session/uploads/<file_id>`
(`internal/api`), so materialization silently skips them. The set therefore lives
in one function, `sandbox.WritablePaths`, which both backends consume and which
deduplicates — a deployment whose workdir *is* one of the fixed paths must not
produce two mounts on one target, which both runtimes reject. When
`ReadOnlyRootfs` is set, the provider mounts writable space over every path in it — Kubernetes an `emptyDir` (the kubelet creates it world-writable,
verified against a real cluster: `drwxrwxrwx`, and a `runAsUser: 65534`
container writes it), Docker an **anonymous volume**.

A tmpfs was the obvious choice on Docker and is the wrong one. The contract row
caught it: the daemon refuses `PUT /containers/{id}/archive` on a
read-only-rootfs container — `http 400: container rootfs is marked read-only` —
when the destination is a tmpfs, and *allows* it when the destination resolves
into a volume (both measured directly against the daemon). Every file the Docker
backend writes goes through that endpoint, so a tmpfs workdir gives a sandbox
that runs commands but can never receive a file: no skill materialization, no
file materialization, no `write` tool. Anonymous volumes need no lifecycle of
their own — `removeContainer` already deletes with `v=1`, verified to take them
away with the container.

The residue, stated rather than hidden: a fresh anonymous volume takes its
ownership from the image's own directory when the image ships one, and is
otherwise root-owned `0755`. So on Docker — unlike Kubernetes — a read-only root
does **not** by itself make the workdir writable by a non-root
`RunAsUser`; the image still decides that. It does make the workdir *exist*,
which is what keeps the container's `mkdir -p <workdir>` entrypoint from failing
under a uid that could not create it. Both halves are in
`docs/self-hosted-security.md` §2.

### 4. Kubernetes cannot express a per-pod pids limit — documented, not faked

The Pod API has no pids resource (`k8s.io/api` v0.36.2 `core/v1` defines no such
`ResourceName`); a per-pod limit is the kubelet's `podPidsLimit` node setting.
So the two backends genuinely cannot enforce the same pids contract. The
alternatives were: fail `Provision` on Kubernetes when a pids limit is requested
(honest, but the platform default of 512 would then break every Kubernetes
deployment out of the box), or apply what the backend can and record the rest.
This plan takes the second: the Kubernetes provider applies CPU, memory,
capabilities, non-root, read-only rootfs and RuntimeClass, ignores `PidsLimit`,
and the gap is recorded in `docs/DIVERGENCES.md` and in
`docs/self-hosted-security.md`, which points Kubernetes operators at
`podPidsLimit`. The contract suite states the same thing structurally: the pids
row is registered only for a backend that declares it enforces one
(`Harness.EnforcesPidsLimit`), the way the gated rows are registered only for a
backend that declares a gate.

### 5. A CPU *limit* must not become a scheduling *reservation*

Kubernetes copies a limit into the request when no request is given, so a 2-CPU
limit would reserve 2 CPUs per sandbox pod and could make the pod unschedulable
on a small node (a kind cluster in CI, notably). The provider therefore sets an
explicit CPU request of `min(limit, 100m)`: the limit is containment, the
request stays out of the scheduler's way. Memory keeps request = limit, which is
the correct posture for a non-compressible resource, and is opt-in anyway.

### 6. The contract suite asserts what a tool can see

The shared rows assert the observable behaviour, never a backend's internals:
capability drops via an operation the dropped capability gates, `RunAsUser` via
`id -u`, read-only rootfs via a write outside the writable mounts, the CPU quota
via the sandbox's own `/sys/fs/cgroup/cpu.max`, and the pids limit (Docker) via
the fork failures a burst of background processes reports. Memory is covered at
the provider level only — asserting an OOM kill is a heavy, timing-shaped test
for a knob that is off by default.

### 7. Two edges the defaults must survive, not assume away

Both found by review, both measured, both defaults rather than exotic
configuration:

- **A gated sandbox may not run as the gate's uid.** The gate's owner-match
  firewall ACCEPTs exactly `gaterun.DefaultGateUID` (65532), so a
  `SANDBOX_RUN_AS_USER` set to it would let every tool process leave the
  namespace unfiltered — `allowed_hosts` void, vault substitution bypassed, and
  nothing logged. It is the same hazard as #196 (a sandbox *image* whose USER is
  that uid) reached through a knob the platform itself now offers, so the
  platform closes its own half: `Hardening.Validate` refuses it and both
  providers call it before they touch a daemon or an API server. 65532 moves to
  `gaterun.DefaultGateUID` so cmd/gate and the providers cannot disagree about
  which uid that is.
- **A CPU cap above the host's CPU count is refused by the Docker daemon**
  ("range of CPUs is from 0.01 to N"), so the on-by-default two-CPU cap would
  fail *every* provision on a one-CPU host — per session, as a daemon 400, not
  at startup. The provider clamps the cap to what `/info` reports (read once,
  logged when it clamps); a daemon that will not answer leaves the configured
  value alone, so a failed probe can never widen a cap. Kubernetes has no such
  ceiling. Also measured and documented rather than validated: the daemon's
  6 MB minimum memory limit.

## What lands

1. `internal/sandbox`: the `Hardening` type, its validation, and
   `HardeningFromEnv`.
2. `internal/sandbox/docker`: `hostConfig` gains the fields; `sandboxConfig`
   applies them and unions the gate's mandatory drops.
3. `internal/sandbox/k8s`: `podSpec` applies resources, the security context and
   the writable mounts; `Config`/`backend.Config` gain `RuntimeClass`, set on
   the pod.
4. `internal/sandbox/sandboxtest`: the new contract rows and the
   `EnforcesPidsLimit` harness flag.
5. `internal/executor`, `internal/worker`, `cmd/executor`, `cmd/worker`: the
   config threading.
6. `internal/toolset`: the `bash` description tells the model to redirect a
   backgrounded process's output — an unredirected straggler holds the exec
   stream open and costs about two seconds per call while the daemon force-closes
   it.
7. `deploy/compose`, `deploy/helm`: the knobs, plus the chart's now-wireable
   optional gVisor `RuntimeClass`.
8. Docs: `docs/self-hosted-security.md` (the shared-responsibility line moves),
   `docs/DIVERGENCES.md` (the Helm entry's gVisor deferral is resolved; the
   Kubernetes pids gap and the `bash` description are recorded),
   `docs/ARCHITECTURE.md`, `README.md`, `CHANGELOG.md`, and this plan's archive
   summary in `docs/HISTORY.md`. **Not** `STATE.md`: this plan is archived in the
   same PR that lands it, so at merge nothing here is in flight and STATE.md's
   incumbent active work stays the tracked item.

## Non-goals

- Seccomp and AppArmor/SELinux profiles: the runtime's defaults already apply,
  and a platform-authored profile is a much larger surface than this issue asks
  for.
- User-namespace remapping, and picking a hardened runtime *for* the operator:
  the platform wires `runtimeClassName` so gVisor/Kata *can* be selected; which
  one to run stays a deployment choice.
- A per-environment sizing policy on the wire. No wire field carries one; these
  are deployment knobs, and the `Spec` seam is where one would later attach.
- `#196` (a sandbox image whose `USER` is already the gate's dropped-to UID)
  keeps its own tracking issue: `RunAsUser` gives an operator a second lever
  against it, but the enforcement that issue asks for is a validation, not a
  default.
