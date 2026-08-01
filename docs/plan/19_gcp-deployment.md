---
status: approved
---

# Production deployment on Google Cloud (plan 19)

A supported, documented, acceptance-proven path to run the platform on GKE with
Google-managed backing services. Every decision below rests on probes run against real
GCP on 2026-08-01 (Ground truth); the plan adds **three small code deliverables** — a
GCS delete-convergence fix, a Cloud KMS cipher backend, and resource bounds on sandbox
pods — and everything else is images, configuration, documentation, and two recorded
acceptance runs. The Helm chart refuses at render time to combine `existingSecret` with
any bundled backing service (`templates/secret.yaml`), so self-managing the Secret
implies bringing external services — which splits the work into two configurations. The
plan runs them in order: **mode-1** (chart-managed Secret + bundled services) proves the
platform end-to-end on GKE with the fewest variables, **mode-2** (`existingSecret` +
external everything) is the production target. The split is chart-enforced; the
ordering is deliberate risk reduction, one variable class at a time.

Deliberately **not** in this plan, each with its own tracker or a follow-up to file:

- **Vertex AI model provider.** The user decision for this deployment is the existing
  public Anthropic-protocol endpoint, which design principle 4 serves with zero code.
  Vertex's shape is structurally different (`:streamRawPredict` URL, `anthropic_version`
  in the body, OAuth) — but `anthropic-sdk-go` ships a `vertex` package whose
  `WithGoogleAuth` option does the whole adaptation, so a future issue is cheap, not a
  rewrite.
- **gVisor sandboxes / standalone-gate redesign — the whole Google-reference shape.**
  Aligning with Google's managed-agents sandbox reference (Autopilot + gVisor + warm pool
  + FQDN egress policy) is not a configuration choice for this platform; it is a
  prerequisite chain. In-pod iptables under GKE Sandbox is impossible (Ground truth), so
  gVisor requires moving egress enforcement to the CNI and running the gate as its own
  pod. That topology is probe-proven feasible and would even delete the `NET_ADMIN`
  requirement, but scoping it against the code found two consequences that make it a plan
  of its own rather than a slice, and both are **security** consequences rather than
  effort ones:
  - **A routable gate needs proxy authentication that does not exist.**
    `gaterun.CheckLoopbackListenAddr` exists precisely because the proxy is
    unauthenticated — safe only while it is reachable solely from inside the sandbox's
    own network namespace. A gate pod on a flat pod network is otherwise an open,
    credential-substituting proxy that any pod able to route to it can spend another
    session's secrets through.
  - **The fail-closed guarantee weakens from structural to behavioral.** Today the rules
    live in the sandbox's netns and the only process that could change them dropped
    `CAP_NET_ADMIN` before serving, and `gaterun.CheckListing` verifies the ruleset
    token-for-token. A NetworkPolicy is an external, mutable object: anyone with
    namespace RBAC can delete it mid-session and egress opens silently, and an admission
    canary can only prove one destination was blocked at one instant. That weakening is
    irreducible, not a matter of better engineering.

  Roughly five PRs and a decision about whether the Docker backend converges on the same
  topology or the two backends keep different enforcement models permanently. It gets its
  own plan, with this plan's probe evidence attached.
