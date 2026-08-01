---
status: approved
---

# Production deployment on Google Cloud (plan 20)

A supported, documented, acceptance-proven path to run the platform on GKE with
Google-managed backing services. Every decision below rests on probes run against real
GCP on 2026-08-01 (Ground truth); the plan adds **three small code deliverables** — a
GCS delete-convergence fix, a Cloud KMS cipher backend, and sandbox pod placement and
bounds — and everything else is images, configuration, documentation, and two recorded
acceptance runs. The Helm chart refuses at render time to combine `existingSecret` with
any bundled backing service (`templates/secret.yaml`), so self-managing the Secret
implies bringing external services — which splits the work into two configurations. The
plan runs them in order: **mode-1** (chart-managed Secret + bundled services) proves the
platform end-to-end on GKE with the fewest variables, **mode-2** (`existingSecret` +
external everything) is the production *shape* — all backing services Google-managed,
Workload Identity, private nodes. The split is chart-enforced; the ordering is
deliberate risk reduction, one variable class at a time.

**What lands here, stated once so nothing downstream has to hedge.** This plan builds
and proves the production *configuration*; it does not certify the result as ready for
production *traffic*, and the two are not the same claim. Three conditions hold at the
end of slice 5, none of them fixable inside this plan's scope:

- **Sustained operation.** No production code calls `Sandbox.Destroy`
  ([#64](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/64)), so every
  completed session leaves its pod holding its CPU request. A finite acceptance battery
  passes; a deployment that keeps running fills its node pool. Slice 4 measures the rate
  so the prerequisite has a number attached, which is evidence — not a fix.
- **Transport security.** This delivery terminates no TLS of its own (Decision 8), so
  it exposes nothing.
- **Host-level isolation of untrusted code.** Sandboxes run on GKE Standard's default
  runtime, because gVisor and today's in-pod gate are mutually exclusive (Decision 1).

So the acceptance runs below are **staging-grade**, and the phrase "production" in this
plan always means the shape of the deployment, never a readiness verdict. Production
readiness is those three prerequisites plus this plan, and the plan-level Acceptance
section repeats the list rather than leaving it here.

Deliberately **not** in this plan. Every deferral below is a filed issue, not a
promise to file one — the backlog is GitHub issues and this plan does not hold any of
it:

- **Vertex AI model provider.** The user decision for this deployment is the existing
  public Anthropic-protocol endpoint, which design principle 4 serves with zero code.
  Vertex's shape is structurally different (`:streamRawPredict` URL, `anthropic_version`
  in the body, OAuth) — but `anthropic-sdk-go` ships a `vertex` package whose
  `WithGoogleAuth` option does the whole adaptation, so
  [#236](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/236) is a small adapter,
  not a rewrite.
- **gVisor for gated sessions / the standalone-gate redesign.** Since plan 19 (#65) the
  chart *can* place sandbox pods on a hardened runtime: `sandboxRuntimeClass.name` sets
  `runtimeClassName` on every sandbox pod and `handler` defaults to `runsc`. So gVisor is
  now one Helm value away — which makes the boundary of that value the thing this plan
  must state, because it is not documented anywhere else:

  > **Operator hazard.** Setting `sandboxRuntimeClass.name` to a gVisor RuntimeClass
  > makes every **gated** session unusable. In-pod iptables under GKE Sandbox is
  > impossible (Ground truth), `cmd/gate/main.go` returns the `gaterun.Setup` error
  > before it listens, and the HEALTHCHECK that admits the sandbox therefore never
  > passes. The failure is safe — no session ever runs with a gate that is not
  > enforcing — but it is total, and nothing in the chart warns about it.
  >
  > "Gated" is wider than it looks, and the deploy guide must say so in these words:
  > the executor asks for a gate when a session's networking is `limited` **or it has
  > any vault attached** (`gateSpec`, `internal/executor/executor.go`), because vault
  > credentials are injected at the gate and never handed to the sandbox. So a session
  > with unrestricted networking and no `allowed_hosts` is still gated the moment it
  > carries a credential. The unaffected set is exactly: unrestricted networking **and**
  > no vaults.

  Closing that gap properly — gVisor *and* working gated sessions — means moving egress
  enforcement to the CNI and running the gate as its own pod. That topology is
  probe-proven feasible and would even delete the `NET_ADMIN` requirement, but scoping it
  against the code found two consequences that make it a plan of its own rather than a
  slice, and both are **security** consequences rather than effort ones:
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
  topology or the two backends keep different enforcement models permanently. It gets
  its own plan under [#237](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/237),
  with this plan's probe evidence attached.
- **Warm-pool sandbox provisioning** (kubernetes-sigs/agent-sandbox `SandboxWarmPool` /
  `SandboxClaim`): cold-start optimization, its own plan under
  [#238](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/238). Two integration hazards
  belong in that plan's problem statement rather than being rediscovered there. The gate sidecar
  and the `limited` route-flush init container are **create-time pod-spec facts**, so a
  pool of pre-created pods cannot serve a `limited` session that was not anticipated when
  the pod was created; a design that pools one template for all sessions would silently
  degrade `limited` to full egress. That is an integration bug to design against — with
  per-topology pools, a cold-path fallback, or refusing a claim whose topology the pool
  cannot satisfy — not an inherent property of warm pools. And sandbox teardown must
  exist first
  ([#64](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/64)): with no
  production caller of `Sandbox.Destroy`, a fixed-size pool drains and every new session
  blocks on binding. That one *is* a hard prerequisite — a pool without reclamation is a
  pool that empties exactly once.
- **A separate namespace for sandbox pods.** The executor creates sandboxes in the
  release namespace and holds `create` on pods there, so a compromised executor process
  could mount the platform Secret — the sandbox itself cannot, running with
  `automountServiceAccountToken: false`. Splitting the namespace is worth materially more
  for runtime exposure than any change to how the Secret is *delivered* (Decision 6), and
  it is a small chart-and-RBAC change; it is out of scope here only because it is
  platform-wide rather than GCP-specific —
  [#239](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/239).
- **A GCS-native blob backend.** The existing S3 backend speaks to GCS through the XML
  API once slice 1 lands; a `cloud.google.com/go/storage` backend would only remove the
  HMAC dependency, so it is
  [#240](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/240) rather than a slice here.
- **An Ingress/Gateway chart template.** The deploy docs get a worked Gateway +
  HTTPRoute example; a chart template is speculative until someone asks.
- **FQDNNetworkPolicy as an egress mechanism.** It cannot substitute credentials (it is
  connection-level), and its validation on Standard was blocked by a gVisor-node DNS
  anomaly (Ground truth). The gate covers egress control; FQDN policy stays out.
- **Workspace persistence** (e.g. GCS FUSE for session outputs, as Google's
  anthropic-agent-sandbox reference does): a product-semantics change, and one that
  already has its tracker —
  [#28](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/28) frames it as
  per-session workspace continuity and affinity, a precondition for HA or multiple
  executors rather than a GCP concern. It stays gated on the open wire question of what
  the reference platform's workspace-persistence behavior actually is (resolve per
  CLAUDE.md's order). Nothing about it belongs in this plan; the GCS FUSE mechanism is
  an implementation option for #28, not a deployment choice.

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
- **Sandbox workspace and containment (read from this repo, not measured on GCP; state
  as of plan 19/#65).** That plan gave `sandbox.Spec` a `Hardening` both backends apply
  at create — but "both backends" is where a GCP plan has to read the fine print, since
  each applies only what its runtime can express. **On Kubernetes**, what is on by
  default is a CPU limit and the `NET_RAW`/`SETUID`/`SETGID` drops, extended from gated
  sandboxes to every sandbox, with privilege escalation forbidden alongside them — that
  last one rides on the drop set rather than standing alone, so `SANDBOX_CAP_DROP=none`
  removes it too. A memory cap, a read-only rootfs and a non-root uid are opt-in.
  `runtimeClassName` is opt-in as well but sits outside `Hardening`: it is the
  Kubernetes provider's own `Config.RuntimeClass`, fed by `SANDBOX_K8S_RUNTIME_CLASS`.
  Every knob is a `SANDBOX_*` variable whose zero value applies nothing. So most of the
  containment this plan originally scoped as its own work already exists. **Three** gaps
  remain:
  - **No seccomp profile.** No Go code and no chart template sets `seccompProfile`, so
    sandbox pods run under the runtime's unconfined default rather than
    `RuntimeDefault` — a gap `docs/self-hosted-security.md` already states as a
    platform property ("the platform authors no profile"). Slice 2.
  - **No bound on the workspace's disk, and no way to place the pod.** The workspace is
    the container's writable layer on the node boot disk (`mkdir -p /workspace` in the
    pod command), and neither `ephemeral-storage` nor an `emptyDir` `sizeLimit` appears
    anywhere in the repo; nor does the sandbox pod spec carry a `nodeSelector`,
    affinity, or tolerations, and the provider's `Config` exposes only kubeconfig,
    context, namespace, net-setup image and RuntimeClass. Untrusted agent code can
    therefore fill the node disk, and there is no supported way to keep it off the
    nodes the platform's own components run on. Both are slice 2, argued in Decision 10.
  - **No per-pod process limit — and this one this plan cannot close in code.**
    `Hardening.PidsLimit` is **Docker-only**: the Pod API carries no per-pod pids limit,
    so the Kubernetes provider ignores a configured value and logs that it did
    (`warnUnenforceablePidsLimit`), with the asymmetry already registered in
    docs/DIVERGENCES.md. On GKE the equivalent control is the kubelet's `podPidsLimit`,
    which is **node configuration** — so it belongs to the staging Terraform's node pool
    and the deploy guide, not to a slice of platform code. A fork bomb in a GKE sandbox
    is bounded by nothing the platform sets today; slice 4 sets it on the node pool and
    slice 5 documents it as a required node setting rather than an optional one.

  Lifecycle is otherwise sound: `Provision` is adopt-or-create per work item and
  skills/files are re-materialized per work item (`internal/executor/executor.go`), so a
  replacement sandbox recovers its mounts; what a sandbox death loses is agent-written
  files and shell snapshots — the documented cattle-not-pets trade.

## Decisions

1. **GKE Standard now; Google's Autopilot shape is the destination.** Google's
   managed-agents sandbox reference runs Autopilot + gVisor + a warm pool + FQDN egress
   policy, and that is where this platform should end up. Exactly one link in that chain
   is a **measured constraint** rather than a preference, and the plan is careful to
   claim only that one: gVisor and today's in-pod gate are mutually exclusive, so a
   cluster placing sandboxes on `runsc` cannot serve gated sessions until the standalone
   gate lands (Ground truth, and the operator hazard above). The rest is a **chosen
   rollout order** and is stated as such rather than dressed up as a dependency graph —
   teardown (#64) before warm pools, because a pool with no reclamation empties exactly
   once; warm pools before Autopilot, because Autopilot bills each pod's requested
   resources for its lifetime and adds scale-up latency, which per-session cold pods
   make the expensive shape. That last one is a cost-and-latency judgement, not an
   impossibility: Autopilot runs cold pods perfectly well today, and a team that valued
   node-management removal over per-session cost could reasonably take it first.
   Standard is what the probes proved working end to end: the gate enforces on the
   default runtime, `NET_ADMIN` needs no workload policy, and Pod Security stays under
   operator control.
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
   plaintext ceiling is accepted as a backend-visible behavioural limit**, and the plan
   states it that way rather than as a theoretical ceiling nothing approaches, because
   the code says otherwise: the API bounds a request body at 4 MiB
   (`maxBodyBytes`, `internal/api/wire.go`), places no length bound on `token`,
   `client_secret`, `access_token`, `refresh_token` or `secret_value`
   (`internal/api/vaultcredauth.go`), and seals the JSON object built from them
   (`internal/api/vaultcredentials.go`). So a credential the API accepts today, and that
   OpenBao transit stores, can be refused by the gcpkms backend. That is a real
   difference between two ciphers behind one interface, and it is handled as one:
   - The backend rejects oversized plaintext with its own clear error naming the limit,
     never a raw `InvalidArgument` from the KMS client — and, critically, that error has
     to *reach the caller*, which today it would not. `vaultcredentials.go` wraps a
     `Cipher.Encrypt` failure with `fmt.Errorf`, and `writeError` renders any
     non-`apiError` as a logged internal fault reported as a generic `api_error`. An
     oversize refusal would therefore surface as a 500 saying nothing, with the useful
     message visible only in the server's log. So this is not only a cipher change: the
     `secrets.Cipher` seam grows a **sentinel error for "plaintext too large"**, the
     vault-credential handler classifies it into the `errInvalid` family, and the caller
     gets a 4xx whose message names the limit. A test that accepted a generic 500 would
     bless exactly the behaviour this decision is trying to avoid.
   - It is registered in **docs/DIVERGENCES.md** — not a wire divergence from the
     reference, but the same discipline: a backend that cannot serve every input its
     interface accepts says so in the registry rather than in a code comment.
   - An **API-level boundary test** pins it: a credential create that succeeds under the
     local cipher and is refused, cleanly, under a gcpkms cipher at the boundary.
   - Ciphertext carries a **format marker** from the first commit, so envelope
     encryption can be introduced later without a migration of stored rows. This is the
     one piece of the escape hatch that is cheap now and expensive retrofitted.

   **Envelope encryption** — a local AES-256-GCM data key wrapped by KMS, which removes
   the ceiling entirely and cuts KMS round-trips — is
   [#242](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/242), deferred
   rather than built now: it roughly doubles the backend's complexity, and real vault
   material (OAuth and bearer tokens, environment-variable values) sits three orders of
   magnitude under the limit. Deferred with the marker in place is a decision that can be
   revisited; deferred without it is one that cannot. Tests: the
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
     Terraform provisions the Secret Manager *secrets* (the containers, with their
     replication and IAM), a **bootstrap script** adds the first version of each, and one
     scripted `gcloud secrets versions access` → `kubectl apply` materializes the K8s
     Secret. This is the shape Google's own managed-agents sandbox reference uses, and it
     buys provenance, versioning, audit logging, and reproduce-from-a-clean-project.

     The split between those three steps is the load-bearing part, because a
     freshly-created secret has **no version to access** and the obvious shortcut is
     wrong: putting the values in Terraform's `secret_data` writes every one of them into
     state in plaintext. So the bootstrap script owns value creation, and the shape that
     makes it work is **generate once, deliver twice**: the script generates a value with
     `openssl rand`, writes it to Secret Manager with
     `gcloud secrets versions add --data-file=-`, and separately installs it wherever it
     has to be live. The database password is the case that makes this explicit —
     `gcloud sql users set-password` **does not generate** one, it takes the value it is
     given, so the script must produce the bytes and hand the *same* bytes to Cloud SQL
     and to Secret Manager, both via `--data-file`/stdin rather than a shell variable
     that lands in history. The model key is pasted from its own provider. Terraform
     never sees any of these; it holds names, IAM bindings, and preconditions only.

     One value cannot follow that shape, and the script has to say so: the **GCS HMAC
     secret is readable exactly once**, at `gcloud storage hmac create`. If creation
     succeeds and the Secret Manager write then fails, the secret is gone and rerunning
     cannot recover it — the failure is not convergent by default. So the HMAC step is
     ordered create → store → verify, and on a failed store it **deletes the orphaned
     key** before exiting non-zero, leaving the next run a clean slate. Everything else
     is a no-op on already-populated secrets; the acceptance criterion is
     reproduce-from-clean, not reproduce-by-overwrite, and a half-finished bootstrap has
     to leave the project in a state the next run can finish from.
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
8. **No domain or managed certificate for this delivery — therefore no public
   endpoint.** The user decision for this delivery is to skip domains and certificates
   and reach the control plane by IP. The consequence has to be carried through rather
   than left implicit: `controlplane` serves plain HTTP (`http.Server`, no TLS config),
   so an external load balancer that terminates nothing carries the global
   `x-api-key`, every session's prompts and outputs, and the whole SSE event stream in
   cleartext across the network the LB sits on. This plan therefore **does not expose the
   control-plane Service publicly at all**. Slices 4 and 5 reach it over
   `kubectl port-forward` (and an internal-only LB where a second host is needed), which
   is enough to drive the real `ant` CLI end to end and is what the acceptance runs use.
   The deploy guide carries a Gateway + HTTPRoute + managed-certificate example and
   states plainly that terminating TLS — or fronting the service with IAP or a VPN — is a
   **prerequisite for any exposed deployment**, not an optional hardening step. Nothing
   goes in the chart, and the plan-level acceptance below claims no transport property it
   did not exercise.
9. **A persistent staging environment with one-command create and destroy.** The cluster
   and its backing services stay up between slices rather than being rebuilt per run,
   but teardown must be one command, so that an idle week costs the cents the
   never-destroyed `foundation/` below is worth rather than a running GKE cluster and
   Cloud SQL instance. Provisioning is
   **Terraform** under `deploy/gcp/`, wrapped by Make targets — the same shape Google's
   managed-agents sandbox reference uses. Terraform rather than shell because destroy is
   the operation that must not miss anything: state-tracked teardown is what prevents
   the orphaned-billable-resource failure mode (this plan's probe sessions only avoided
   it by manual sweeps). This adds Terraform as a **developer-tooling** dependency for
   GCP deployment only — never a dependency of the platform, its build, or `make verify`,
   and the Helm chart stays the portable installation path.

   One GCP asymmetry breaks the symmetry of create/destroy, and it is not a detail to
   be discovered on the second run: **KMS key rings and crypto keys cannot be deleted.**
   `terraform destroy` removes them from state and leaves them in the project, so a
   naive configuration collides on the name the next time it runs — and for the crypto
   key the stakes are higher than a name collision, because **the vault ciphertext in
   Postgres is only decryptable by that key**. A teardown that scheduled its key
   versions for destruction would be silent, irreversible data loss discovered at the
   next credential read.

   So `deploy/gcp/` is **two Terraform configurations, not one**. The line between them
   is not "expensive versus cheap" but **"can a rebuild recreate this identically?"**:
   - **`foundation/` — created once, never destroyed.** The KMS key ring and crypto key
     (undeletable, and the only thing that can ever decrypt what they encrypted); the
     Secret Manager secrets; and **the service accounts**, which belong here for a
     reason that is easy to miss — deleting a service account deletes its HMAC keys, so
     an identity in the disposable half would strand the once-readable HMAC secret
     sitting in Secret Manager, valid-looking and dead. `terraform destroy` is not a
     supported operation here; the resources carry `prevent_destroy`, and the Make
     target that tears the environment down does not touch this state. Idle cost is
     cents a month (an active KMS key version, and Secret Manager versions past the
     free allowance), not zero.
   - **`environment/` — created and destroyed freely.** GKE cluster and node pools,
     Artifact Registry, Cloud SQL, the GCS bucket, and the IAM bindings that attach the
     foundation's identities to them. It reads the foundation's key, secret and account
     names through `data` sources, never owning them.

   That also answers the clean-project case a `data` source alone cannot: the first
   `apply` of `foundation/` creates the ring and key; every later environment rebuild
   adopts them. A project that already has a ring documents a one-time
   `terraform import` into the foundation state instead.

   **What a destroy actually costs, said plainly:** `environment/` owns Cloud SQL, so
   tearing it down **destroys the staging database** — and vault ciphertext lives only
   in Postgres (`internal/secrets`: the cipher transforms bytes, Postgres is the
   canonical store). Retaining the KMS key does not bring a deleted row back. That is
   the right trade for a staging environment and this plan makes it deliberately, but it
   means the rebuild proof cannot be "a credential written before teardown reads back
   after" — that credential is gone by design. Acceptance is **create → destroy →
   create** on `environment/`, with `foundation/` untouched, proving the three things a
   rebuild can actually fail at: the second `apply` succeeds with no KMS name collision;
   the bootstrap script converges on the already-populated secrets, **including
   reconciling the GCS HMAC key** against the surviving service account; and a fresh
   vault credential round-trips on the rebuilt stack. An operator who needs the data to
   survive needs a Cloud SQL export/restore step, which this plan does not build and the
   deploy guide says so.
10. **Sandbox pods get a disk limit *and* a node of their own.** The exposure is real
    (Ground truth): agent code can fill the node's boot disk, and nothing in the repo
    bounds it or places the pod.

    An earlier draft of this decision refused the `ephemeral-storage` limit, on the
    grounds that its enforcement action is eviction and a session killed mid-task is a
    worse outcome than the disk pressure it averts. That reasoning compared the wrong
    two things and is abandoned here. Declining the limit does not avoid an eviction; it
    changes **who** gets evicted. An unbounded writer drives the node to `DiskPressure`,
    at which point the kubelet evicts by its own ranking of usage over requests and
    taints the node against new pods — so sessions that did nothing wrong die alongside
    the offender, and the node stops accepting work. With a per-container limit the
    eviction is **targeted and attributable** — the pod that exceeded its own limit is
    the pod that dies, at a threshold the operator chose. Targeted beats arbitrary.
    The real objection to a limit — that a legitimate session outgrows one set too
    small — is a *sizing* problem, and the answer to a sizing problem is a generous
    operator-set value, not refusing the mechanism.

    Two things this does **not** buy, so the decision is not over-read in the direction
    it was just corrected away from. A per-pod limit does not guarantee the node stays
    healthy: node-pressure eviction is a separate mechanism that still fires when local
    storage runs low for any reason, and a limit is only enforced while the kubelet's
    measurement of it is working. And the accounting is one pool, not two — a pod's
    ephemeral-storage usage includes both its writable layer and its `emptyDir` volumes,
    so `emptyDir.sizeLimit` is an **additional per-volume bound inside** that total, not
    coverage of bytes the limit misses. It is also not currently reachable: the provider
    creates its read-only-rootfs volumes with a bare `EmptyDirVolumeSource` and exposes
    no size setting, so an operator cannot set one today. Slice 2 adds the
    `ephemeral-storage` limit as `Hardening` configuration with an empty default —
    today's behaviour stays the default, and the GCP deploy guide sets a real value; the
    per-volume bound is left out of this plan rather than described as available.

    **Node isolation is the second half, and it needs code.** The sandbox pod spec
    carries no `nodeSelector`, affinity, or tolerations, and the Kubernetes provider's
    `Config` exposes only kubeconfig, context, namespace, net-setup image and
    RuntimeClass — the chart's existing scheduling knobs apply to the *executor
    Deployment*, not to the pods it creates. So a tainted sandbox node pool would leave
    every sandbox `Pending` forever. Slice 2 adds node selection and tolerations to the
    provider config, following the `RuntimeClass` precedent exactly: cluster shape is
    provider configuration, not a `sandbox.Spec` field, because which nodes a cluster
    has is a property of the cluster and not of a session.

    What each half buys, kept separate so neither is over-claimed: the **limit** stops a
    sandbox from taking down its co-tenants, which on this pool are other sandboxes; the
    **dedicated pool** removes the platform's own components from that blast radius by
    not putting them on the same host. The pool is host isolation only — it does nothing
    about network reachability, and a sandbox on it can still reach the control plane's
    Service exactly as before; the egress gate, not the scheduler, is what governs that.
    Node isolation alone would only relocate the tenancy problem, which is why this
    decision does both.
11. **The GCS deployment identity needs bucket-read on top of object-admin.** Measured
    consequence of a code path, not a preference: `s3.New` calls `BucketExists`
    unconditionally before any object work (`internal/blob/s3/s3.go`), minio-go issues
    that as `HEAD Bucket`, and GCS maps it to `storage.buckets.get` — which
    `roles/storage.objectAdmin` does **not** grant. An identity holding only
    `objectAdmin` therefore fails at process startup with 403, before any of this plan's
    object-path work matters. This plan's own GCS probe missed it by granting
    project-wide `roles/storage.admin`, which is exactly the kind of gap a probe run
    under a convenient role produces. Slice 5 grants `roles/storage.objectAdmin` **plus**
    `roles/storage.legacyBucketReader`, both scoped to the single bucket, and its
    acceptance runs under the exact service account the deploy guide specifies rather
    than under the operator's own credentials.

    One wrinkle the guide should carry rather than leave to be rediscovered: with
    `BLOB_REGION` empty, minio-go resolves the bucket's location first
    (`GET Bucket?location`) and caches it, so construction is two bucket-level calls,
    not one. `legacyBucketReader` covers both — it grants `storage.buckets.get` — so the
    role choice is unaffected; setting `BLOB_REGION` explicitly just removes the extra
    round trip. The role is sufficient, not permission-minimal: the genuinely minimal
    answer is a pre-created-bucket mode that skips `BucketExists`/`MakeBucket` and drops
    the bucket privilege entirely, which is a better long-term answer for every
    S3-compatible backend rather than just GCS, and is
    [#241](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/241)
    rather than smuggled into this plan.

## Slices

Each slice is one PR unless noted; TDD per CLAUDE.md (the failing test first).

- **Slice 1 — GCS delete convergence (`internal/blob/s3`).** `Delete` treats
  `NoSuchKey` from `RemoveObject` as success (the same error-code check `Get` already
  uses), restoring the contract's convergence on GCS while changing nothing on
  S3/MinIO (which never produce it). Test: a stub S3 endpoint whose DELETE answers
  `404 NoSuchKey` — asserted failing against the pre-fix code first (the
  fixture-must-force-the-transformation lesson); the MinIO-backed contract suite stays
  as-is and keeps passing.
- **Slice 2 — sandbox pod placement and bounds (`internal/sandbox` +
  `internal/sandbox/k8s` + chart).** Plan 19/#65 delivered most of what this slice was
  originally scoped to add — CPU limit, capability drops, no-privilege-escalation,
  non-root, read-only rootfs, `runtimeClassName` — as `SANDBOX_*` operator configuration
  whose zero value applies nothing, which is exactly the shape this plan wanted. Three
  things it did not, each small and each following a precedent already in the file:

  - **`seccompProfile: RuntimeDefault`.** No Go code and no chart template sets it, so a
    sandbox pod runs under the runtime's *unconfined* default with every syscall the
    kernel offers. `RuntimeDefault` is the container runtime's own curated filter — what
    an ordinary Docker container already gets — so setting it on Kubernetes closes a
    divergence between our two backends rather than opening a compatibility question.
    Set it on the pod spec, covering the sandbox container and the gate sidecar alike.
  - **An `ephemeral-storage` limit**, as a `Hardening` field
    (`EphemeralStorageBytes`, env `SANDBOX_EPHEMERAL_STORAGE_BYTES`, matching
    `MemoryBytes`/`SANDBOX_MEMORY_BYTES` in name, units and zero-means-unbounded
    semantics) — Decision 10. Kubernetes-only, the mirror image of `PidsLimit` being
    Docker-only: Docker's writable-layer quota needs a specific storage driver, so the
    asymmetry is registered in docs/DIVERGENCES.md the way #65 registered its own rather
    than faked.
  - **Node selection and tolerations** on the sandbox pod, as provider `Config` fed by
    `SANDBOX_K8S_NODE_SELECTOR` / `SANDBOX_K8S_TOLERATIONS` — the `RuntimeClass`
    precedent, and for the same reason: which nodes a cluster has is a property of the
    cluster, not of a session. Without this, Decision 10's tainted sandbox pool would
    leave every sandbox `Pending`, so it is a prerequisite for slice 4's environment
    rather than an enhancement.

    `RuntimeClass` is a scalar and these are not, so the encoding is part of this
    slice's contract rather than something to settle at implementation time:
    **`SANDBOX_K8S_NODE_SELECTOR` is comma-separated `key=value` pairs** (the form
    `kubectl` already uses for label selectors), and **`SANDBOX_K8S_TOLERATIONS` is a
    JSON array of the Kubernetes `Toleration` shape** — because tolerations carry
    `key`/`operator`/`value`/`effect`/`tolerationSeconds` and any flat encoding of that
    is a worse dialect of JSON. Both follow #65's rule that a malformed value **fails
    executor startup** rather than silently reverting to the default; the empty value
    applies nothing.

  Chart surface for all three. Two of them — the storage limit and the placement knobs —
  default to today's behaviour exactly. **`seccompProfile` deliberately does not**: it
  is applied unconditionally, so pods that ran unconfined now run under `RuntimeDefault`,
  and that behaviour change is the entire point of shipping it rather than a knob nobody
  turns on. Saying so matters beyond tidiness — it is what lets the
  shared-responsibility row move onto the platform, which an opt-in default could not
  justify. Unit-test at the podSpec level, and run the shared contract suite on kind
  **including a gated session**: a seccomp filter that interfered with the gate's
  `iptables-restore` under `NET_ADMIN` would surface there and nowhere else, and this
  slice must prove it does not.

  Docs move with the code in the same PR, and one of them is not optional:
  `docs/self-hosted-security.md` currently records "the platform authors no profile" as
  a platform property and carries the corresponding shared-responsibility row on the
  operator. Both go stale the moment `RuntimeDefault` lands, so this slice moves that
  row onto the platform the way plan 19 moved its own.

  Worth stating so nobody over-reads the result: that same document lists exactly three
  reasons a strict `restricted` Pod Security Admission label rejects a sandbox pod —
  no `runAsNonRoot: true`, no `seccompProfile`, and a `capabilities.drop` that does not
  contain `ALL`. This slice removes the second; `SANDBOX_CAP_DROP=ALL` already removes
  the third, leaving `runAsNonRoot` as the one blocker no configuration reaches. So
  slice 2 is a step toward `restricted` compliance, not the arrival — and for a **gated**
  pod it is not even that, since the gate sidecar adds `NET_ADMIN`, which `restricted`
  forbids outright. Nothing in this plan should be read as making sandbox namespaces
  `restricted`-ready.

  Deliberately not in this slice, because no code can carry it: the **per-pod process
  limit**. `Hardening.PidsLimit` is Docker-only and the Pod API has no equivalent, so on
  GKE that containment is the kubelet's `podPidsLimit` — node configuration, set on the
  sandbox node pool in slice 4 and documented as required in slice 5.
- **Slice 3 — `internal/secrets/gcpkms` + chart knob + `cmd/` wiring.** The direct-KMS
  cipher of Decision 3: the ciphertext format marker, the 65536-byte plaintext guard
  behind a **`secrets` sentinel error** that the vault-credential handler classifies
  into the `errInvalid` family — without that classification the refusal is a generic
  500 and the limit reaches nobody but the log — and the `secrets-backend=gcpkms` env
  plumbing
  (key resource name; ADC for auth) through the chart's Secret keys and
  `map.secretsEnv`; docs for the Workload Identity binding. Hermetic contract + live
  tier as above. Two things land in this same PR rather than being batched later,
  because they are what makes the accepted limit honest: the **docs/DIVERGENCES.md
  entry** recording that this backend cannot serve every input the API accepts, and the
  **API-level boundary test** — a vault-credential create that succeeds under the local
  cipher and is refused under gcpkms at the boundary **with a 4xx whose message names
  the limit**, rather than a unit test of the cipher in isolation that never proves the
  API surfaces the refusal usefully. A test that accepted a 500 here would certify the
  defect.
- **Slice 4 — staging environment + mode-1 acceptance on GKE.** `deploy/gcp/` Terraform
  and its Make targets (Decision 9), in the two configurations that decision requires:
  **`foundation/`** — the KMS key ring and crypto key, the Secret Manager secrets, and
  the service accounts, applied once and never destroyed — and **`environment/`** —
  cluster, Artifact Registry, Cloud SQL, the GCS bucket, the Workload Identity and IAM
  bindings that attach the foundation's identities to them,
  and the **dedicated tainted sandbox node pool** of Decision 10, whose node system
  config also sets the kubelet's `podPidsLimit` (the per-pod process containment the Pod
  API cannot express). They are used to create the environment this slice then deploys
  into, so the scripts are exercised rather than merely written. Teardown is proven as
  **create → destroy → create** on `environment/`, against the three things a rebuild
  can actually fail at (Decision 9): the second `apply` succeeds with no KMS name
  collision; the bootstrap script converges on already-populated secrets, including
  reconciling the GCS HMAC key against the surviving service account; and a **fresh**
  vault credential round-trips on the rebuilt stack. Explicitly *not* an old credential
  surviving — `environment/` owns Cloud SQL, so the destroy takes the ciphertext with it
  by design.
  Then: Cloud Build images; `helm install` with bundled services; the real `ant` CLI
  end-to-end over `kubectl port-forward` (Decision 8 — nothing public): sessions, bash
  and file tools, HITL, the `skill-answer` and `file-answer` evals, a vault-attached
  `limited` session proving allowed_hosts enforcement and credential substitution
  through the gate on K8s; OTel Collector shipping traces to Cloud Trace. No planned
  code — anything found feeds back as fix PRs. This is also the first real execution of
  [#75](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/75) (publish
  images, run a real end-to-end `helm install` acceptance) — on GCP rather than
  generically, so that issue is narrowed by this slice rather than closed by it.

  Two things this run must **measure and record rather than discover**: with no
  production caller of `Sandbox.Destroy` (#64), every session leaves its pod holding its
  CPU request — record how many sessions the staging pool absorbs before scheduling
  degrades, because that number is what turns #64 from a tidiness issue into the sizing
  constraint it actually is; and the workspace disk a representative session consumes,
  which is the input to Decision 10's node-pool sizing guidance in slice 5. Acceptance
  record → docs/HISTORY.md.
- **Slice 5 — mode-2 acceptance + `docs/deploy-gcp.md`.** Cloud SQL (private IP,
  managed pooling off, `sslmode=require`, and a **dedicated owner of the platform's own
  database**), GCS (pre-created bucket,
  HMAC key, single-bucket `roles/storage.objectAdmin` **plus**
  `roles/storage.legacyBucketReader` per Decision 11), KMS (Workload Identity, the
  foundation's key), the bootstrap script and pre-created Secret of Decision 6 (ESO
  documented as the alternative), private nodes + Cloud NAT, AR mirrors. Re-run the
  slice-4 acceptance battery — **under the deploy guide's own service account, not the
  operator's credentials**, which is the only way Decision 11's privilege set is
  actually tested.

  The database role needs the same treatment, and for the same reason the GCS role did.
  What the code establishes is the privilege the platform *requires*: `store.Open`
  migrates only the database its DSN names, and the 15 migrations are ordinary schema
  DDL with no role or database creation. That is not what Cloud SQL *grants* — a
  built-in PostgreSQL user is created with `cloudsqlsuperuser`, which carries `CREATEDB`
  and `CREATEROLE`, so accepting "it only needs schema DDL" as "it only has schema DDL"
  would repeat Decision 11's mistake one layer down. This slice specifies the
  custom-role or explicit-revocation sequence that narrows it, and asserts the result
  rather than assuming it: `rolcreatedb = false`, `rolcreaterole = false`, and ownership
  of the platform database and nothing else. Write the deploy guide: the Gateway +
  managed-certificate example and
  Decision 8's do-not-expose-without-it statement, the collector config, the
  node-local-dns DNS-carve-out note, the sandbox node-pool sizing guidance from slice
  4's measurements, `podPidsLimit` as a **required** node setting rather than an
  optional one, the gVisor operator hazard with its true affected-session boundary, and
  backups — which under KMS lose the OpenBao pairing complexity but must still name the
  HMAC-key and KMS-key lifecycles, the second of which is now a data-loss hazard rather
  than a rotation chore. Acceptance record → docs/HISTORY.md; README status line updated
  in the same PR.

## Acceptance (plan-level)

The real `ant` CLI drives the platform on GKE in mode-2, reached over
`kubectl port-forward` — all backing services Google-managed; KMS and the collector
authenticate via Workload Identity with no downloaded key material; both evals pass;
gate enforcement and credential substitution are proven in a `limited` session; traces
are visible in Cloud Trace; and the deploy guide reproduces the whole setup from a clean
project. Teardown is proven on `environment/` — `terraform destroy` removes the cluster,
Cloud SQL and the bucket, `foundation/` is deliberately untouched (Decision 9), and a
second `apply` rebuilds a working stack against the surviving KMS key and service
accounts. Both acceptance runs recorded in docs/HISTORY.md.

**Every static credential that remains in-cluster is rotatable. Exactly one of them is
unscoped**, and the guide says which rather than letting "scoped and rotatable" cover
the set. Scoped, each asserted rather than assumed: the GCS HMAC pair (single bucket;
its removal is the GCS-native-backend follow-on), the model key (the provider's own),
and the **Cloud SQL user, narrowed to ownership of the platform's own database** —
DDL-capable there because `store.Open` migrates the database its DSN names, and
narrowed *away* from the `cloudsqlsuperuser` a built-in user is created with, with
`rolcreatedb`/`rolcreaterole` checked false in the acceptance run (slice 5). Unscoped:
the **management API key, which authorizes every management `/v1` route with no role or
resource model** (`requireAPIKey`). That one is unscoped by design, and narrowing it is
a platform change rather than a deployment one.

**What this acceptance does not establish** — the same three conditions the plan opens
with, repeated here so a reader who arrives at the acceptance never has to go looking:

- **Sustained operation.** No production code calls `Sandbox.Destroy`
  ([#64](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/64)), so pods
  accumulate for as long as the deployment runs. A finite battery passes; a long-lived
  deployment fills its node pool. Slice 4 measures the rate. **#64 is a prerequisite for
  production traffic, not for this plan.**
- **Transport security.** Nothing here terminates TLS (Decision 8), and nothing here is
  publicly exposed. Exposing this deployment requires TLS, IAP or a VPN first.
- **Host-level isolation of untrusted code.** Sandboxes run on GKE Standard's default
  runtime. `docs/self-hosted-security.md` recommends a hardened runtime such as gVisor
  or Kata for model-directed commands, and this plan cannot adopt it without breaking
  gated sessions (Decision 1, and the operator hazard above). The interim boundary is
  the disk limit and dedicated tainted sandbox node pool of Decision 10, the node's
  `podPidsLimit`, and the containment plan 19/#65 already applies; the real answer is
  the standalone-gate plan.

Meeting those three is what turns this plan's production *shape* into production
*readiness*. This plan delivers the shape and says so; it does not quietly claim the
rest.

## Open questions

*(none — the plan's questions were resolved before approval: no domain/TLS for this
delivery, a persistent staging environment with scripted teardown, and the secret-delivery
mechanism, are Decisions 8, 9 and 6 respectively. Every trade this plan declines to make
is argued in a Decision and named in the acceptance above rather than left open.)*