- **Warm-pool sandbox provisioning** (kubernetes-sigs/agent-sandbox `SandboxWarmPool` /
  `SandboxClaim`): cold-start optimization, its own plan, and the condition for
  revisiting Autopilot (Decision 1). It sits behind *two* prerequisites, not one. The
  standalone gate must land first — the gate sidecar and the `limited` route-flush init
  container are create-time pod-spec facts a pre-created warm pod cannot acquire, so
  adopting warm pools earlier would ship a Kubernetes mode that silently degrades
  `limited` sessions to full egress. And sandbox teardown must exist
  ([#64](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/64)): no
  production code calls `Sandbox.Destroy` today, so a fixed-size pool would drain and
  every new session would block on binding.
- **A separate namespace for sandbox pods.** The executor creates sandboxes in the
  release namespace and holds `create` on pods there, so a compromised executor process
  could mount the platform Secret — the sandbox itself cannot, running with
  `automountServiceAccountToken: false`. Splitting the namespace is worth materially more
  for runtime exposure than any change to how the Secret is *delivered* (Decision 6), and
  it is a small chart-and-RBAC change; it is out of scope here only because it is
  platform-wide rather than GCP-specific. File it as an issue when this plan lands.
- **A GCS-native blob backend.** The existing S3 backend speaks to GCS through the XML
  API once slice 1 lands; a `cloud.google.com/go/storage` backend would only remove the
  HMAC dependency and is not needed.
- **An Ingress/Gateway chart template.** The deploy docs get a worked Gateway +
  HTTPRoute example; a chart template is speculative until someone asks.
- **FQDNNetworkPolicy as an egress mechanism.** It cannot substitute credentials (it is
  connection-level), and its validation on Standard was blocked by a gVisor-node DNS
  anomaly (Ground truth). The gate covers egress control; FQDN policy stays out.
- **Workspace persistence** (e.g. GCS FUSE for session outputs, as Google's
  anthropic-agent-sandbox reference does): a product-semantics change, gated on the open
  wire question of what the reference platform's workspace-persistence behavior actually
  is (resolve per CLAUDE.md's order before filing).

## Ground truth (probes run against real GCP, 2026-08-01)

Method: throwaway fixtures (bucket + HMAC key, KMS keyring, Cloud SQL instance, two GKE
clusters), each exercise driven by the repo's own contract suites ported verbatim, all
resources deleted and the sweep verified empty afterwards.

- **GCS ↔ `internal/blob/s3` (minio-go v7.2.1, the repo's pin).** The `blobtest`
  contract passes 7/8; `DeleteMissingIsNil` fails: GCS's XML API answers DELETE of a
  missing object with `404 NoSuchKey` (measured: `Code="NoSuchKey" StatusCode=404`)
  where AWS S3 and MinIO answer 204 — which breaks `blob.Store`'s crashed-and-retried
  delete convergence. Everything else round-trips, including the 5 MiB payload.
  `MakeBucket` against a missing bucket **succeeds** unattended (no `x-goog-project-id`
  needed), so bucket pre-creation is least-privilege hygiene, not a requirement.
- **Cloud KMS as a `secrets.Cipher`.** A KMS-backed cipher (key resource name as the
  keyID, decrypt refusing a foreign keyID — the OpenBao backend's guard) passes the
  `secretstest` contract 6/6, including the 64 KiB round-trip. The raw API's plaintext
  ceiling is exactly 65536 bytes (65537 is rejected, `max_expected_size:65536`) — the
  contract's largest case with zero margin, and a limit OpenBao transit does not have.
  Decision 3 accepts that ceiling rather than engineering around it. Ciphertext is
  randomized and authenticated (fresh-encryptions-differ and tamper-detection both
  pass); overhead ≈85 bytes.
- **Cloud SQL for PostgreSQL 16.** The repo's 15 migrations apply in order. The event
  log's exact LISTEN/NOTIFY pattern (dedicated pooled connection holding LISTEN, NOTIFY
  inside the writer's transaction) works: nothing is delivered before commit, delivery
  arrives after commit. `pg_notify` payloads: 7999 bytes accepted, 8000 rejected —
  standard Postgres, not a Cloud SQL limit. `FOR UPDATE SKIP LOCKED` works. Two
  operational constraints: transaction-mode connection pooling breaks a persistent
  LISTEN (the failure mode `events/broker.go` itself names) — keep Cloud SQL's managed
  pooling **off**; the Auth Proxy is a plain TCP tunnel and is fine (the probe ran
  through it).
- **The gate on GKE Standard's default runtime** (COS + containerd, Dataplane V2,
  v1.35.6-gke — above the chart's ≥1.29 native-sidecar floor): the exact
  `iptables-restore --noflush` payload + OUTPUT jump from `cmd/gate` applies on IPv4 and
  IPv6, reads back as exactly the three-rule owner-match chain, and **enforces**: root
  egress dropped, gate-UID egress allowed.
- **gVisor × in-pod iptables: impossible.** Under GKE Sandbox, the nft backend fails
  (`Failed to initialize nft: Protocol not supported`) and the legacy backend too
  (`can't initialize iptables table 'filter': Table does not exist`). The real gate
  fails **closed** — `gaterun.Setup` error exits the process before anything listens,
  so the HEALTHCHECK that gates sandbox admission never passes: gated sessions on
  gVisor are unusable, not unprotected.
- **The standalone-gate topology (follow-on's evidence).** On Standard + Dataplane V2:
  NetworkPolicy **binds on gVisor pods** — raw-IP egress (`https://1.1.1.1`) times out
  at the TCP layer, so enforcement is non-cooperative — and an HTTPS CONNECT tunnel
  from inside gVisor through a runc proxy pod returns 200. The sandbox needs no
  external DNS (CONNECT hands the hostname to the gate, which resolves) — a property of
  the topology. One deployment gotcha for any NetworkPolicy this plan writes: with
  node-local-dns active (GKE enables it alongside FQDN network policy), the
  "allow egress to kube-dns pods" carve-out does not match — the node intercepts the
  kube-dns VIP — so DNS carve-outs must use the port-53 / ipBlock form.
- **Sandbox workspace.** The K8s provider's pod spec declares **no volumes and no
  resources** — the workspace is the container's writable layer on the node boot disk
  (`mkdir -p /workspace` in the pod command), with no ephemeral-storage bound. Untrusted
  agent code can therefore fill the node disk and evict neighbors — a security property,
  fixed by slice 2. Lifecycle is otherwise sound: `Provision` is adopt-or-create per
  work item and skills/files are re-materialized per work item
  (`internal/executor/executor.go`), so a replacement sandbox recovers its mounts;
  what a sandbox death loses is agent-written files and shell snapshots — the
  documented cattle-not-pets trade.

## Decisions

1. **GKE Standard now; Google's Autopilot shape is the destination, not the starting
   point.** Google's managed-agents sandbox reference runs Autopilot + gVisor + a warm
   pool + FQDN egress policy, and that is where this platform should end up. It cannot
   start there: every one of those four rests on machinery this repo does not have yet,
   and the chain is strictly ordered — standalone gate (else gVisor breaks gated
   sessions outright), then sandbox teardown (#64), then warm pools (else per-session
   churn makes Autopilot's per-pod billing and scale-up latency the wrong trade), and
   only then Autopilot. Adopting any link early ships a mode that silently loses
   `limited` networking or drains its pool. Standard is what the probes proved working
   end to end today: the gate enforces on its default runtime, `NET_ADMIN` needs no
   workload policy, and Pod Security stays under operator control. The two follow-on
   plans below are the path, and each names its own revisit condition.
2. **Model endpoint: the existing public Anthropic-protocol endpoint**, as one
   `brain.modelProviders` route with `upstream_model` set (the #88 metric-cardinality
   note in values.yaml). Design principle 4; zero code.
3. **Cipher: `internal/secrets/gcpkms`, KMS `Encrypt`/`Decrypt` directly — this is what
   removes OpenBao.** Two different things are easy to conflate here, so to be explicit:
   the *cipher* is a runtime encryption service for the platform's vault feature
   (ciphertext in Postgres, keys never leaving the service), which is OpenBao transit
   today and Cloud KMS after this slice; it is unrelated to how the deployment's own
   Kubernetes Secret is produced (Decision 6). Secret Manager is not a substitute for
   either — it stores secrets for retrieval and has no encrypt/decrypt API for arbitrary
   payloads, so it is nowhere in the runtime path. Mode-1 still runs the bundled OpenBao
   (fewest moving parts, and it exercises the out-of-the-box install other self-hosters
   use); **mode-2 drops the OpenBao StatefulSet entirely**, and with it the `bao-token`
   the Secret carries today — KMS needs only a key resource name, which is not a secret,
   plus Workload Identity. keyID is the CryptoKey resource name and Decrypt refuses any
   other (the OpenBao backend's guard).
   Auth is ADC — Workload Identity on GKE, no static key material. **The 65536-byte
   plaintext ceiling is accepted as the backend's documented limit**, not worked around:
   the measured ceiling sits exactly on the contract's largest case, so the contract
   passes with zero margin, and vault credential material (OAuth tokens, bearer tokens,
   environment-variable values) is orders of magnitude below it. The backend rejects
   oversized plaintext with its own clear error rather than surfacing a raw
   `InvalidArgument`, and the limit is documented where an operator picking a cipher will
   see it. **Envelope encryption** — a local AES-256-GCM data key wrapped by KMS, which
   removes the ceiling entirely and cuts KMS round-trips — is the recorded escape hatch
   if that limit is ever reached; it is deliberately deferred rather than built now
   (it roughly doubles the backend's complexity for a limit nothing is near). Tests: the
   full `secretstest` contract against an in-process fake KMS gRPC server (hermetic —
   `make test` never touches GCP) plus an explicit at-and-over-the-ceiling case, and a
   live tier under the repo's standing consent contract (`.env` supplies the key name,
   `RUN_LIVE_KMS_TESTS=1` supplies consent, opted-in missing config fails rather than
   skips).
4. **Two acceptance phases, dictated by the chart.** Mode-1: bundled
   Postgres/MinIO/OpenBao, inline values — proves images, the K8s sandbox path, the
   gate, skills/files/vaults end-to-end on GKE. Mode-2: Cloud SQL + GCS + KMS +
   pre-created Secret (`existingSecret`) — the production shape. The chart's render-time
   exclusivity makes the two-configuration split structural; running mode-1 first is
   the plan's own risk-reduction choice, not a chart requirement.
5. **Observability: an in-cluster OTel Collector** (googleclientauth extension) fronting
   `telemetry.googleapis.com`. The platform's OTLP exporter is gRPC + endpoint +
   insecure only, and that stays: auth is the collector's job.
6. **Secrets handling: Secret Manager is the source of truth; materialization is the
   variable.** Mode-1 uses inline Helm values. Mode-2 needs a pre-created Secret, and the
   deploy guide documents two ways to produce it — never a hand-typed
   `kubectl create secret --from-literal`, which leaks the value into shell history and
   CI process arguments:
   - **Baseline (what this delivery uses): a deploy-time materialization step** —
     Terraform provisions the Secret Manager entries, and one scripted
     `gcloud secrets versions access` → `kubectl apply` materializes the K8s Secret. This
     is the shape Google's own managed-agents sandbox reference uses, and it already
     buys the properties that matter here: provenance, versioning, audit logging, and
     reproduce-from-a-clean-project.
   - **External Secrets Operator, for teams and multiple clusters.** Its marginal buy
     over the baseline is continuous drift correction, multi-cluster fan-out, and
     deployers not needing Secret Manager read access — real at that scale, close to
     nothing for a single staging environment, against the cost of a cluster-scoped
     operator with churn-prone CRDs and a new silent failure mode (a broken sync is
     invisible until the next cluster build). Note its `creationPolicy: Owner` default:
     deleting the `ExternalSecret` deletes the K8s Secret, and `DATABASE_URL` /
     `CONTROLPLANE_API_KEY` are not `optional`, so every component wedges on the next
     pod churn.

   Two things the guide must state plainly, because both are commonly assumed and both
   are false here. **Neither mechanism reduces runtime exposure**: every process reads
   the same Kubernetes Secret from etcd, so what changes is where the authoritative copy
   lives, not what a compromised pod can reach (encryption at rest is a separate,
   orthogonal control — GKE application-layer secrets encryption with Cloud KMS, worth
   enabling under either mechanism). And **neither rotates a running deployment**: every
   env-injected credential is resolved at container start, the brain's mounted
   `model-providers.json` is read once at startup (`cmd/brain/main.go:69`), and the
   chart's only rollout trigger — the `checksum/secret` annotation — is inert in the
   `existingSecret` path, because `templates/secret.yaml` renders empty there and its
   checksum is therefore constant. Rotation is `kubectl rollout restart` on all three
   Deployments, under either mechanism.
7. **Images: Cloud Build, native linux/amd64.** One server image pushed under the three
   per-component names the chart's `map.image` helper expects, plus the `--target gate`
   image. Artifact Registry remote repositories mirror `debian:stable-slim` and
   `busybox` for `executor.sandboxImage` / `netSetupImage` (rate-limit and
   private-node hygiene).
8. **No domain or managed certificate for this delivery.** The control plane is reached
   at a load-balancer IP (or in-cluster), so slices 4 and 5 exercise no TLS termination
   of our own. The deploy docs still carry a Gateway + HTTPRoute example for operators
   who have a domain; nothing goes in the chart.
9. **A persistent staging environment with one-command create and destroy.** The cluster
   and its backing services stay up between slices rather than being rebuilt per run,
   but teardown must be one command so an idle week costs nothing. Provisioning is
   **Terraform** under `deploy/gcp/`, wrapped by Make targets — the same shape Google's
   managed-agents sandbox reference uses. Terraform rather than shell because destroy is
   the operation that must not miss anything: state-tracked teardown is what prevents
   the orphaned-billable-resource failure mode (this plan's probe sessions only avoided
   it by manual sweeps). This adds Terraform as a **developer-tooling** dependency for
   GCP deployment only — never a dependency of the platform, its build, or `make verify`,
   and the Helm chart stays the portable installation path.

## Slices

Each slice is one PR unless noted; TDD per CLAUDE.md (the failing test first).

- **Slice 1 — GCS delete convergence (`internal/blob/s3`).** `Delete` treats
  `NoSuchKey` from `RemoveObject` as success (the same error-code check `Get` already
  uses), restoring the contract's convergence on GCS while changing nothing on
  S3/MinIO (which never produce it). Test: a stub S3 endpoint whose DELETE answers
  `404 NoSuchKey` — asserted failing against the pre-fix code first (the
  fixture-must-force-the-transformation lesson); the MinIO-backed contract suite stays
  as-is and keeps passing.
- **Slice 2 — sandbox pod bounds and the free hardening (`internal/sandbox/k8s` +
  chart).** Three things Google's reference pod spec has that ours lacks and that cost
  nothing behaviorally: **`seccompProfile: RuntimeDefault`**,
  **`allowPrivilegeEscalation: false`**, and **cpu/memory/ephemeral-storage requests plus
  cpu/memory limits as operator configuration with an empty default** — never hardcoded
  constants, since Google's 500m/1Gi would make sandbox pods unschedulable on a small
  node and an unschedulable pod burns the full `readyTimeout` before `Provision` fails.
  The GCP deploy guide and the staging Terraform then set concrete values sized to their
  node pool; the platform itself stays unopinionated and today's behavior is the default.
  Three deliberate exclusions, each with a reason: an **ephemeral-storage limit** is left
  off, because a session resource mount can be hundreds of megabytes and kubelet evicts
  on the limit — mid-session eviction surfacing as `ErrNotFound` is a worse failure than
  the node-pressure risk it averts at that granularity, and Google's own template does
  not set one either; a **memory limit** must be documented as turning an OOM from one
  dead command into a dead sandbox (`RestartPolicyNever`, so nothing restarts it);
  **`capabilities: drop ALL`** waits on a measurement, since `apt-get install` in the
  sandbox — the product premise — needs `CAP_CHOWN` for dpkg's postinst and SETUID/SETGID
  for apt's own sandbox. Docker parity is decided in the same PR: either mirror the two
  security options into `hostConfig` or register the divergence, because two backends
  with two answers has been the defect here before. Unit-test at the podSpec level, and
  run the shared contract suite on kind.
- **Slice 3 — `internal/secrets/gcpkms` + chart knob + `cmd/` wiring.** The direct-KMS
  cipher of Decision 3, including its documented 65536-byte plaintext ceiling and the
  clear error above it; `secrets-backend=gcpkms` env plumbing (key resource name; ADC
  for auth) through the chart's Secret keys and `map.secretsEnv`; docs for the
  Workload Identity binding. Hermetic contract + live tier as above.
- **Slice 4 — staging environment + mode-1 acceptance on GKE.** `deploy/gcp/` Terraform
  and its Make targets (Decision 9) stand the environment up and tear it down in one
  command each — cluster, Artifact Registry, service accounts and Workload Identity
  bindings — and are used to create the environment this slice then deploys into, so the
  scripts are exercised rather than merely written. Then: Cloud Build images;
  `helm install` with bundled services; the real `ant` CLI end-to-end: sessions, bash
  and file tools, HITL, the `skill-answer` and `file-answer` evals, a vault-attached
  `limited` session proving allowed_hosts enforcement and credential substitution
  through the gate on K8s; OTel Collector shipping traces to Cloud Trace. No planned
  code — anything found feeds back as fix PRs. One thing this run will make visible and
  should measure rather than be surprised by: with no production caller of
  `Sandbox.Destroy` (#64), every session leaves its pod behind, which on a metered node
  pool costs money and node capacity rather than just a stale container. Acceptance
  record → docs/HISTORY.md.
- **Slice 5 — mode-2 acceptance + `docs/deploy-gcp.md`.** Cloud SQL (private IP,
  managed pooling off, `sslmode=require`, DDL-capable user for startup migrations),
  GCS (pre-created bucket, HMAC key, single-bucket `storage.objectAdmin`), KMS
  (Workload Identity), pre-created Secret (ESO documented), private nodes + Cloud NAT,
  AR mirrors. Re-run the slice-4 acceptance battery; write the deploy guide (including
  the Gateway example, the collector config, the node-local-dns DNS-carve-out note, and
  backups — which under KMS lose the OpenBao pairing complexity but must still name
  the HMAC-key and KMS-key lifecycles). Acceptance record → docs/HISTORY.md; README
  status line updated in the same PR.

## Acceptance (plan-level)

The real `ant` CLI drives the platform on GKE in mode-2 — all backing services
Google-managed; KMS and the collector authenticate via Workload Identity with no
downloaded key material; the static credentials that do remain in-cluster are each
scoped and rotatable: the platform's own keys (management API key, model key), the
Cloud SQL password inside the DSN, and the single-bucket GCS HMAC pair — the last being
the S3-interop trade-off whose removal is the GCS-native-backend follow-on — with both
evals passing, gate enforcement and credential substitution proven in a `limited`
session, traces visible in Cloud Trace, and the deploy guide reproducing the whole
setup from a clean project. Both acceptance runs recorded in docs/HISTORY.md.

## Open questions

*(none — the plan's questions were resolved before approval: no domain/TLS for this
delivery, a persistent staging environment with scripted teardown, and the secret-delivery
mechanism, are Decisions 8, 9 and 6 respectively.)*
