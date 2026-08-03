# HISTORY.md — acceptance and decision records

What a changelog structurally cannot hold: acceptance-run records, review-hardening
records with their evidence, decisions evaluated and rejected, and archived plans'
progress summaries. A change's **narrative** is written once, in
[CHANGELOG.md](../CHANGELOG.md) — never duplicated here (the one-writer rule; CLAUDE.md →
"Iteration workflow"). The as-built system description is
[ARCHITECTURE.md](./ARCHITECTURE.md).

Provenance: this file began 2026-07-16 as the verbatim completed-work archive moved out
of [STATE.md](../STATE.md), and documents — [DIVERGENCES.md](./DIVERGENCES.md) above
all — cite its section headings as evidence anchors. On 2026-07-18 the per-PR delivery
narratives were verified section-by-section against CHANGELOG.md and pruned (git history
is the backstop); every cited heading is preserved below, and anything still under one is
recorded nowhere else.

---

## Delivery slices

| # | Slice | Status |
|---|---|---|
| 0 | `internal/domain` (Anthropic-native types) + `internal/telemetry` (OTel/OTLP, context propagation) | ✅ Done |
| 1 | Postgres schema + migrations (`internal/store`), reserved multi-tenant columns | ✅ Done |
| 2 | Control plane CRUD (agents / environments / sessions) + optimistic versioning + ID prefixes + `x-api-key` auth | ✅ Done |
| 3 | Append-only event log (seq allocation) + `POST /events` + SSE stream (`event_start` / `event_delta` reconciliation) + `span.*` emitted from the same point as OTel spans | ✅ Done |
| 4 | `ModelProvider` (config-driven: protocol / model / base_url / api_key) + `model_providers` routing; first provider passing a single model turn; verify a custom `base_url` works | ✅ Done |
| 5 | Brain orchestration loop (replay → assemble provider request → write Anthropic-native events). No adk runtime. | ✅ Done |
| 6 | tool-exec queue (Postgres `FOR UPDATE SKIP LOCKED`) + executor + Docker sandbox provider + built-in toolset really executing inside the sandbox | ✅ Done |
| 7 | Permission policies + `requires_action` / `user.tool_confirmation` approval round-trip | ✅ Done |
| 8 | Wire-compatible work API (`/work/poll`, `/ack`, `/heartbeat`, `/stop`) + distributable BYOC worker + `traceparent` propagated through work items | ✅ Done (PRs A, B, C1, C-list, C2a, C2b, C3, C2b-2, C-meta, C-stats — per-PR narratives in CHANGELOG.md § 0.1.0) |
| 9 | Kubernetes sandbox provider + Helm chart (with OTLP endpoint values) | ✅ Done (K8s `sandbox.Provider` on the shared contract suite via kind, `SANDBOX_BACKEND` selection, Helm chart, compose stack) |

---

## GCP deployment plan (20) — archived 2026-08-03, all five slices delivered (#20)

docs/plan/20_gcp-deployment.md is archived complete. What it set out to deliver — a
supported, documented, acceptance-proven path to run the platform on GKE with
Google-managed backing services — exists in `deploy/gcp/` (a `foundation/` applied once
and never destroyed, an `environment/` created and destroyed freely), in the chart's
Cloud-native seams, and in [docs/deploy-gcp.md](./deploy-gcp.md). The plan itself landed
approved in PR #243. Slice 1: GCS delete convergence in `internal/blob/s3`, in two PRs —
deleting an already-deleted object converges (PR #245), then a bare 404 stops reading as a
missing object (#244, PR #252). Slice 2: sandbox containment and
placement, in three PRs — the runtime's seccomp filter (#246), an opt-in ephemeral-storage
cap (#247), and pinning sandbox pods to a node pool of their own (#248). Slice 3:
`internal/secrets/gcpkms`, the Cloud KMS credential cipher, with its own live tier and its
ceiling made honest (PR #249). Slice 4a: the staging Terraform (PR #250), with the
Cloud Build API moved into `foundation/` when the documented order proved unrunnable
without it (PR #253); slice 4b: the **mode-1** acceptance on GKE (PR #256, record below).
Slice 5a: the mode-2 configuration —
private nodes, Cloud NAT, private-IP Cloud SQL, the Docker Hub mirror, and a platform
database role outside `cloudsqlsuperuser` — plus the deploy guide (PR #259). Slice 5b: the
**mode-2** acceptance run, its three findings, and this archiving (the record immediately
below).

The plan states its own limits and they survive it: this delivery is production in
*shape*, not a readiness verdict. The three prerequisites it names — a production caller of
`Sandbox.Destroy` ([#64](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/64)),
TLS in front of the control plane, and host-level isolation for model-directed commands —
are unmet by design, each for a reason written down rather than deferred silently. The
follow-on work it scoped rather than built is filed:
[#236](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/236) (Vertex AI
provider), [#237](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/237) (the
standalone-gate plan that would unblock gVisor), and
[#238](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/238) (warm-pool
sandboxes).

## GCP mode-2 acceptance — Cloud SQL + GCS + Cloud KMS on GKE, real `ant` CLI (run 2026-08-03) — ✅ passed

The production-shaped half of plan 20, run end to end against the live project
`opensdlc-managed-agents`: the platform on GKE with **no bundled services at all**, every
credential in a pre-created Secret, and each backing service reached through the deploy
guide's own identity rather than the operator's. Nothing was exposed — the whole battery
ran over `kubectl port-forward` (Decision 8).

**The build-up, and what it incidentally re-proved.** `environment/` had been destroyed
after the mode-1 run, so this run began by rebuilding it on the surviving `foundation/` —
which is the Decision-9 teardown proof executed a second time, in mode 2:
`terraform apply` created all **34** resources with no KMS name collision (the key ring and
crypto key are only *read* here); `make gcp-bootstrap` reconciled the surviving Secret
Manager secrets against freshly created infrastructure — the surviving database password
authenticated against the **new** Cloud SQL instance, and the stored GCS HMAC pair against
the **new** bucket, both by authenticated call rather than by inspection; and a vault
credential created *after* the rebuild round-tripped through the gate (below). The four
images were one Cloud Build (3m07s); `controlplane`, `brain` and `executor` are three tags
on a single digest (`sha256:d083eb16422c…`), so they cannot drift, and `gate` is the fourth.

**The database role is not a Cloud SQL superuser — asserted against the real thing.**
`make gcp-db-init` passed on its first run against a live Cloud SQL instance, which is the
first execution of slice 5a's hardening outside its unit suite:

```
NOTICE:  dbinit: role=map superuser=f createdb=f createrole=f bypassrls=f
         in_cloudsqlsuperuser=f owns=map encrypted=t
ok: map is a non-superuser, outside cloudsqlsuperuser, owning only map
```

The assertion slice 5a added last — that the role's membership set is *exactly* itself, its
group role and `pg_database_owner` — was the one flagged as most likely to false-fail on a
real instance. It did not: Cloud SQL's `CREATE ROLE … LOGIN IN ROLE` grants membership in
one direction only, and the `GRANT map TO CURRENT_USER` the file needs for the ownership
transfer runs the other way, so the set closes cleanly.

**Acceptance, all through the real `ant` CLI 1.21.0.**

- *Sessions and the async loop.* Wire-correct prefixes throughout (`agent_`, `env_`,
  `sesn_`, `sevt_`, `vlt_`, `vcrd_`, `sesrsc_`, `file_`, `skill_`). A bash turn closed the
  loop — `agent.tool_use` → executor → `agent.tool_result` → `agent.message` →
  `status_idle end_turn` — and the tool's output was `map-sesn-292hrehmcbdr7t74w1f2r6dp`,
  the sandbox pod's own hostname, so it ran in the per-session pod rather than the executor.
- *Sandbox placement, bounds, and the mirror.* The pod landed on a sandbox-pool node with
  `map.opensdlc.dev/sandbox` in both its nodeSelector and its toleration,
  `allowPrivilegeEscalation: false`, and `NET_RAW`/`SETUID`/`SETGID` dropped. Its image was
  `…/map-dockerhub/library/debian:stable-slim` — slice 5a's Docker Hub mirror serving the
  sandbox path, which mode 1 never exercised because its third-party images were the
  bundled services instead.
- *`file-answer`.* A file uploaded through the Files API landed in GCS
  (`gs://…-map-blob/files/file_ysnjte6v8wwj5mq6zqvr1y09`), mounted as a session resource,
  and the model read the mount and returned `marlin-000a256b` — a token generated for this
  run that appears in no prompt.
- *`skill-answer`.* A skill uploaded as a zip, injected as Level-1 metadata and
  materialized: the model read `SKILL.md`, then `answer.txt`, and returned
  `kestrel-a8dce216` exactly.
- *HITL.* An `always_ask` toolset suspended the first tool call —
  `stop_reason: {"type":"requires_action","event_ids":[…]}` — and a `user.tool_confirmation`
  with `result: allow` released it into `agent.tool_result` → `agent.message` → `end_turn`.
- *The egress gate, with a vault, on Cloud KMS.* For a `limited`, vault-attached session the
  executor attached the gate as a **native Kubernetes sidecar** (an `initContainers` entry
  with `restartPolicy: Always`) holding `NET_ADMIN`, beside a sandbox container that drops
  `NET_RAW`/`SETUID`/`SETGID`. Three things were then shown separately:
  - The sandbox held only the placeholder: `printenv M2_TOKEN` returned
    `vltph_0875d38c504402092c65b2a398b38113`.
  - The origin received the real secret. A plain-HTTP request through the gate to an
    allowed host echoed back an `Authorization` value whose SHA-256 was
    `dc897c7e6d532655da9e5c75d4ef278b51d157fd404fa5e9c8dd210e3a793de1` — byte-identical to
    the stored secret's hash. Only the hash was ever printed. Since the ciphertext lives in
    Cloud SQL and the key in Cloud KMS, this single equality proves the whole mode-2
    credential path: store → KMS decrypt → substitute at the gate.
  - Both fail-closed layers hold. A disallowed host was refused by the gate
    (`403 host not permitted by the environment's networking policy`), and a connection
    that bypassed the proxy entirely — straight to a public address from inside the
    sandbox — **timed out**, because the gate's rules live in the pod's network namespace.
    The mode-1 record proved the policy; this adds the structure underneath it.

**Every backing service was reached with this deployment's own credential rather than the
operator's — three different kinds of credential, and the distinction matters.**

- *Cloud KMS: Workload Identity, no key material anywhere.* Cloud Audit Logs cannot show
  this — KMS crypto operations are data-access logs and are off by default — so it was
  established directly: a pod running under the chart's controlplane ServiceAccount asked
  the GKE metadata server who it was and got `map-controlplane@…iam.gserviceaccount.com`;
  the same probe under the executor's ServiceAccount got `map-executor@…`. Neither is the
  node service account, which is what the `iam.gke.io/gcp-service-account` annotations
  exist to produce, and the node account holds no role on the key.
- *GCS: the `map-storage` service account's HMAC pair* — a static credential belonging to
  that account, not Workload Identity. Its only roles are `roles/storage.objectAdmin` and
  `roles/storage.legacyBucketReader` **on the one bucket**, and the second is not
  decoration: `s3.New` calls `BucketExists` before returning a store, which needs
  `storage.buckets.get`, which `objectAdmin` alone does not grant. All three processes
  logged `object storage configured` at startup, so that call succeeded — which is
  Decision 11's privilege set proving itself sufficient as well as minimal.
- *Cloud SQL: **no Google identity at all**.* This run took the direct private-IP path the
  guide documents — `sslmode=require` from inside the VPC — so the credential is the
  platform's own non-superuser **database** role and its password. The Cloud SQL Auth Proxy
  path, which is the one that would involve `roles/cloudsql.client` and a per-component
  Google identity, was not exercised; the guide already says the chart templates no sidecar
  for it.

**Traces reach Cloud Trace, and the pagination trap reproduced.** 82 traces over the run,
including 11 `model_turn`, 4 `google.cloud.kms.v1.KeyManagementService/Encrypt` — the KMS
calls traced, which mode 1 had nothing to show — 2 `egress_request` from the gate, 9
`GET /internal/v1/gate/config`, and the HTTP server spans for every management route
exercised. The first page came back with 22 traces and a `nextPageToken`; following it
produced 60 more. The mode-1 record's warning about an empty or short first page now has a
second data point.

**Four findings, all fed back as fixes in the slice-5b PR — and the first one is a real
defect in the delivered teardown path.**

0. ***`make gcp-env-destroy` could not complete on any environment that had
   `make gcp-db-init` run against it.*** The destroy removed 21 resources and then stopped:

   ```
   Error: failed to delete database "map". Detail: pq: must be owner of database map.
   (Please use psql client to delete database that is not owned by "cloudsqlsuperuser")
   ```

   The cause is slice 5a's own design meeting the Admin API. `dbinit.sql` deliberately
   transfers the platform database to the platform's own role — that is the containment the
   whole slice exists to establish — and the Admin API's database-delete runs as
   `cloudsqlsuperuser`, which is not a member of that role and therefore cannot drop what it
   owns. Slice 4b's teardown proof passed because mode 1 never runs `gcp-db-init` at all, so
   no destroy had ever met this.

   The obvious remedy — hand ownership back before destroying — is not merely awkward, it is
   **impossible**: the instance has no public address, so only a Pod in the cluster can run
   SQL against it, and Terraform destroys the cluster *before* it reaches Cloud SQL. By the
   time the delete is attempted there is nothing left that could have prepared for it. The
   fix is therefore `deletion_policy = "ABANDON"` on `google_sql_database.map`, which drops
   the resource from state without calling the API and lets the instance destroyed in the
   same run take the database with it — the lifetime that was always true. This run's own
   teardown was completed by removing the resource from state by hand and re-running the
   destroy, which is exactly the manual step the fix removes.

1. *The dbinit record omitted `REPLICATION`.* Slice 5a added the assertion
   (`dbinit.sql`) and a row for it in the deploy guide's asserted-properties table, but not
   the corresponding field in the summary `RAISE NOTICE` — so the property was enforced and
   the record that evidences it was silent. Found by reading the live output against the
   table. The NOTICE now carries `replication=%`, and `dbinit_test.py` asserts it (80
   checks, up from 79).
2. *The guide's collector paragraph was under-specified, in two sequential ways.* The
   platform builds `otlptracegrpc`, `otlpmetricgrpc` **and** `otlploggrpc` against the one
   `OTEL_EXPORTER_OTLP_ENDPOINT`, so a collector declaring only a `traces` pipeline made
   every process print `Unimplemented … LogsService`; declaring `logs` and `metrics`
   pipelines took that to zero on all three. Fixing it then exposed the second trap: the
   `googlecloud` exporter drops the logs signal with `no log name provided` until
   `log.default_log_name` is set. Both are now in the guide, together with the honest
   limit — after the fix the platform emitted no further log records at all (the collector
   saw zero logs-signal batches across two ten-minute windows), so **no platform log entry
   was observed arriving in Cloud Logging**; the fix is proven only to the extent that
   nothing is dropped any more. On GKE the containers' stdout reaches Cloud Logging through
   the node's own agent regardless, which is why the gap is easy to miss.
3. *The guide claimed mode 2 had never been accepted.* True when written; this run is what
   changes it.

**Teardown took three runs, and only the first failure was a defect.** Run 1 removed 21
resources and stopped on finding 0 above. Run 2, after the database was taken out of state
by hand, removed the rest of the billable infrastructure — the Cloud SQL instance included —
and then failed on the VPC peering with `Producer services (e.g. CloudSQL, Cloud Memstore,
etc.) are still using this connection`, because Cloud SQL releases the peering
asynchronously and the delete landed while the release was in flight. Run 3, a few minutes
later, completed. That second failure is GCP's timing rather than a fault in the
configuration, but it is reliable enough on a private-IP instance that the guide now tells
operators to expect two runs. `foundation/` is untouched by design.

**A hazard that mode 2 structurally does not have.** The mode-1 record's
orphaned-PersistentVolume trap **cannot fire here**: mode 2 runs all three bundled services
off — `existingSecret` does not disable them, it makes leaving them on a render error — so
the release renders no StatefulSet and claims no volume; the run
finished with zero PVCs and zero PVs, and the only disks in the project were the four node
boot disks the destroy reclaims. The OTel collector's Google service account and its two
project role bindings were created by hand for this run and removed after it.

**Two observations that are not defects.** Unlike mode 1, where the three platform pods
restart two or three times on a first install while the bundled Postgres comes up, all
three reached `Running` on the **first** attempt with zero restarts — Cloud SQL was already
serving, so there was no DSN host to wait for. And #64 is unchanged and visible: the five
sessions this battery created left five sandbox pods `Running` at teardown, one apiece,
each still holding its CPU request.

## Outcomes plan (21) — archived 2026-08-03, all five slices delivered (#77, absorbing #161)

docs/plan/21_outcomes.md is archived complete: the reference's define-outcomes surface — `user.define_outcome` (text and file rubrics, `initial_events` on session create), the platform-provisioned grader loop with revision feedback up to `max_iterations`, the `span.outcome_evaluation_start/_ongoing/_end` trio, the `outcome_evaluations[]` projection, and the `/mnt/session/outputs/` harvest into the Files API — runs end to end and is verified against the reference doc's own example. Slice 1: SDK bump v1.59.0→v1.61.0 with the wire-schema verification record below (PR #255). Slice 2: define_outcome acceptance + storage + rendering + `initial_events`, with interrupt settlement pulled forward so mid-outcome chaining works (PR #257). Slice 3: the grader loop, transcript-stage (PR #258). Slice 4: the `outputs_harvest` work kind, the per-path deliverables snapshot, and the grader's deliverables input (PR #260). Slice 5: the full-chain acceptance below plus docs settlement (the archiving PR). Two platform fixes were forced by the live acceptance and landed in the slice-5 PR: per-route `max_tokens` on model-provider config, and the outputs-path line in the outcome charge. Deliberate divergences and inferences are in docs/DIVERGENCES.md (the plan-21 entries all *Tracked: #77/#78*); the as-built system in docs/ARCHITECTURE.md.

## Outcomes acceptance — the doc's define-outcomes example, three clients, real model (run 2026-08-03) — ✅ passed

The reference doc's DCF example (<https://platform.claude.com/docs/en/managed-agents/define-outcomes>) driven against the full compose stack (controlplane + brain + executor + Postgres + MinIO + OpenBao) with a real model behind the Anthropic-protocol route (MiniMax-M3 via the `.env` gateway), by three independent clients:

- **Go SDK leg (anthropic-sdk-go v1.61.0, typed end to end).** `TestLiveDefineOutcomesAcceptance` runs the doc flow twice — file rubric and text rubric — with `assertNoExtras` on the file, session, and outcome-evaluation resources. File-rubric: session `sesn_fwd8vp7a635s8j7j78x3scj5` reached `satisfied` with one deliverable (`Costco_DCF_Model.xlsx`, 61,202 bytes, correct xlsx mime) in 609s. Text-rubric: `sesn_antqq6tn9ev1nddccn7ncqak` reached `satisfied` with two deliverables (`Costco_DCF_Model.xlsx` + `build_dcf.py`) in 576s. Every deliverable downloaded byte-count-identical to its `size_bytes`. The harness fails any `satisfied` outcome with zero harvested files — the assertion the discovery runs earned (below).
- **Real `ant` CLI leg (v1.21.0, built from the local checkout, over `--base-url`).** The doc page's Note variant: `beta:sessions create --initial-event` carrying the `user.define_outcome` with a **file rubric** (`beta:files upload` first) created session `sesn_4vr1x915wnpb0tz29e7fhsgk` directly in `running`; a mid-outcome `beta:sessions:events send user.message` was accepted; the poll reached `satisfied` at **iteration 1** — the revision loop live: the grader's iteration-0 feedback drove a rebuild, and the iteration-1 `span.outcome_evaluation_end` references its start event, carries usage, and settles to `status_idle end_turn`. The deliverable listed via `beta:files list --scope-id` and downloaded via `beta:files download` — `file(1)` reads it as `Microsoft Excel 2007+`, size matching `size_bytes`.
- **Raw-wire curl leg (the doc's four curl blocks, byte-faithful).** Multipart rubric upload, session create + define_outcome, the doc's own `jq -r '.outcome_evaluations[] | "\(.outcome_id): \(.result)"'` poll (observed `pending` → `running` → `satisfied` at iteration 0 — the no-revision path), and the files list + `/v1/files/{id}/content` download, on session `sesn_2zwc3nejcgz3dn4f1580xbbq` with three deliverables (`README.md`, `build_dcf.py`, `Costco_DCF_Model.xlsx`). Headers as printed in the doc (`x-api-key`, `anthropic-version`, `anthropic-beta: managed-agents-2026-04-01`) — accepted and ignored per the platform's beta-header contract.

Two discovery runs preceded the recorded one, each converting a live failure into an in-PR fix (the vaults-acceptance precedent). Run 1: the model's whole-workbook `write` tool call was truncated at the anthropic adapter's 8192 `max_tokens` fallback mid-JSON (`model_request_failed_error`, retries exhausted) → the per-route `max_tokens` config knob, with the compose route set to 32768. Run 2 (rounds 3): both rubric variants graded `satisfied` with **zero** harvested deliverables — the agent wrote to `/workspace` because nothing named the outputs directory → the outcome charge now ends by naming the deliverables contract, and the harness hard-fails a hollow pass. The grader's explanations in the recorded run cite the deliverables sheet-by-sheet against the rubric criteria.

Client-side accommodations, none touching the wire: the CLI's `--model` flag is typed `map[string]any` (its usage text notwithstanding), so agent create takes the `model_config` object form `{id: …}` — the server accepts both forms per the SDK schema; the CLI transcript's final download was re-issued with the file ID passed literally after a `--transform` guess extracted the whole object; and the `ongoing` heartbeat was not observed live — grading cycles completed inside the 30s cadence (the rehearsal pins the no-heartbeat path; the firing path itself has only unit coverage of the event shape, no end-to-end exercise yet).

**Review hardening, landed in the same PR.** The verifier returned PASS WITH FINDINGS — three docs-accuracy defects (an over-attributed mid-outcome `user.message`, an overclaimed `assertNoExtras` scope, an inverted heartbeat parenthetical), all fixed. The Codex pass found the one substantive defect: the live leg consented via `RUN_LIVE_MODEL_TESTS`, so the documented cents-level provider smoke would have silently bought whole agent sessions — the exact failure modeltest's tier contract names; the leg now carries its own `RUN_LIVE_ACCEPTANCE_TESTS` tier. The Claude review (Opus 5) put nothing over its confidence bar but surfaced two real nits fixed here: no `-timeout` guidance for a leg whose two 25-minute variants exceed go test's 10-minute default (the doc comment now spells the invocation), and a stale `TestRehearsalDCF*` glob in the package comment. CodeRabbit added three keepers — the SSE watcher now closes its stream, the Helm references state `max_tokens` must be positive or the brain fails at startup, and ARCHITECTURE's `provider.go` row names `MaxTokens` — and its remaining threads were refuted with evidence: the live leg cannot sweep containers on a stack it reaches only over HTTP, a `Close` on the sandbox provider for a process-lifetime transport is speculative surface, and the "future-dated" records carry the runs' actual 2026-08-03 timestamps.

## Blob absence proof (#244) — rejected alternatives, 2026-08-02

The narrative is in CHANGELOG.md. What it cannot hold is why the fix does not look like the
remedy the issue itself proposed.

**Evaluated and rejected: a ranged GET.** The issue's own leading option was to learn absence
from `Range: bytes=0-0` rather than a HEAD, so an error response *could* carry the document,
and it priced the option honestly: empty objects, the 200-vs-206-vs-416 variation across
endpoints, an extra request on the hot path, and reader/size handling all to be worked out.
Every one of those costs is an artifact of the range, not of the requirement. `minio.Core` —
minio-go's low-level API, plain S3 on the wire — exposes the same `GetObject` **eagerly**,
returning `(io.ReadCloser, ObjectInfo, http.Header, error)` from one unranged GET. No `Range`
header means no 206/416 question and no empty-object special case; the reader and the size
are what the call already returns; and because `Get` was going to issue that GET on the
caller's first read anyway, the request replaces the `Stat`'s HEAD instead of joining it. The
ranged GET buys nothing the eager GET does not, and costs a round trip.

**Evaluated and rejected: no fix for `Delete`'s header override.** The issue left that residue
open on the ground that the actor required — a server contradicting its own error document,
or an intermediary injecting a MinIO-dialect header — could equally forge a well-formed
`NoSuchKey` body, which is out of scope for any remedy. True, and not a reason to leave it:
forging a document takes an adversary, while contradicting one takes only a buggy proxy, and
the transport wrapper that closes it is small enough that the asymmetry it removes is worth
more than the lines. The forged-document case remains genuinely unclosable and is documented
rather than defended against.

**Not done: `Get`'s `blob.ErrNotFound` kept for bare 404s.** Preserving today's behavior for
reads while hardening only `Delete` was the conservative option, and it was rejected because
the two failure modes differ only in severity, not in kind — a read told "no such object"
when it was refused is a wrong answer given confidently. No caller loses a client 404 by the
change: both API download paths already mapped *every* `Get` error, `ErrNotFound` included,
to a 500 (`internal/api/files.go`, `internal/api/skills.go` — "a row whose object is gone is
an operator incident, not a client 404"), and the executor's two readers still skip the mount
or the skill and carry on. What moves is the outcome they record, from a miss to a failure,
which is the point of the change.

**Evaluated and rejected: relaxing `Get`'s proof again for AWS delete markers.** Review found
one absence that arrives with no document to demand. AWS answers a GET for a key whose current
version is a delete marker with a 404 carrying `x-amz-delete-marker: true`, `Content-Type:
text/plain` and no body at all — its GetObject reference's "If the Latest Object Is a Delete
Marker" sample response, which stands in deliberate contrast to the `<Error>` document the
sample immediately above it carries. On a versioned bucket that is the ordinary state of every
deleted key, so demanding the document would have broken `ErrNotFound` for the whole store
there. Dropping the document requirement for reads would have undone the fix; reading the
header inside the absence check was impossible, since `minio.ErrorResponse` carries no headers.
So the transport translates: on that one answer it writes in the error document the header
stands for. The translation cannot launder an ordinary bare 404 into absence, because a
misrouting proxy's 404 does not carry an affirmative AWS delete-marker header — which is the
whole point of asking for an affirmative signal rather than accepting a missing one.

---

## Sandbox hardening (plan 19) — archived 2026-08-01, delivered in one PR (#65)

docs/plan/19_sandbox-hardening.md is archived complete: every sandbox is now created with cgroup limits and capability drops by default, with a memory cap, a read-only root filesystem, a non-root uid and a Kubernetes `runtimeClassName` available on request. The narrative is in CHANGELOG.md; what follows is only what a changelog cannot hold — the alternatives that were evaluated and rejected, and the measurements that decided them.

- **Failing `Provision` on Kubernetes when a pids limit is configured** — the fail-closed shape this codebase normally prefers, and the honest one: a security control the backend cannot enforce would never be silently ignored. Rejected because the platform default is *on* (512), so it would have broken every Kubernetes deployment out of the box for a control the cluster expresses elsewhere entirely — the kubelet's `podPidsLimit`. What landed instead makes the asymmetry structural rather than hidden: the shared contract's pids row is registered only for a backend declaring `Harness.EnforcesPidsLimit`, `TestPodSpecIgnoresPidsLimit` pins the Kubernetes side as deliberate, and docs/DIVERGENCES.md carries the entry. Measured while deciding: a pod under a 500m CPU limit on kind reads `/sys/fs/cgroup/pids.max` as the node default (9517), and `k8s.io/api` v0.36.2 `core/v1` defines no pids `ResourceName` at all.
- **Putting the hardening on the provider config rather than on `sandbox.Spec`** — less code, and arguably the more honest home for what is deployment configuration. Rejected because the acceptance asks the *shared contract suite* to cover the controls, and a per-`Spec` knob lets one row provision a differently hardened sandbox from the same provider; a provider-config knob would have needed a second harness hook per dimension.
- **A tmpfs for the read-only-rootfs writable mounts on Docker** — the obvious choice, and wrong. The contract row caught it before review did: the daemon refuses `PUT /containers/{id}/archive` on a read-only-rootfs container with `http 400: container rootfs is marked read-only` when the destination is a **tmpfs**, and allows the same PUT when the destination resolves into a **volume** (both measured directly against the daemon, with `docker cp` into a container carrying one of each). Every file that backend writes goes through that endpoint, so a tmpfs workdir would have shipped a sandbox that runs commands but can never receive a file — no skill materialization, no file materialization, no `write` tool. Anonymous volumes need no lifecycle of their own: `removeContainer` already deletes with `v=1`, verified to take them away with the container (28 → 30 → 28 volumes across a create/delete cycle). The residue is recorded rather than papered over — a fresh anonymous volume takes its ownership from the image's directory when the image ships one and is otherwise root-owned `0755`, so on Docker a read-only root makes the workdir *exist* for a non-root uid but does not make it *writable*; on Kubernetes the kubelet's world-writable `emptyDir` does both (`drwxrwxrwx`, verified with `runAsUser: 65534`).
- **Letting a CPU limit become the Kubernetes request** — the default when no request is given, and it would have turned containment into a capacity decision nobody made: a 2-CPU limit reserves 2 CPUs per sandbox pod and can make it unschedulable on a small node, a CI kind cluster especially. The provider pins the request to `min(limit, 100m)` instead. Memory keeps request = limit, which is correct for a resource that cannot be throttled, and is opt-in anyway.
- **Defaulting the memory cap on** — rejected: an OOM kill lands in the middle of a task, which is a worse failure than the throttling a CPU quota causes, and the issue's acceptance names pids and CPU only. The knob exists and is off.

**Review hardening, landed in the same PR.** The first cut had three defects that
would have shipped a hardening flag nobody could safely turn on; each is now
pinned by a mutation-checked test (the fix reverted, the test observed to fail).

- **A read-only root filesystem broke the `bash` tool outright** (Claude review,
  measured against the daemon). The persistent shell keeps every session's cwd
  and environment under `/var/lib/map-shell`, which is on the container's root —
  so the *first* bash call of every session hit `EROFS`, and hit it as a
  **backend fault**: the tool was left unanswered and the work item reclaimed,
  rather than the model seeing an error. The sandbox contract row could not see
  it (it exercises `Exec` and `WriteFile`, never `shell.Run`) and the shell
  suite provisioned unhardened sandboxes. The same class hid a second path:
  session file resources land under `/mnt/session/uploads/<file_id>` (Codex),
  where materialization would have logged a failure and continued without the
  file. Both paths are now in one shared list, `sandbox.WritablePaths`, asserted
  by the contract row and by a new shell row that provisions a read-only-rootfs
  sandbox; reverted, it reproduces `mkdir: cannot create directory
  '/var/lib/map-shell': Read-only file system` exactly.
- **`SANDBOX_RUN_AS_USER` could silently disable the egress gate** (Claude
  review). The gate's owner-match firewall ACCEPTs uid 65532, so setting the
  sandbox to run as it — distroless's `nonroot`, the most copy-pasted non-root
  uid, and the one the security doc already flagged as dangerous for an *image*
  — would have let every tool process leave the namespace unfiltered with
  `allowed_hosts` void and nothing logged. #196 reached through a knob this
  change itself introduced. `Hardening.Validate` now refuses it on a gated
  session and both providers call it before contacting anything; 65532 moved to
  `gaterun.DefaultGateUID` so cmd/gate and the providers cannot disagree.
- **The on-by-default CPU cap would have failed every provision on a small
  host** (Claude review, measured). The Docker daemon rejects a container asking
  for more CPUs than the machine has, so two CPUs on a one-vCPU host meant a 400
  per session rather than an error at startup. The provider now clamps to the
  daemon's own count, read once and logged when it clamps; a daemon that will
  not answer leaves the value alone, so a failed probe can never widen a cap.
- **A large `SANDBOX_CPU_MILLIS` wrapped to "no limit"** (Codex): millicores ×
  10⁶ overflows int64 above ~9.2×10¹² and lands on exactly zero, which
  `omitempty` then drops. Refused at parse time instead. Capability parsing was
  laxer than it claimed for the same reason — `0`, `_`, `123` and
  `CAP_CAP_NET_RAW` all passed a character-class check and would have failed
  every container create instead of this deployment's startup — and `none` was
  matched case-sensitively while every other value is upper-cased, so `NONE`
  became a capability literally named NONE.
- **The wiring nobody tested.** `Hardening: cfg.Hardening` in the executor and
  the worker is the one line that makes "on by default" true for every real
  deployment, and deleting either left the whole suite green (Claude review):
  the contract rows pass a Hardening straight to `Provision` and never travel
  through those packages. Both now have a row.
- **The Helm template silently dropped an explicit zero** (verifier). `{{- if not (empty $value) }}` is true of Helm's int `0` and of `false`, so an operator writing an unquoted `cpuMillis: 0` to turn a cap **off** had that ignored and got the executor's 2000-millicore default — the exact "silently changed configuration" failure the rest of the change refuses. Measured both ways (`--set cpuMillis=0` rendered nothing; `--set-string cpuMillis=0` rendered `value: "0"`). The test is now `not (or (kindIs "invalid" $value) (eq (toString $value) ""))`, and CI asserts the `0`/`false` passthrough.
- **The plan promised a `STATE.md` update the PR deliberately does not make** (verifier). The non-update is correct — this plan is archived in the same PR, so at merge nothing here is in flight and STATE.md's incumbent active work stays the tracked item — but the plan's "What lands" list said otherwise. Corrected there, with the reason.
- **The `bash` description was the one sandbox-tool string that still matched the SDK's verbatim**, and now deliberately does not (verifier, recorded for completeness). Registered in docs/DIVERGENCES.md rather than left as an unregistered mismatch, since a divergence outside the registry is a finding.
- **Documentation claims that were not true**, each corrected: `RunAsUser`'s own doc comment and the contract row's said a read-only root's writable mount makes the workdir writable by a non-root uid, which holds only on Kubernetes (the plan and the security doc already said so — the code contradicted them); the divergence registry claimed a shared *memory* contract row that does not exist; the security doc claimed a strict Pod Security `restricted` namespace would accept the pod once `SANDBOX_RUN_AS_USER` was set, when `restricted` wants `runAsNonRoot`, a `seccompProfile` and `drop: [ALL]`, none of which that knob supplies; and the chart's RuntimeClass comment claimed the created object outlives the release, when Helm removes it on uninstall (Codex) — left Helm-managed deliberately, since a chart that strands cluster-scoped objects is the worse failure.
- **The clamp's own cache could reintroduce what the clamp prevents** (verifier, second pass). `clampCPU` read the daemon's CPU count under a `sync.Once`, which cached a *failed* probe as readily as a successful one: one transient `/info` error on a host smaller than the cap left every later provision unclamped and taking the daemon's rejection per session until the executor restarted. Only a success is cached now — a failed probe is retried on the next provision, at the cost of a redundant `/info` when racing provisions both miss. Mutation-checked: restoring the cached failure turns `TestClampCPURetriesAFailedProbe` red at one `/info` call instead of two.
- **Three defects the bot reviewers found on the PR**, all in the configuration surface rather than the containment itself, each mutation-checked. `SANDBOX_CAP_DROP` validated the *shape* of a capability name, so `NET_RAWW` started cleanly and then failed every container create — the opposite of the fail-fast contract the rest of the knobs keep; it is checked against the kernel's own capability set now, with `ALL` still the wildcard. `WritablePaths` deduplicated exact strings, so a workdir written `/tmp/` slipped past and produced the duplicate mount target both runtimes reject; the workdir is cleaned first. And `volumeName` folded a path into a readable stem with nothing bounding it: a workdir over ~57 characters exceeded the DNS-1123 label limit, and two paths differing only in a character it erases collided — `/VAR/LIB/MAP_SHELL` against the platform's own `/var/lib/map-shell`, found by the test rather than by inspection. A hash of the exact path is appended and the stem truncated to fit.
- **Two more from the PR round, both in the same configuration surface.** `SANDBOX_RUN_AS_USER` was bounded only below, so a uid past 32 bits truncated in the runtime's setuid and could land on 0 — an operator asking for an unprivileged sandbox getting a root one, the hazard `gaterun.CheckGateID` already guards for the gate's own uid; it is bounded by a positive int32 now. And `Hardening.Validate` compared `RunAsUser` against `gaterun.DefaultGateUID` while the gate container's uid was whatever its image's `GATE_UID` said, so a custom gate image could move the firewall's accepted identity out from under the check. Rather than plumb a configurable uid through `Validate`, both providers now *state* `GATE_UID` in the gate container's environment, where it beats the image's — the constant is true by construction.
- **Refuted, with evidence:** that an adopted sandbox should be replaced when the configured containment changes. The observation is correct — `Provision` adopts a session's existing container or pod, so a session already running when the executor rolls keeps the containment it was created with — but the remedy costs more than the gap: replacing a live sandbox discards the workdir, the session's materialized file resources and the persistent shell's state mid-task, to harden a session that is already running and already bounded by its own end. Containment binds at create exactly as `Env` and the networking mode do, which `sandbox.Spec` documents; the rollout consequence is now stated for operators in docs/self-hosted-security.md rather than left to be discovered.
- **Refuted, with evidence:** that `EffectiveCapDrop` could be made to drop the gate's three by configuration. Every route was checked — `"ALL"` (which absorbs them), duplicates, ordering, and an empty configured set — and the union is applied after the configured value in one function both backends call. Both reviewers audited it independently and neither found a path; the remaining hazard is the *uid*, not the capability set, which is the finding above.

## Spill oversized tool output (plan 18) — archived 2026-08-01, delivered in one PR (#226)

A single-PR plan, the plan-16 precedent. The issue's open design fork — the web tools run in the
executor with no sandbox provisioned, so where does a web answer spill? — resolved to **web
answers do not spill**: provisioning a sandbox solely to store a spilled answer would couple the
web pass back into sandbox lifecycle (plan 15's "no sandbox and no gate" was deliberate), and web
content, unlike a command's output, is re-fetchable — the model can fetch again, narrower. The
five eligible sandbox tools spill via the `Runner`'s own sandbox handle. Also evaluated and rejected: spilling
raw (pre-sanitize) bytes — the file holds the NUL-stripped text the trailer describes; spilling
in bash's success arm as well as its failure arms — the success arm reaches dispatch uncapped
and spilling twice would write the same file twice per call. The issue's other unknown — the
reference's preview shape and path convention — stayed unobservable (no recording capability
against the real backend); both landed as ours, INFERRED, in the rewritten DIVERGENCES entry.
One honest bound recorded there: "whole" is everything `Sandbox.Exec` retained and returned to
the toolset, bounded by its pre-existing 1 MiB per-stream memory guard. The review round then
forced one behavior change and killed one wrong claim, both reviewers converging on each: the
first cut spilled `read` output too, and because every spill file exceeds the cap by
construction, reading one back minted another full copy under a fresh id — a chain with no fixed
point, measured at five distinct 106 KB copies in five follow-the-notice reads — so `read` is now
exempt (decision 8; its full content already sits at the path the model named); and the original
rune-boundary fixture (2-byte runes against an even cap) could never split a rune, so both
preview paths are now pinned with 3-byte fixtures the cap genuinely cuts mid-rune, plus the
timeout arm's spill and the write-attempt-on-failure, all mutation-checked. The PR round added
one more: a successful grep that hit the sandbox's own 1 MiB Exec retention arrived with
`Truncated` set but dropped the flag, so a spill of it claimed "full output" for a result the
sandbox had already cut — grep's success arm now carries the upstream `[output truncated]`
marker at its head, mutation-checked (the test observed red against the unfixed arm).

## Model endpoint stall bound (plan 17) — archived 2026-08-01, delivered in one PR (#121)

docs/plan/17_model-endpoint-stall-bound.md is archived complete: a model turn is now bounded by the endpoint's silence (`provider.StallGuard`), enforced on the request context and fed by byte-level progress, with a per-route `stall_timeout` defaulting to 10 minutes. The narrative is in CHANGELOG.md; what follows is only the designs that were evaluated and rejected, since the issue proposed two of them and named a third as wrong.

- **A shared, package-level `http.Client` with a `ResponseHeaderTimeout`** — the issue's first suggestion, and the shape that most directly restores what `option.WithoutEnvironmentDefaults()` costs. Rejected on two counts, both verified in the pinned SDK's source. `requestconfig.shouldRetry` returns true whenever `res == nil`, so a transport-level timeout is a *retryable* error: a wedged endpoint would buy `MaxRetries+1` budgets (three, by default) rather than one, and the wait an operator configured would not be the wait they got. And a header timeout bounds only the wait for headers — the issue says so itself — leaving a gateway that sends headers and then dies mid-SSE exactly as unbounded as before. Cancelling the context the SDK was handed avoids both: `ExecuteNewRequest` checks `ctx.Err()` immediately after the handler, ahead of the retry decision, and the same cancellation aborts a body read at any point in the stream.
- **A per-turn deadline at the brain's call site** — the issue's second suggestion. Rejected because a deadline bounds *duration*, and duration is not the defect: a model streaming a 64k-token answer legitimately holds one request open for tens of minutes, so any deadline safe for that turn is far too loose to be a bound on a wedged endpoint. It would also apply the same number to every route, and to the brain's own database writes inside `streamTurn`, where a fired deadline would be misclassified as an `infraError` and abandon the turn to lease expiry instead of reporting it.
- **A stall watchdog on `stream.Next()` in the brain** — the same idea rebuilt on silence rather than duration, and provider-agnostic, which was its appeal. Rejected on granularity: the adapters return from `Next` only on *content* chunks, so a tool call streaming a large input holds one `Next` open for the whole block (`input_json_delta` frames accumulate without returning), and a long tool input would read as a stall. The guard has to sit where the bytes are.
- **`option.WithHTTPClient(&http.Client{...})` per provider instance** — named in the issue as the naive fix and confirmed as one: `provider.Registry` builds a provider per turn, which is affordable only because every instance shares `http.DefaultClient`, so a per-instance client would hand each turn its own idle-connection pool and TLS session cache (#88). Nothing in the delivered change touches the client at all.
- **Feeding the guard from protocol frames instead of bytes** — considered and dropped once the SDK was read: `ssestream.Stream.Next` handles Anthropic's `ping` with `case "ping": continue`, so the frames that exist precisely to prove a quiet endpoint is alive never surface to an adapter, and an SSE comment is not an event in the first place. Both adapters wrap the response body instead — the OpenAI one directly, the Anthropic one through `option.WithMiddleware`, the only hook the SDK offers onto a body it owns.
- **A fixed process-wide budget instead of a route key** — rejected because the worst legitimate silence is a property of the endpoint, not of the platform: a hosted gateway answers in seconds while a queued self-hosted model may take minutes to send its first byte, and only the route knows which it is. The default stays conservative (10 minutes, the SDK's own `defaultResponseHeaderTimeout`) so an operator who configures nothing never loses a healthy turn.

**Review hardening, landed in the same PR.** Both reviewers found real defects in the first cut, each now pinned by a mutation-checked regression test (the fix reverted, the test observed to fail):

- **The bound had a hole exactly where the issue lives.** A completed OpenAI turn drains its tail on `Close` so the connection can be pooled — and the drain read through the progress wrapper, so an endpoint that keeps writing past its own `[DONE]` fed the guard with the very bytes that were holding the drain. `Close` never returned, the brain's settle-time defer never returned, and the replica was wedged: #121 preserved, one path over. Fixed with a byte limit (`drainTailLimit`), a bound that does not depend on the reader being idle. Reverting it hangs the test binary rather than failing it, which is the shape of the defect.
- **A stalled error body could persist a credential prefix.** The OpenAI non-200 path quoted whatever `io.ReadAll` returned and discarded the read error. Before this change a stall there simply hung; now the guard cancels the read, so the truncated bytes reach the error — and redaction matches whole secrets, so a key cut in half survives it, which this package's own truncation test defines as a leak. A read failure now drops the body and reports the status alone. Reverted, the test finds a nine-character prefix of the key in the `session.error`.
- **Response headers were not progress**, so the wait for headers and the wait for the first byte shared one budget: an endpoint spending 0.6 of it on each was cancelled although neither silence lasted a whole budget. `ProgressBody` now records a sign of life when it wraps, which also re-arms on each of the SDK's retry attempts.
- **The OpenAI adapter's error path kept that hole after the fix closed it on the success path** (found on the PR, post-review). The wrap happened after the status check, so a non-200 read neither counted the response's arrival as progress nor let the error body's bytes re-arm the guard: an upstream that was slow but never silent — a long diagnostic streamed in chunks — had its read cancelled, and the operator got `context canceled` instead of the explanation. The wrap moved ahead of the status check, which is also what the anthropic adapter has always done (its `option.WithMiddleware` wraps every response body, error responses included); only the hand-rolled path could tell a 200 from a 500 and treat them differently. Reverted, the new test reports exactly the predicted `502 Bad Gateway, and its error body could not be read: context canceled`. The credential-truncation guarantee is unaffected — a genuinely silent endpoint still trips and its partial body is still dropped.
- **Documentation overclaimed the settlement.** Four places said a stall completes the work item with `retries_exhausted`; a stall with pending mid-turn input chains a fresh turn with `retrying` and requeues, like any other failed model request. Corrected in CHANGELOG, DIVERGENCES, ARCHITECTURE and the plan.
- **Two comments asserted things that were not true** — that a brain blocked on the database for a budget has necessarily lost its lease (`queue.KeepLease` cancels only on a *failed* renewal), and, by omission, that the guard bounds a peer that dribbles one byte per budget (it cannot, by construction). Both replaced by what is actually true, including the one case the guard knowingly mislabels: the SDK sleeps out an uncapped upstream `Retry-After` under the same context, so a long backoff is cut short and reported as a stall.
- **Refuted, with evidence:** that sampling `time.Since(start)` before `last.Load()` is a defect. Descheduled between the two, the guard trips *late* by that interval; the opposite order would trip *early* on progress that had just arrived. Late is the safe direction — the ordering is deliberate and now says so.

## One answer per tool call (plan 16) — archived 2026-08-01, delivered in one PR (#222)

A single-PR plan: the same PR landed the file (archived at birth) and the fix. The triage-opened
repair fork — a commit-time answered-set re-diff in the executor's web settlement vs. an API-layer
rejection scoped to web tool names — resolved to the **API-layer rejection**, one new arm in the
already-existing `ValidateToolResults` (a platform-ownership predicate the API fills with
`toolset.IsWebTool`). The re-diff was rejected for two reasons recorded in the plan: with the arm
in place it guards an unreachable state (clients rejected at the door; interrupts and rival
executors already fail the settlement's `queue.Complete` lease proof; `tool_exec` is cloud-claimed
only; the permission gate never overlaps a live run), and its semantics are wrong for a
platform-owned call — it silently drops one answer instead of 400-ing the poster, letting a
client-fabricated result stand for a call the platform was asked to run. Also evaluated and
rejected: importing `toolset` into `internal/events` for the name check (drags the sandbox
dependency into the log package — the predicate is injected instead). Reachability analysis that
narrowed the issue's "both paths affected equally" to self_hosted `web_exec` only is recorded on
the issue and in the plan. Public docs (self-hosted-sandboxes / tools / events-and-streaming,
2026-08-01) were checked for reference behavior before recording the rejection as INFERRED.

## Web tools plan (15) — archived 2026-08-01, all four slices delivered (#47)

docs/plan/15_web-tools.md is archived complete: the last two `agent_toolset_20260401` tools execute in the platform executor's process on both deployment modes, behind config-driven Tavily/Jina backends. Slice 1: the `internal/webtool` seam — Searcher/Fetcher interfaces, tavily/jina adapters, shared contract suite, `RUN_LIVE_WEB_TESTS` live tier (PR #221). Slices 2+3, one PR (#224): domain `SearchResultBlock` + `Result.SearchResults` + eight-tool definitions + openai `search_result` flattening; the `web_exec` work kind (migration 0015), the web-first hold-back (brain settlement + confirmation resume), the executor web driver (no sandbox, both env kinds), worker/sandbox-pass filters, env wiring (compose passthrough + helm `executor.extraEnv`), and the acceptance run below. Review hardening landed in-PR: fail-closed fetch construction, the metadata-charged output budget, NUL sanitization, the http/https scheme check at the executor seam, the stray-web-call heal, claim-order alternation. Follow-ups split out rather than absorbed: #222 (double-answer race, pre-existing), #223 (sandbox NUL output, pre-existing), #225 (allowed-domains allowlist), #226 (spill-to-file). Slice 4: the remaining DIVERGENCES registrations (#225/#226), the README status line, the plan archive. Deliberate divergences and inferences are in docs/DIVERGENCES.md; the as-built system in docs/ARCHITECTURE.md.

## Web-tools slices 2+3 acceptance — real stack, real Tavily/Jina, real `ant beta:worker` (run 2026-08-01) — ✅ passed

The full compose stack (controlplane + brain + executor + Postgres + MinIO + OpenBao), the executor carrying real `TAVILY_API_KEY`/`JINA_API_KEY`, every management call driven by the real `ant` CLI (v1.21.0, built from the local checkout) over `--base-url`. Two runs, both against the plan's acceptance:

- **Cloud, search-then-fetch.** A `cloud` unrestricted session asked to find and read the Go docs site produced exactly the plan's log shape: `agent.tool_use web_search {"query":"Go programming language official documentation"}` → `agent.tool_result` with **five `search_result` blocks** (title / source URL / text content / `citations:{enabled:false}`, verified field-for-field on the wire), then `agent.tool_use web_fetch {"url":"https://go.dev/doc"}` → `agent.tool_result` with **one `text` block** carrying the reader's markdown, then a correct final `agent.message` and `session.status_idle`. `docker ps` showed **no sandbox container** for the session — the web driver ran both calls in the executor process.
- **Self_hosted, mixed turn.** A session on a real-`ant`-worker-polled environment was asked for one turn calling `web_search` and `bash` together. The work-item timeline proves the web-first hold-back: the settlement enqueued **only** `web_exec` (37.011s); the `tool_exec` was created **in the same commit** as the web `agent.tool_result` (45.644s); the worker claimed it after (46.380s) and answered bash with a `user.tool_result` (the proof file landed on the worker host). The turn resumed only once both results were in, and completed. One measured surprise, folded into the DIVERGENCES entry: the v1.21.0 worker's scan *did* list the already-answered web call (its answered-diff does not count a platform `agent.tool_result`) and logged "tool not owned by this runner; leaving the tool_use_id pending for its owner" — this client tolerates a non-owned call rather than erroring, so the ownership check, not the hold-back, is what spared it; the hold-back stays for determinism and for clients without that tolerance. The plan's "without the worker ever seeing a web call" criterion reads as the *unanswered*-call invariant — no unanswered web `agent.tool_use` visible to a polling worker — and that held: the worker's item did not exist until the web result was committed.

One endpoint accommodation, recorded because it exercised a second code path rather than dodging one: the `.env` gateway's **Anthropic-protocol** endpoint (MiniMax) rejects `search_result` blocks inside replayed `tool_result` content (`invalid tool_result content (2013)`) — a gap in that endpoint's Messages implementation, not in the wire shape (the pinned SDK's tool-result content union includes `search_result`). The acceptance re-ran against the same gateway's **OpenAI-compatible** endpoint, which put the openai provider's new documented lossy conversion — `search_result` flattened to text on replay — on the live path for every post-search turn. A fully-conformant Anthropic endpoint needs no flattening.

## A bulk sandbox write (plan 14) — archived 2026-07-26, delivered in one PR

The measurements the design rests on, taken on this branch against a real Docker daemon and a
local kind cluster with `debian:stable-slim`, since they are what a reviewer would otherwise have
to re-derive:

**The cost being removed.** A buffered `WriteFile` on the docker backend: 20 calls in 409ms
(20.4ms each), of which a bare `Exec` is 13.8ms — so the rename step #71 added is ~68% of a small
write. At `skills.MaxMembers` (10,000) that is ~138s of exec overhead for one skill.

**The cost after.** 200 files across 8 directories, same sandbox: docker 4.149s one at a time →
255ms in one batch (16.3×); k8s 3.627s → 253ms (14.3×).

**Two facts that fixed the archive's shape**, measured with docker 28 and GNU tar 1.35:

| | implicit parent dirs | file entry |
|---|---|---|
| docker daemon untar (`PUT /archive`) | 0755 | 0644 |
| in-container `tar -x` under `umask 022` | 0755 | 0644 |

The two agree with each other and with what `mkdir -p` and the single write's tar header already
gave, so the archive need say nothing about directories — and must not: an **explicit** directory
entry chmods a directory that already exists (0700 → 0755 under both untars), which a write must
never do to a directory it merely passes through.

**Decisions evaluated and rejected.**

- *Batch atomicity (all-or-nothing).* Rejected: nothing asks for it, and it would be new behavior
  wearing a bug fix's clothes. The batch is a drop-in for the loop it replaces, whose first failure
  also stopped the run and left what had landed; the skills sentinel records only what landed, so
  the next pass re-runs the skill regardless.
- *Staging every member in one directory and moving from there.* Rejected: it breaks the guarantee
  the change is required to preserve. A temporary file must be in its **target's own** directory or
  the rename can cross a filesystem, and `mv` across one copies onto the destination name — exactly
  the non-atomic truncation #71 removed.
- *Carrying the manifest in the script instead of the archive.* Rejected as impossible at the sizes
  that matter: Linux caps a single `execve` argument at `MAX_ARG_STRLEN` (128 KiB) and a
  10,000-member batch's generated script is on the order of 800 KB.
- *A k8s framing that avoids `tar`* (a length-prefixed stdin protocol read with `head -c`, or bash's
  `read -N`). Rejected: `read` cannot hold NUL bytes so it is binary-unsafe, and a per-member `head`
  is one process per file — trading the exec-per-file for a fork-per-file. `tar` costs one process
  for the batch and joins an image contract that already names `/bin/bash`, `mv`, `rm`, `stat`,
  `tee` and `wc`.
- *Naming the failing member by printing its path.* Rejected in favour of a marker line carrying its
  **index**: an image's own noise shares that stream, and a number the caller can index into what it
  already holds cannot be confused with it.

**Not done, deliberately.** `internal/sandbox/shell/shell.go`'s two-or-three writes per bash call,
which #206 also counts. They bracket an exec — the command file before it, the head pointer after
— so they cannot be batched; only the restart pair could, saving one exec on restart calls alone.

## `user.interrupt` semantics (plan 13) — wire resolution and rejected alternatives, archived 2026-07-26

**The wire question the issue raised was answerable, and not from the checkouts.** The typed
schema cannot pin an interrupt's outcome — `BetaManagedAgentsUserInterruptEvent` carries only
`{id, type, processed_at, session_thread_id}`, `session.status_idle`'s `stop_reason` union is
exactly `end_turn | requires_action | retries_exhausted`, and the `ant` CLI never sends one — so
the resolution came from authority #1, the public docs: an interrupt "stops the agent
mid-execution", and "the interrupted turn ends with a `session.status_idle` event whose
`stop_reason` is `end_turn`, the same value as a turn that finishes on its own; there is no stop
reason specific to interruption." The same page pins the canonical usage as **one** send carrying
`[{user.interrupt}, {user.message}]`, and the session-operations page makes an interrupt the
prerequisite for updating, archiving or deleting a `running` session — so the interrupt must
genuinely leave the session not running. What no source pins is what happens to the calls the
ended turn left outstanding; that stayed an inference (DIVERGENCES.md).

**Evaluated and rejected: a new `stop_reason`.** An `interrupted` variant would read better on the
wire and is what the domain type's shape invites. It is wrong twice over: the pinned SDK's idle
union has no such variant, so a client decoding strictly would fail on it, and the docs state
positively that there is none.

**Evaluated and rejected: answering every abandoned call with an `agent.tool_result`.** It is what
the denial synthesis already does and would have been the smaller diff. Rejected for the reason
`internal/events/toolflow.go` already gives for confining denials to `agent.tool_use`: a result of
the wrong family is not the answer a client watching that tool is waiting for — a custom tool's
result is a `user.custom_tool_result` keyed by `custom_tool_use_id`, and a client matching on that
key would never see an `agent.tool_result` naming the same call.

**Evaluated and rejected: teaching the brain to notice an interrupt at settlement.** The
alternative to cancelling work items was to leave them alone and have the brain check, under the
session row lock, whether an interrupt landed after its turn began, then settle differently. It
costs a new code path in the brain *and* one in the executor, both of which must stay correct
against every ordering, and it still needs the interrupt to win — so the ownership proof the queue
already has (`Complete`/`Requeue`/`Assert` re-assert the lease inside the committing transaction)
does the same job with no new component logic: cancel the items under the lock, and the claimant's
whole settlement rolls back on its own.

**Evaluated and rejected: closing the interrupted turn's `span.model_request_start`.** The
reference emits the terminal `span.model_request_end` for a request that ends early; ours does not,
because the rollback that discards the turn discards that event with it. Synthesizing one from the
interrupt's transaction was considered and dropped on two grounds: it cannot be made airtight — a
turn that opens its span *after* the interrupt commits is unreachable from it, and that window is
real (a brain claims, then replays before calling the model) — and the usage such an event would
report is invented, since the interrupt knows nothing of what the model spent. Recorded as a
CONFIRMED divergence instead.

**Accepted consequence: an interrupted turn logs like a lost claim.** Cancelling a live
`model_turn` makes the brain's settlement fail its lease proof, which it reports as a red
`model_turn` span and a `turn failed` log — the signal `internal/brain/telemetry_test.go`
deliberately pins for the reclaim case. Distinguishing "cancelled by an interrupt" from "reclaimed
after expiry" needs a reason code the `work_items` state machine does not carry, which is queue
surface beyond this issue. Only the log's wording changed: it no longer claims the lease was left
to expire, which is false for an item that was cancelled outright.

## Vaults slice 1 (plan 12, PR #168) — review hardening, 2026-07-24

**The dual review converged on the transit trust boundary.** The Codex pass (`gpt-5.6-sol`,
config `ultra` effort) returned five findings; four were confirmed and fixed in the same PR,
each behavioral one with a test proven red on the vulnerable code: the transit client scrubs
the configured token from server-controlled error text (an interposed endpoint reflecting
`X-Vault-Token` could otherwise land it in a process error path), `Encrypt` refuses a response
whose ciphertext is not `vault:v<digits>:<payload>` (a broken proxy's junk would otherwise
persist and fail only at decrypt), the init scripts scope the `map-transit` policy to exactly
the configured transit key instead of transit-wide wildcards (proven live: `map-secrets`
round-trips, a foreign key 403s), and a pre-existing token under the deterministic
platform-token ID is adopted only after a policy check (proven live: a planted `default`-policy
token under the ID fails the init closed with a revoke hint). The Claude-side review workflow
(agents on Opus 4.8; zero findings across the crypto, deployment, and security dimensions)
added one confirmed doc defect: the externalOpenBao token guidance omitted `update` on
`transit/keys/<key>`, which the startup ensure-POST needs once the key exists — a token
provisioned per the old comment would 403 the controlplane/executor at boot.

**Refuted with evidence — helm subPath mounts on a fresh PVC.** Codex's remaining finding
(High: "nothing creates the `file/`/`init/` subPath directories on a new PVC, so kubelet
rejects both mounts and the pod stays in `CreateContainerConfigError`") was refuted rather
than fixed: the live-cluster run in this same branch created both subPaths on a brand-new PVC
— the first pod attempt entered the image entrypoint (failing later on `chown`, which proves
the mounts had succeeded) and, after `SKIP_CHOWN`, completed first-boot init and wrote
`init.json` into the `init` subPath. Kubelet creates missing subPath directories; the finding's
premise is false. That same live run is what surfaced the one genuine runtime defect lint
could not see (the entrypoint's fatal `chown` on an fsGroup-owned PVC, fixed with the image's
own `SKIP_CHOWN` escape hatch).

## Skill archive integrity (plan 10) — archived 2026-07-23

[docs/plan/10_skill-archive-integrity.md](./plan/10_skill-archive-integrity.md), delivered in one PR
(**#162**) for [#155](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/155). The change and its
reasoning are the CHANGELOG § [Unreleased] entry; recorded here is only what a changelog cannot hold.

**Why this was a plan, not a straight fix.** The read half that runs in a customer's BYOC worker
never touches the database, and the pinned SDK (v1.58.0, `betaskillversion.go`) gives the version
object no checksum field — so the expected digest had to reach it over the wire, a surface no
source records for the reference. That decision (ship the worker half now, on an additive response
header) and its alternative (executor-only, worker deferred) are materially different scopes, which
is what put the resolution in a plan rather than in the diff.

**Decisions evaluated and rejected.** (a) RFC 9530 `Repr-Digest: sha-256=:<base64>:` — the
standards-track field for exactly this. Rejected: nothing between our control plane and our worker
consumes the standard field, and shipping one algorithm with no `Want-Repr-Digest` and no
`Content-Digest` distinction advertises a contract we do not honor; a platform-specific
`x-skill-archive-sha256` is the smaller, honest claim. (b) The object store's own checksum — the
S3 ETag `minio-go` already returns and `internal/blob/s3` discards. Rejected: an ETag is not a
content hash under multipart upload and is backend-specific, so it cannot be the cross-backend
contract `blob.Store` needs; the digest belongs in the registry, which is already the trusted store.
(c) A `NOT NULL` digest column. Rejected as impossible rather than undesirable: the bytes a
pre-existing row's digest would be computed from live in object storage, which a SQL migration
cannot read. `NULL` therefore means exactly "written before migration 0010", is read unverified with
a log line, and a later migration can tighten the column once no such rows remain. (d) Making a
mismatch fatal to the tool run. Rejected: materialization's per-skill tolerance is the reference's
own contract, and turning one corrupt object into an outage of every session referencing it is
strictly worse than that session running without the skill — hence the skip under a distinct
`corrupt` outcome, which keeps integrity failures alertable apart from dangling references.

**Review hardening — the sentinel could inherit a weaker guarantee.** The Codex pass
(`gpt-5.6-sol`, config `ultra` effort) found the one real defect in the first cut, and it was in
neither half of the digest plumbing: the `.materialized` sentinel records only `{skill_id, version}`,
and both halves return *before* downloading anything when it matches. A rolling upgrade therefore had
a hole — control plane and migration deployed first, an old execution binary materializes a
now-digest-bearing version without verifying it and writes the legacy marker, and after that binary
upgrades the unchanged marker keeps matching, suppressing verification for the rest of the session.
On a platform whose premise is long-horizon sessions, "the rest of the session" is not a short
window. Fixed by giving the marker an integrity generation (`skills.SentinelVersion`) rather than the
reviewer's own first suggestion of recording the digests in it — that would force the BYOC worker to
spend a wire round trip per skill per pass, because it learns a digest only from the download
response, i.e. after the skip decision it is trying to make. The generation costs one
re-materialization per live sandbox at upgrade and nothing at steady state. The forced pass detects
and refuses a substituted archive but does not erase bytes an older binary already wrote — the
sandbox seam has no delete primitive, the residual already recorded for in-place tampering. A
knock-on the fix created: several `SentinelMatches` cases used bare-array fixtures that the new
parser rejects outright, so they would have passed for the wrong reason; they were re-expressed in
the current generation (via a `marker()` helper that follows future bumps) so they still exercise the
bijection and probe rules they were written for. Everything else in that pass checked clean: all
three writers covered with no fourth, both constructors hashing the exact bytes stored, `*string`
scanning distinguishing NULL from a value, `strings.EqualFold` not exploitable, migration 0010 safe
on a populated table, and the download endpoint's auth/404/Content-Length behavior unchanged.

**Verification that the tests are load-bearing.** Both refusal tests substitute the stored object
with a *different but perfectly valid* archive, so zip's own per-member CRC-32 cannot catch it and a
fixture cannot pass by accident. Each was run against a neutered guard to confirm it fails:
disabling the comparison in `skills.ReadArchive` let `"tampered instructions"` reach the sandbox in
the executor test, and suppressing the response header alone did the same in the worker test —
proving the worker's protection travels over the wire and is not incidentally supplied by anything
else. The sentinel-generation test was checked the same way: made to accept the legacy array form
again, it fails with the pre-verification bytes still in the sandbox and the old marker still
vouching for them — the reviewer's scenario, reproduced.

**Governance — a single-PR plan archives itself and leaves STATE.md alone.** The plan was authored
and archived within its one delivering PR, so STATE.md's Active work stayed **None**: it tracks work
in flight, and at merge there is none. The narrative lives in CHANGELOG; this is the same shape as
plan 07 below, and is only defensible *because* the plan lands `archived` rather than `in-progress`
with nothing left to do.

## Vaults plan — archived 2026-07-25, all four slices delivered + acceptance passed (#50)

docs/plan/12_vaults-credentials.md (issue #50) is archived complete: the wire-compatible `/v1/vaults` and nested credentials API with encrypted-at-rest secrets, session attachment via `vault_ids`, and the per-session egress gate that finally honors `limited` networking's `allowed_hosts` and substitutes vault credentials at egress time. Two explicitly-split-out follow-ons stay open as their own issues: BYOC gate delivery (#165) and TLS-terminating in-sandbox substitution (#166). Slice 1: `internal/secrets` cipher seam (`Cipher` iface, `local` AES-GCM + `openbao` transit backends, shared contract suite, `secretstest`), controlplane/executor plumbing, compose `openbao` + init one-shot, helm StatefulSet (PR #168). Slice 2: the wire-complete `/v1/vaults` + credentials CRUD — migration 0011, `vlt_`/`vcrd_` ids, the full `mcp_oauth`/`static_bearer`/`environment_variable` auth union, cipher-sealed write-only secrets, the live `mcp_oauth_validate` probe (PR #169). Slice 3: session `vault_ids` attachment — create-time accept/validate (existing + unarchived, `FOR SHARE`) and round-trip, update wire-faithfully rejected (PR #170). Slice 4, the egress gate, across many sub-PRs: the `sandbox.Spec.Env` seam (PR #172); the `internal/egress` substitution engine — `HostSet` matcher, deterministic `vltph_` placeholder, `Engine.Substitute` (PR #173); read-time resolution `internal/vaultresolve` + placeholder injection, unsafe-name skipping (PR #174); the `internal/gate` forward-proxy core — CONNECT + plain-HTTP host filters, substitution, hop-by-hop hygiene, bounded body (PR #175); gate-side cipher resolution, fail-closed (PR #176); per-session `internal/gatetoken` (mint/ensure/authenticate, revoke-on-re-mint) + migration 0012 (PR #177); the internal `GET /internal/v1/gate/config` endpoint + `internal/gateconfig` client on their own `gtk_` auth lane (PR #178); the `cmd/gate` + `internal/gaterun` runtime — owner-match iptables (v4+v6, verify-before-drop), fetch-and-swap (401→deny-all+terminate), `egress_request` spans, gate image + HEALTHCHECK-gated admission (PR #179); the Docker gate-pair go-live (PR #183) and the real-egress permit path with the contract suite rewritten to "limited = only allowed_hosts through the gate" (PR #186); `credential_host_unreachable_error` as a config-conflict event (PR #191), later detached and bounded (PR #200); the firewall reconcile into a gate-owned `MAP-GATE-EGRESS` chain (PR #194); and the K8s native-sidecar go-live with the gated contract row live on kind and the Helm `executor.gateImage` opt-in (PR #198). Gate-hardening deferrals raised during review: #196 (a sandbox image running as the gate UID), #197 (revoke on gated→ungated), #199 (sandbox-pod imagePullSecrets). Deliberate divergences and inferences are in docs/DIVERGENCES.md; the as-built system in docs/ARCHITECTURE.md.

## Vaults acceptance — real `ant` CLI + gated Docker sandbox, credential substitution end to end (run 2026-07-25) — ✅ passed

The full compose stack (controlplane + brain + executor + Postgres + MinIO + OpenBao + the built gate image), the brain routed to a real Anthropic-protocol gateway (a MiniMax endpoint from `.env`), the executor opted into the gate (`EXECUTOR_GATE_IMAGE` + `CONTROLPLANE_URL`). All resources created by the **real `ant` CLI** built from the local checkout, driving the control plane over `--base-url`: a vault (`vlt_…`), an `environment_variable` credential (`secret_name: HTTPBIN_TOKEN`, `networking.allowed_hosts: [httpbingo.org]`, header-only injection, a sealed `secret_value`), a `limited` environment (`allowed_hosts: [httpbingo.org]`), a bash-toolset agent, and a session with the vault attached. Two run-specific accommodations, neither touching what was proven. **The plan names `httpbin.org`; its public instance was returning 503 during the run, so the reachable plain-HTTP header-echo host `httpbingo.org` was substituted** — the only deviation from the plan's letter (the plan specifies the host but not the sandbox image). And the default `debian:stable-slim` sandbox image carries no HTTP client, so a one-off `debian:stable-slim + curl` image was set as `EXECUTOR_IMAGE` for the run (the sandbox base is a documented deployment choice, not a plan requirement).

Each assertion in the plan's acceptance, proven by driving the model to emit a `bash` tool call whose result carries the egress evidence:

- **Substitution on plain-HTTP egress.** The model emitted `agent.tool_use bash {"command":"curl -s http://httpbingo.org/headers -H \"Authorization: Bearer $HTTPBIN_TOKEN\""}`; the `user.tool_result` (`is_error:false`) carried httpbingo's echo with **`\"Authorization\": [\"Bearer s3cr3t-acceptance-value-xyz\"]`** — the real secret, which the gate substituted for the placeholder on the way out. The sandbox never held the secret.
- **The sandbox sees only an opaque placeholder.** `printf "SANDBOX_SEES=%s" "$HTTPBIN_TOKEN"` returned **`SANDBOX_SEES=vltph_8bc51b78b22e6faff3046b2ae87342c4`** — the deterministic `vltph_` placeholder, never the secret bytes.
- **A non-allowed host is refused by the gate.** `curl http://example.com/` through the injected proxy returned **`denied_host_status=403`** — the gate host-filtered it (example.com is outside the environment's `allowed_hosts`); a direct dial would separately be dropped by the owner-match firewall.
- **The management API never returns `secret_value`.** `GET /v1/vaults/{id}/credentials/{cid}` echoed the auth arm (`secret_name`, `networking`, `injection_location`) with **no `secret_value` field**.
- **Revocation half 1 — a fresh resolution mints no placeholder.** After `ant beta:vaults:credentials archive`, a new session's sandbox reported **`SANDBOX_SEES=[]`** — the archived credential contributes no env var.
- **Revocation half 2 — a reused pre-archive placeholder is no longer substituted.** In the same fresh session, replaying `vltph_8bc51b78…` in an `Authorization` header after archival, httpbingo echoed **the placeholder literally** (`"Bearer vltph_8bc51b78b22e6faff3046b2ae87342c4"`), not the secret. The gate substitutes from the config it fetches periodically from the controlplane, and that config is rendered from current rows at fetch time: the new session's gate fetched a config from which the archived credential — and its purged ciphertext — was already absent, so the placeholder has nothing to resolve to. (Revocation is thus observed once a gate holds a post-archive config — immediately for a new session's gate, as here; within the fetch interval for a gate already running when the credential is archived.) The host stayed reachable (the environment's `allowed_hosts` is independent of the credential) — only the secret is gone.

No `credential_host_unreachable_error` events were emitted (the credential's `allowed_hosts` matched the environment policy — no conflict). One healthy `map-gate-sesn_*` gate container ran per gated session; the gate's per-decision `egress_request` spans export to OTLP (Jaeger under the compose profile), not the session event log, and are unit-covered. No CLI, SDK, or wire accommodation anywhere — the unmodified `ant` binary drove every management call.

## Files plan — archived 2026-07-23, all four slices delivered (Files half of #55)

docs/plan/08_files.md (issue #55) is archived complete for its **Files half**; #55 stays open for git/repo mounting (`github_repository` resources). Slice 1: the wire-compatible `/v1/files` registry over object storage — migration 0008, `file_` ids, the multipart upload decode (one `file` part, filename/MIME validation, 500 MB budget), five endpoints, the newest-first `Page` list envelope, and the `downloadable`-column download gate (an upload is `downloadable:false`, refused 400) (PR #156). Slice 2: session `resources[]` accepting `type:"file"` mounts — `file_id` existence-checked in the create transaction, `mount_path` defaulting/validation, materialization into the `sessions.resources` jsonb, and the five management-only `sesrsc_` sub-endpoints (list/get/add/delete + update-reject); `github_repository`/`memory_store` stay rejected, keeping the union seam open for the git half (PR #157). Slice 3: materialization on the platform half — `Sandbox.WriteFileStream` on both backends (docker `io.Pipe` tar, k8s stdin-counting script) pinned by the sandboxtest contract, the executor's `materializeFiles` (row-authoritative existence, `.files_materialized` sentinel + `test -e` present-set probe, per-file tolerance), the brain's "Mounted files" system-prompt block with `files.injected`/`files.block_chars` span attributes and the `files.resolve.misses` counter, and the opt-in `file-answer` eval proving upload→mount→materialize→read (PR #158). Slice 4: the BYOC half — the worker's wire-only `SetupFiles` twin (session GET → `resources[]` → stream from the content lane into the sandbox, same sentinel/tolerance duplicated because the two halves never share a sandbox) and the environment-scoped content lane it reads through (`GET /v1/files/{id}/content` made dual-auth via `isFileReadPath`, the download handler made lane-aware to skip the `downloadable` gate and authorize by `fileMountedInEnvironment` — a jsonb-containment check filtered on `environment_id` — instead, 404 for anything no session in the caller's environment mounts), a `mountAtPath` guard on both halves so a mount at the sentinel's own path is not clobbered by the marker write (the slice-3 executor carried the same latent hazard, fixed here), plus the worker's `files.materialized`/`files.materialize.duration` instruments (this PR). Deliberate divergences and inferences are in docs/DIVERGENCES.md; the as-built system in docs/ARCHITECTURE.md.

## Files slice-4 acceptance — BYOC worker file materialization over the wire (run 2026-07-23) — ✅ passed (automated E2E; a real-CLI E2E-3 is N/A by divergence)

The Files plan's slice-4 acceptance is the automated over-the-wire test `TestSetupFilesOverTheWire` (`go test ./internal/worker`), not a manual `ant beta:worker` transcript — and that substitution is itself a documented consequence of the divergence, not a shortcut. The reference `ant` CLI's worker SDK runs `SetupSkills` but has **no file-materialization step at all** (docs/DIVERGENCES.md: "the reference has no worker file lane"); file mounting on the BYOC half is *our* worker binary's behavior, which the real CLI cannot exercise. So the acceptance drives our `internal/worker` against a **real control plane** (httptest-hosted controlplane over a disposable Postgres + a blob store, the worker SDK client authenticated with a minted environment key): a session mounts a seeded file, the worker's `SetupFiles` reads the session's top-level `resources[]`, pulls the mount's bytes from `GET /v1/files/{id}/content` **through the new environment-scoped content lane**, and streams them into the sandbox at `mount_path` — the same wire path a self-hosted worker takes, with no test-only shortcut around the control plane's auth. The test also pins the sentinel (an unchanged set skips re-streaming; a deleted-then-missing mount is caught by the `test -e` probe and restored) and, in `TestSetupFilesTolerance`, that a dangling mount 404s and is tolerated (`not_found`, never fatal) while a good mount still lands. The lane's auth boundary is pinned separately by `TestFileContentEnvironmentKeyLane` (`go test ./internal/api`): a mounted file downloads 200 on the environment key, an unmounted or cross-environment file 404s (indistinguishable from absent), and the `/v1/files` metadata GET, list, and DELETE routes 401 on that key — so the key reads only files that some session in its own environment mounts (a superset of, not restricted to, the one session a worker is servicing), never another environment's files and never a probe of their existence. (The session `resources` sub-endpoints' management-only 401s are pinned separately by the pre-existing `TestSessionResourcesManagementOnlyLane`, not by this test.) Mutation probes confirmed each guard load-bearing (scope check forced always-true → unmounted/cross-env leak to 200; the gate-skip removed → a mounted upload 400s; the `mountsPresent` conjunct dropped → a deleted mount is not restored; the `mountAtPath` guard neutered → a mount at the sentinel path is clobbered by marker JSON).

## Skills slice-5 acceptance — Level-1 injection, the full chain end to end (run 2026-07-22) — ✅ passed

The skills plan's slice-5 acceptance (plan E2E-2), against a real model: the opt-in eval task `skill-answer` (`RUN_EVALS=1 go test ./evals -run TestEvals/skill-answer`). The harness uploads a self-authored fixture skill through the public multipart endpoint — a SKILL.md whose instructions point at an `answer.txt` beside it, the answer being a per-trial `{{RECALL}}` token the prompt never contains — creates an agent referencing it at `latest`, and drives one turn that asks only for the passphrase, naming neither the skill nor a path — so the injected Level-1 metadata is the model's only route to learning a skill can answer and where it lives (the discovery mechanism under test; a turn that announced the skill would let the model succeed by exploring the filesystem even if injection regressed). The run log shows `skill created skill_id=… version=…` then `skill materialized session_id=… skill_id=… version=…`, and both graders passed: the transcript carries a tool call reading `skills/eval-secret/SKILL.md` (the agent found and followed the injected Level-1 metadata), and the final `agent.message` contains the `{{RECALL}}` passphrase — reachable **only** through the materialized answer file. Registry → brain resolution + Level-1 injection → executor materialization → the model acting on it, one unbroken chain exercised by a value the model could not otherwise know. (The turn was strengthened after CodeRabbit review to name no skill or path; the re-run above is green.)

## Skills plan — archived 2026-07-22, all five slices delivered

docs/plan/06_skills.md (issue #54) is archived complete. Slice 1: `internal/blob` + `blob/s3` (minio-go) + contract suite, compose/helm object storage (PR #145). Slice 2: the wire-compatible `/v1/skills` registry over object storage, migration 0007, `skillver_` ids, both upload forms, nine endpoints, E2E-1 compose round-trip + real `ant beta:skills` acceptance (PR #146). Slice 3: the run-once anthropic prebuilt-skills importer, date versions, idempotent, self-authored CI fixtures, real-checkout acceptance (PR #147). Slice 4: runtime materialization on both execution halves (executor blob-sourced + wire-only worker SetupSkills twin), the env-key dual-auth read lane, sentinel idempotence, the 500-skills cap, real-model + real `ant beta:worker` acceptance (PR #148, hardened across four Codex review rounds — the sentinel trust boundary, `latest_version` numeric-max concurrency, a hard-bounded archive read). Slice 5: brain Level-1 injection with the `skills.resolve.misses` metric, the E2E-2 eval, and docs closure (PR #152, hardened after a Codex + Claude/Opus + verifier review round — the resolve-miss counter is flushed before the turn's early-return paths so a logged miss always counts, the `model_request` span's `skills.injected`/`skills.block_chars` gained assertions, and the `ReadsSkillFile` eval grader was tightened to a real file read). Deliberate divergences and inferences are in docs/DIVERGENCES.md; the as-built system in docs/ARCHITECTURE.md.

## Skills slice-4 acceptance — materialization on both halves, real model + real `ant beta:worker` (run 2026-07-22) — ✅ passed

The skills plan's slice-4 acceptance, both deployment points.

**Cloud half — full compose stack, real model.** `docker compose up` (controlplane + brain + executor + Postgres + MinIO; brain routed to a real Anthropic-protocol gateway). A fixture skill uploaded via the loose-files form; an agent with `tools:[agent_toolset_20260401]` + `skills:[{type:custom, skill_id}]`; a session posted `user.message` "Run exactly this bash command and show me its output: cat skills/alpha-notes/SKILL.md". The model emitted `agent.tool_use bash {"command":"cat skills/alpha-notes/SKILL.md"}`; the executor provisioned the Docker sandbox, logged `skill materialized session_id=… skill_id=… version=1784657206256533`, and the `agent.tool_result` carried the SKILL.md content **byte-exact**; the model's closing `agent.message` quoted it and the session went idle with a clean stop. Registry → resolution (`latest` → concrete) → blob fetch → sandbox write → bash read, one unbroken chain.

**BYOC half (E2E-3) — the real `ant beta:worker`, whose SDK internals run SetupSkills.** Against a locally-run controlplane (disposable Postgres + MinIO): a `self_hosted` environment with a minted key, the same fixture skill, an agent referencing it at `latest`, and a session suspended on `bash cat skills/alpha-notes/SKILL.md`. The unmodified `ant beta:worker poll --base-url … --environment-key …` claimed the item and its log shows the reference worker's own materialization against this platform: `downloaded skill … skill_id=skill_jzp3zwh12qjx7rankcz9jgyj version=1784656902111009 dest=…/skills/alpha-notes` — the SDK resolved the `latest` alias by listing versions, fetched the version object, downloaded `/content`, and extracted onto its workdir, all through the new environment-key dual-auth lane; then `executing tool … tool=bash` and a posted `user.tool_result` (`is_error:false`) carrying the exact SKILL.md text. A second suspended command (`cat skills/alpha-notes/reference.md`) round-tripped the same way. No CLI or SDK accommodation anywhere.

---

## Skills slice-3 acceptance — real anthropics/skills checkout imported, listed by `ant` (run 2026-07-22) — ✅ passed

The skills plan's slice-3 acceptance: the run-once operator import against a **real, fresh clone of github.com/anthropics/skills** (cloned to a scratch directory — never into this repo, per the license red lines).

**Flow.** Disposable Postgres + MinIO; `go run ./cmd/controlplane -import-anthropic-skills <clone>` with only `DATABASE_URL` + `BLOB_*` set → all four document skills imported at version `20260716` — the clone's last commit date, resolved via git with no flag; the four real SKILL.mds passed the upload validation **unchanged** (descriptions 437–948 runes, under the 1024 cap). An immediate re-run reported `imported 0, skipped 4, failed 0` — idempotence over the same version, no storage traffic. The server was then started on the same database and the **real `ant` CLI** confirmed the catalog: `beta:skills list --source anthropic` returned `xlsx / pptx / pdf / docx`, each `latest_version 20260716`, `source anthropic`; `beta:skills:versions download --skill-id xlsx --version 20260716` streamed a real zip (PK magic) through the short-name id path — the id shape the slice-2 API was built to accept ahead of this slice.

---

## Skills slice-2 acceptance — real `ant beta:skills` against the registry (run 2026-07-21) — ✅ passed

The skills plan's slice-2 acceptance (docs/plan/06_skills.md): the **real `ant` CLI** (built from the local checkout) driving the new `/v1/skills` registry, zip form — the only form the CLI can emit, since it basenames every part filename, which makes it the canonical compatibility probe.

**Setup.** Disposable Postgres + MinIO containers, `cmd/controlplane` run locally with `BLOB_*` pointed at the MinIO, a fixture skill (`financial-skill/{SKILL.md,reference.md}`) zipped locally.

**Flow exercised, all against `--base-url` with unmodified CLI commands.** `beta:skills create --file financial-skill.zip` → the Skill object (`skill_` id, `display_title` defaulted from the frontmatter name, epoch-microsecond `latest_version`, `source:"custom"`); `beta:skills list` / `retrieve` echo it; `beta:skills:versions create` mints a second version (`skillver_` id, name/description/directory extracted from SKILL.md) and `latest_version` follows; `beta:skills:versions list --limit 10` returns both, newest first; `beta:skills:versions download` streams the archive **byte-identical** to the uploaded zip (`cmp` clean); `beta:skills delete` with versions still present is the wire's `400 invalid_request_error`; both `beta:skills:versions delete` calls echo the **version timestamp** as the deleted id (`skill_version_deleted` — the reference's asymmetry); the final `beta:skills delete` returns `skill_deleted` and a retrieve after it the enveloped `404 not_found_error`. No CLI flag, path, or field needed any accommodation.

---

## Slice-8 acceptance — real `ant beta:worker` end to end (run 2026-07-16) — ✅ passed

The plan's slice-8 acceptance, deferred until the local stack existed, has now been run and **passed**.

**Setup.** The docker-compose stack (controlplane + brain + Postgres, no executor — the worker replaces it for self-hosted), the brain pointed at a real Anthropic-protocol endpoint (a MiniMax gateway, from `.env`). A self-hosted environment, an agent carrying the built-in `agent_toolset` (bash), and a session — all created by the **real `ant` CLI** (built from the local checkout) driving the control plane over `--base-url`. An environment key was seeded directly into the DB (the issuance primitive `EnsureEnvironmentKey` has no operator surface yet — see Deferred below).

**Flow exercised.** A `user.message` asking the model to run `echo hello-from-byoc-worker` → the brain called MiniMax, which emitted `agent.tool_use` (`bash`) → because the agent toolset defaults to `always_allow` and the environment is self-hosted, the brain enqueued a self-hosted `tool_exec` work item → a real **`ant beta:worker poll`** (also the local-checkout binary, authenticated with the seeded environment key) polled the environment's queue, claimed the item, reconciled the session by listing its events, ran `bash` in its in-process runner, and posted the `user.tool_result` (`hello-from-byoc-worker\n`) → the brain resumed, produced the final `agent.message` quoting the output, and the session went `idle`. The full event log confirmed the round-trip (`user.message` → `agent.tool_use` → `user.tool_result` → `agent.message` → `session.status_idle`).

**Bug found and fixed (this PR).** The worker's reconcile step — the SDK `SessionToolRunner` listing the session's events, `GET …/events?limit=1000` — was rejected `400` by our shared list cap of 100 (poll and ack had already succeeded), so the worker could never read the outstanding `agent.tool_use` and no tool ran. The reference's general list convention is 1-to-1000 (documented on most SDK list params; the agents list is the documented exception at "maximum 100"), and the worker requests exactly 1000, while the SDK's event-list param documents no explicit cap — so the events endpoint gets `maxEventLimit = 1000` (a compatible upper bound, not a proven reference cap; some cap is needed since an unbounded limit is a query-cost risk) while the other lists keep 100. With the fix the acceptance passes end to end.

**Deferred follow-up.** There is no operator-facing way to **issue** an environment key: `EnsureEnvironmentKey` exists and is tested but is wired to nothing (no endpoint, no CLI, no bootstrap seed). The reference mints these off-wire (its console), so a self-hosted operator needs an equivalent primitive. Tracked for a follow-up iteration (design choice pending: an off-wire admin command vs. a management endpoint).

---

## Completed

Pruned 2026-07-18 (plan 03). Each subsection's delivery narrative lives **once** in
[CHANGELOG.md](../CHANGELOG.md) — find it under § 0.1.0 (or § [Unreleased] for later
work) by the same slice/PR tag the heading carries; sections without a tag carry their
own pointer line below — and its per-file reference in
[ARCHITECTURE.md](./ARCHITECTURE.md) § "Package reference". The headings survive as
citation anchors for [DIVERGENCES.md](./DIVERGENCES.md); where a citation's parenthetical
quotes prose from a pruned body, that prose lives on in the matching CHANGELOG entry or
ARCHITECTURE row. What remains under a heading is recorded nowhere else.

### Repository & tooling

*Narrative: the foundation entries at the bottom of CHANGELOG § 0.1.0 — "Project
foundation", "CI pipeline", "CI coverage gate", "GitHub checks", "Dual code review",
"Docs-consistency rule", "STATE.md".*

### `internal/domain` — Anthropic-native core types

*Narrative: CHANGELOG § 0.1.0 → "Project foundation" (slice 0).*

### `internal/telemetry` — OTel init + W3C trace-context propagation

*Narrative: CHANGELOG § 0.1.0 → "`internal/telemetry` — OTel foundation"; the log bridge
is under § [Unreleased] → "OTel logs on the execution chain".*

### `internal/store` — Postgres schema + migrations

*Narrative: CHANGELOG § 0.1.0 → "`internal/store` — Postgres schema + embedded
migrations (slice 1)".*

### `internal/api` + `cmd/controlplane` — wire-compatible control-plane CRUD

*Narrative: CHANGELOG § 0.1.0 → "`internal/api` + `cmd/controlplane`" (slice 2).*

### `internal/events` + events API — append-only log, send/list, SSE stream (slice 3)

### `internal/provider` — config-driven model access (slice 4)

#### OpenAI-compatible adapter (`internal/provider/openai`)

*Narrative: CHANGELOG § 0.1.0 → "OpenAI-compatible provider adapter".*

### `internal/brain` + `internal/queue` + state machine — the orchestration loop (slice 5)

### `internal/sandbox` — the hands (slice 6, first part)

A disposable container per session, driven over the Docker Engine API. This section is
kept nearly whole: it is the canonical record of how the exec deadline was driven to its
final design by seven rounds of adversarial review — measurements, attacks, and the
chronology of verifier passes refuted by reviewers — which exists nowhere else.

**Slice-6 decisions (documented divergences, all deliberate):**
- **No `Attach`.** The plan's `SandboxProvider` had `Provision` + `Attach(sandboxID)`. `Provision` is instead idempotent per session — it adopts the session's running container — which is the only thing an executor ever needed `Attach` for, and it spares us persisting a sandbox id nothing else would read.
- **`Glob`/`Grep` are not on the interface.** The plan listed them on `Sandbox`. They are pure functions of `Exec` and the file primitives, so they belong once in the toolset layer rather than re-implemented by every backend. `Checkpoint` is likewise absent: the plan marks it 后续, and a seam is an interface boundary, not a method every backend must stub.
- **A deadline is enforced twice, and only the outside one is a guarantee.** Docker has no API to kill a running exec, so the killing has to happen inside the container: a wrapper `exec`s the command — so the command *becomes* the exec's own process — after forking a watchdog that, at the deadline, `kill -9`s the command's **process group** (`set -m` makes it a group leader). But that watchdog is a process the command can find and kill. So `Exec` never lets the container decide. It stops waiting `Timeout + 2s` after the deadline (`killGrace`), and it decides the verdict from outside. The watchdog still earns its keep — an honest command's runaway loop stops burning CPU, and the sandbox learns a real exit code — it is simply never believed.
  **Why the command *becomes* the exec, rather than running under a wrapper shell.** The pid Docker reports for an exec is what `Exec` watches from outside. If that pid is a wrapper and the command is its child, the command can `kill $PPID` to make the watched pid vanish while it runs on — a reviewer (Codex) demonstrated exactly that against the earlier design: kill the watchdog, kill the wrapper, run 2s past a 1s deadline, and the call reports `ExitCode=137 TimedOut=false`. `exec`ing the command closes it structurally: the watched pid *is* the command, and a command cannot kill itself and keep running. (The shared contract test now kills the watchdog **and** the parent a prober might watch, then runs long — it fails against the old wrapper and passes against this one.)
  **The verdict is two probes of one question: was the command's process still alive?** Alive as the deadline arrived, and the exec exits 137? Then the watchdog killed it — a timeout. Still alive once the deadline *and* `overrunSlop` (500 ms) had passed? Then no exit code it later reports can be believed, because on the honest path the watchdog would already have killed it — also a timeout.
  **The guarantee, and its soft edge.** A command cannot outrun its deadline by more than `killGrace` — `Exec` stops waiting on its own clock and reports `TimedOut` whatever the container does; that bound is hard. Detecting an overrun *within* that window is softer, because it rests on `top`, and `top`'s answer reflects when the daemon ran `ps`, not when the probe fired — so a command that overruns, exits inside the grace window, and induces the daemon to run its confirming `ps` only after that exit can have the overrun read as a clean finish (up to `killGrace`, and only under attacker-timed daemon delay). The hard bound holds regardless; what leaks is the sub-`killGrace` overrun *label*. Its real containment is the reserved cgroup limits (`PidsLimit`, `CpuQuota`), which also cap the process-count pressure a command needs to stall `ps` — the probe is the cheap first line, not the wall. `TimedOut` is the authoritative field; `ExitCode` may be the kill's, or the one a command that dodged the kill picked for itself.
  Consequences worth knowing: the wrapper keeps **no state inside the container** (a marker file, the first design, let a command forge a timeout it never hit or erase one it did); the deadline a command must have been alive at is the watchdog's rounded-up one, not the caller's request, so a command that kills itself early is never mislabelled; timeouts round up to whole seconds (`sleep` takes no portable fractional argument); the first probe fires `probeLead` (50 ms) *before* the deadline, because at the deadline a command the watchdog has just killed looks exactly like one that was never running — the price is that a command SIGKILLing itself inside that 50 ms reads as a timeout. The watchdog polls `kill -0` on the command rather than sleeping the whole deadline, so an honest command that finishes early takes its watchdog with it within one poll — no stray `sleep` piles up across a session's thousands of quick commands. Its own stdio goes to `/dev/null`; because the command *is* the exec, its stderr reaches the tool result untouched, and a SIGKILL leaves no shell "…Killed…" line behind to begin with.
- **The probe reads the daemon's process list, because both obvious clocks are wrong.** The exec's **output stream** does not close when the command exits: a process the command backgrounds inherits its stdout, and the daemon holds the stream open for it — measured at ~2s, then force-closed. And the daemon's own `Running` flag on the exec tracks that same stream rather than the process — measured at **2.06s** for a command whose process was gone in **40ms**. Timing either one charges a command for stragglers it never waited for, and `sleep 300 & echo started` under a one-second deadline came back `ExitCode: 0` *and* `TimedOut: true`. What does track the process is `GET /containers/{id}/top`, cross-referenced with the exec's `Pid`: `ps` runs on the daemon's host, needs nothing from the image, and is the one view of the sandbox that the sandbox cannot edit. The same measurement fixes the other end — once `Exec` gives up on a stream it asks for an exit code only when the probes say the command is gone, because a real daemon publishes one 1.7s after such a stream is closed and never publishes one for a command still running.
  Two safety properties fall out of doing it this way. A pid the daemon will not name is **fatal**, not ignored: a zero pid would answer "gone" to every probe and disarm the deadline in silence. And a probe the daemon will not answer counts the command as **still running**, because hiding an overrun breaks the guarantee while mislabelling one costs a tool call. That fail-open direction is deliberate, and the overrun probe is careful about it in one more way: its confirmation runs on a clock the output stream's close cannot stop, because a command that overran and then exited *during* its own confirming `top` would otherwise have that exit — not its overrun — read as the answer, and the overrun erased (a reviewer, Codex, found exactly that: the confirmation rode the stream's cancellation, so a clean exit mid-probe came back `TimedOut=false`). What the fail-open direction costs is paid by an honest command a broken `top` can no longer tell from an overrun: one that finished on time but left a straggler holding its output open past the deadline. It reads as a timeout while `top` is down; a working `top` sees the command's pid already gone and clears it, so this misread costs a tool call, never a hidden overrun — a `top` *outage* fails toward the timeout label. (A command that SIGKILLs itself inside the 50 ms probe lead is a separate, unconditional cost of sampling a lead ahead of the deadline: the pre-deadline probe cannot tell a self-kill in that window from the watchdog's, so it reads as a timeout regardless of `top` — the probe-lead cost noted above, not a `top`-outage cost.) Where `top` can still mislead the other way is not an outage but a late-run `ps` answering as of after the command exited: that is the soft edge in the guarantee above, and the reserved cgroup limits, not this probe, are its containment.
- **What the deadline does *not* do: reclaim the container.** The kill is a process *group*, not a process tree, so a child that calls `setsid` escapes it; and when `Exec` stops waiting it abandons the exec — closing the HTTP stream is not a kill primitive. Either way a process the command left behind keeps running, and a sabotaging command can pin a core, until the session's container is destroyed. Every `Exec` call is bounded, so no session wedges, and no command that outlives its deadline escapes the timeout label. What escapes is a process detached from a command that *finished inside* its deadline — `nohup work & exit 0` is honest by every measure the sandbox has, and the work goes on. The real answer is cgroup limits (`PidsLimit`, `CpuQuota`) at provision time, which belongs with the container hardening item, not with the exec path.
- **A container is adopted only if the platform owns it.** `Provision` names a container after the session, so the name alone is not evidence: a container left by an earlier deployment, or one that collides, can wear it. Both adoption paths (an existing container, and the loser of a create race) check the `dev.opensdlc.managed-agent-platform.session-id` label first. The point is not only isolation — a container's **network mode is fixed when it is created**, so adopting a foreign container is how a `limited` session would quietly acquire a `bridge` container's route out, defeating the fail-closed rule. This is not a trust boundary against a hostile daemon co-tenant — the label is world-writable and forgeable by anyone with daemon access, who already owns every sandbox on the host — it defends against the accidents, which are the realistic failure on a single-tenant daemon.
- **404s are classified by the daemon's own message, anchored.** The archive endpoints echo the requested path into their missing-path error, so a substring search for "No such container" let a file *named* that turn its own missing-file error into a destroyed-sandbox error. The exec endpoints have a third 404, "No such exec instance", which is a lost exec and not a lost sandbox.
- **`Exec` itself is stateless per call.** Each `Exec` is its own `docker exec` of `/bin/bash -c`, so `cd` does not survive between calls at this layer. The reference's `bash` tool *does* persist state (`restart: true` resets it); that persistence is built one layer up, in `internal/sandbox/shell`, as a pure function of this stateless `Exec` plus the file primitives — deliberately, so the deadline `Exec` enforces from outside the container is inherited verbatim by every command.
- **A command that backgrounds a process without redirecting its output still costs ~2s.** The command's *result* is correct and its deadline is judged on its own life, but the call does not return until the daemon force-closes the stream the straggler is holding. `docker exec` behaves the same way; `cmd >/dev/null 2>&1 &` returns immediately. Cutting the read short as soon as the probes see the command exit is possible and was not done: it would drop output the command itself wrote and the daemon had not yet flushed, to save two seconds on a tool call the agent chose to shape this way. If it ever matters, it belongs behind the toolset's `bash` tool.

**Test coverage:** the contract suite runs against a real Docker daemon (a missing daemon is a hard failure, not a skip — as with `pgtest`), using `debian:stable-slim`: the official `bash` image does *not* have `/bin/bash` (its bash is in `/usr/local/bin`), which is exactly the assumption the image contract pins. It asserts the deadline kills the command's children along with it, leaves no watchdog behind, leaks nothing into the tool result's stderr, does not mistake a straggler for the command, and does not read a fast exit 124 — the code GNU `timeout(1)` gives a killed command — as a timeout of its own. The crown-jewel subtest tears down every guard the command can see — a watchdog beside it or below it, and the parent a prober might watch in its place — then holds a marker alive past the deadline, and asserts the command never both outlives its deadline and is reported finished; a third case overruns and then exits clean, leaving no marker to see, so the timeout has to be called from the overrun alone; a fourth mutes its own stdout and stderr and then overruns, because a reviewer argued a command could EOF the output stream early and cancel the probes by closing its own fds — measured against a real daemon, it cannot: the daemon holds the exec stream open until the *process* exits, so closing the container-side fds does not close it, the abandoned-plus-overran path still fires, and the command reads as a timeout. It passes against this backend and *fails* against the earlier fork-a-child wrapper, which is the whole reason it is in the shared suite rather than the Docker tests: a backend that enforces its deadline only inside the sandbox, or that watches a killable wrapper, fails it while passing every other assertion in the file. A scripted fake daemon over `httptest` covers what a real one will not reproduce on demand — and models an exec the way a real one runs it, with the process's life and the output stream's life set independently: image-missing → pull → retry, a lost create race adopting the winner's container, a container that wears the session's name without its label, a pull failure delivered inside a 200 stream, garbled and non-JSON replies, start/exec/inspect failures, an exec the daemon never stops calling running, context cancellation mid-poll, symlink and oversize and truncated archives, a `No such exec instance` 404, a path whose own name is `No such container`, the daemon's system-error stream frame, a command that overran and then exited *during* its own `top` probe with the stream closing mid-request, and the unix-socket transport itself.

Every guard is mutation-tested (disable it → exactly its own test fails). Seven rounds of review plus the mutation pass drove the deadline to where it is; the first six found real defects, all fixed here: a non-container 404 from `WriteFile` reported as `ErrNotFound`; the `/tmp` marker a sandboxed command could write to forge or erase its own timeout; a single-PID kill that left the command's children alive; bash's killed-job announcement landing in the tool result's stderr; a leaked watchdog `sleep` per timed command; `TimedOut` compared against the caller's deadline rather than the watchdog's rounded-up one; a path whose name is `No such container` turning its own missing-file error into a destroyed-sandbox error; a stale exec id read as a destroyed sandbox; the daemon's system-error frame folded into stdout as if the command had printed it; a container adopted by name without checking the ownership label; the overrun confirmation cancelled by the command's own stream close, so a clean exit mid-probe erased the overrun; and the two deadline probes run in sequence, so a first probe stalled on a slow `top` left the overrun probe unreached. The seventh round found a residual left in by design rather than a defect fixed here: `top` reports as of when the daemon ran `ps`, so a command that times a daemon `ps`-stall to land just after its own exit can hide a sub-`killGrace` overrun — accepted and documented (in the guarantee above, and as (7) below), with the reserved cgroup limits as its real containment. An eighth round — a fresh Codex (`gpt-5.5`/`xhigh`) and Claude pass on the final branch — turned up no new fixable bypass in the deadline. Its headline finding, a command muting its own stdout and stderr to EOF the stream and cancel the probes before they fire, was reproduced against a real daemon and *did not hold*: the daemon keeps the exec stream open until the process exits, so closing the container-side fds does not close it, and the muted overrun still reads as a timeout (now pinned by the fourth crown-jewel case). Its second finding is a narrow same-family limitation, not a fix: `execPid` polls on the caller's context before the probes are scheduled (anchored to `start`), so a daemon pathologically slow to report the exec's `pid` — slower than `deadline − probeLead` — can push the pre-deadline probe past the watchdog's kill, and a command the watchdog *did* contain reads `TimedOut=false`. Like the seventh, it is a degraded daemon weakening the sub-`killGrace` *label*, never the hard bound (the command was killed on time) and never a hidden overrun; the reserved cgroup limits are the family's real answer. The round's kept findings were otherwise doc and test hygiene: the restored fast-exit-124 guard above, the README daemon rationale, and a `top`-outage note that had wrongly implied a healthy `top` clears an honest self-kill inside the probe lead — it does not, that cost is unconditional.

Seven came from **the sandboxed command attacking its own deadline**. Six were reproduced and fixed; the seventh was reproduced and accepted as a documented residual. (1) Kill the watchdog, run 4.4s past a 1s deadline, report success — fixed by bounding the wait in `Exec`. (2) Once the bound existed, the narrower trick: kill the watchdog, overrun the deadline, and exit *inside* the grace window, so the bound never fires and the exit code is the command's own (measured **2.06s under a 1s deadline, `ExitCode=0 TimedOut=false`**) — fixed by the two probes. (3) The mirror of it, an *honest* command accused: a backgrounded process holding the exec's stdout made `sleep 30 & echo started` read as a timeout — fixed by timing the command's process, not its stream. (4) With the command running as a child of a wrapper shell, `kill $PPID` made the pid the probe watched vanish while the command ran on (measured **2s past a 1s deadline, `ExitCode=137 TimedOut=false`**, reported by two independent reviewers) — fixed by `exec`ing the command so the watched pid *is* the command. (5) Subtler still, once the probes matched the exec's pid: kill the watchdog, overrun the deadline, and exit *during* the overrun probe's own `top` request — the command's stream closes, `Exec` stops probing, and a confirmation riding that cancellation reads the command as finished and returns `TimedOut=false` for a real overrun (Codex, against the exec-wrapper commit) — fixed by running the overrun confirmation on a clock the stream's close cannot stop. (6) The sibling of (5), found re-reviewing that very fix: with the two probes run in sequence, a *first* probe stalled on a slow `top` was still waiting when the command overran and exited, so the stream's close cancelled the whole wait before the overrun probe was ever reached (Codex again) — fixed by running the two probes on independent clocks, so nothing can keep the overrun instant from being measured. (7) The limit behind (5) and (6): even with the probes independent, `top` reports as of when the daemon ran `ps`, not when the probe fired, so a command that overruns, exits inside the grace window, and induces the daemon to run its confirming `ps` only after that exit hides the overrun — bounded by `killGrace`, and only under attacker-timed daemon delay (Codex, re-reviewing the independent-probes fix). This one is accepted and documented rather than fixed: a robust fix means concurrent rapid polling with its own cost and failure surface, and the plan already reserves cgroup limits as the real containment for a command that abuses the daemon — limits that also cap the process pressure needed to stall `ps`. (1)–(4) were each closed with a contract test that fails against the vulnerable code, binding every backend; the general invariant behind (5) and (6) — overran, then exited clean — is a shared subtest too, but the specific `top`-probe races are ones a shared test cannot stage against a real daemon, so they are pinned by Docker unit tests. A verifier pass asserted (2) did not exist and a reviewer proved it did; a verifier pass certified the fix for (1)–(3) and a reviewer found (4) in it; a verifier pass certified (4) and Codex found (5) beside it; a verifier pass certified (5) and Codex found (6) in the re-review; a verifier pass certified (6) and Codex, re-reviewing that fix, found (7) — the point at which the tool reaches the limit of what `top` can witness, and the residual is documented rather than chased. The pattern is why the deadline is pinned by adversarial tests that fail on the vulnerable code, not by a verdict.

The last round found the defect from the other direction — an honest command **accused** of a timeout. A reviewer reasoned that a backgrounded process holding the exec's stdout would stall `Exec` for the full grace period and make it report `TimedOut` and SIGKILL. Reproduced against a real daemon, the mechanism was wrong (the daemon force-closes such a stream after ~2s) but a worse bug was underneath: `sleep 30 & echo started` under a **one-second** deadline returned `ExitCode: 0` together with `TimedOut: true`. Redirecting the straggler's output made it vanish, which named the cause. The fix retired the stream as a clock. Its replacement was chosen by measurement, not by reading the API: the daemon's `Running` flag turned out to track the stream too (2.06s versus the process's real 40ms), and only `top` tracks the process. The mutation pass separately caught a *test* that passed for the wrong reason — the caller-cancellation case tripped during `execStart` and never reached the branch it claimed to pin.

### `internal/sandbox/shell` — the persistent bash tool (slice 6, second part)

Known limits, each an accepted cost rather than a fix (the delivery narrative and the
divergences-from-a-resident-shell list are in CHANGELOG.md § 0.1.0):

- **Snapshots and command files accumulate.** Every call writes both and nothing prunes them; `restart` prunes nothing either. The container is per-session and disposable and each snapshot is a few KB, so a long session costs tens of MB and a destroyed sandbox takes it all with it — a garbage collector is not worth its failure modes yet.
- **A `Run` that fails *after* the command ran discards the command's output.** The snapshot probe or the `head` write against a broken container returns the error with an empty `Result`. That is deliberate: the alternative hands the caller a transcript for a call whose state the next call will not see, which invites treating it as committed. Both reviewers raised it; it stays a documented choice rather than a fix.
- **Concurrent `Run` calls on one session race on `head`.** Two in-flight calls snapshot into separate directories and the last to commit wins. The shell package itself does not lock `head`; serializing a session's bash calls belongs with the caller. (Since partly superseded: the executor now serializes a session's `tool_exec` work per-(session, kind), so the platform's own path cannot race — the unlocked `head` remains a property of the package.)
- **A cwd whose name ends in a newline is not round-tripped.** The snapshot stores the cwd with `pwd` and restores it through `$(<file)`; command substitution strips *all* trailing newlines, so a directory whose own name ends in one is restored a byte short and the `cd` lands elsewhere or is skipped. It is a pre-existing property of the substitution (the earlier `$(cat …)` behaved identically), it costs only cwd (exports, functions, aliases, options are unaffected), and a newline-terminated directory name does not occur in real use — reading the value byte-exact to handle it is machinery a hot, safety-critical path does not warrant. Codex raised it.

Every guard in the package is mutation-tested — disable a guard and exactly the test that
pins it fails. (The full mutation matrix was pruned with the delivery narrative; the
tests themselves are its ground truth.)

### `internal/toolset` — the built-in tools (slice 6, third part)

Two accepted-cost notes recorded nowhere else:

- **`singleQuote` is a third copy** of the POSIX single-quote escape (`sandbox/docker` and `sandbox/shell` have their own). Each is one line and independently tested; a shared home would couple three sandbox-tree packages for a trivial helper, so the duplication stays, as the shell package's own note already accepts for its copy.
- The glob pipeline's failure handling is carried on reasoning rather than a failing-on-the-old-code test: a broken pipeline (a `stat` without `--printf`, a missing tool) and a mid-listing `stat` race are both non-deterministic to stage against a real GNU-coreutils container, so `pipefail`'s "never a silent no-matches" guarantee rests on the happy-path glob tests proving the NUL-delimited pipeline works plus the argument that any masked mid-pipeline failure would otherwise read to the model as an empty directory. A second reviewer (Codex) confirmed the masking behavior a `pipefail`-less pipeline would have had, which is why `pipefail` is kept and the per-file-race-errors-conservatively cost is accepted.

### `internal/executor` + `cmd/executor` + brain `agent_toolset` expansion — the closed loop (slice 6, fourth part)

### permission policies + the confirmation round-trip (slice 7)

Two records CHANGELOG's entry does not carry — a liveness design decision, and the
review-hardening acceptance record (each correctness fix landed with a test that fails on
the vulnerable code):

**Liveness — a gated turn re-idles rather than chains, and that is deliberate.** Both the brain's suspend and the API's partial re-idle commit under the session row lock with no chain-or-idle check (unlike `end_turn`): a session blocked on human confirmation is genuinely waiting on input, and a `user.message` that arrived mid-turn stays unprocessed and rides the next replay once the gate clears — it is never lost, and the session is not spuriously woken past a pending approval. This mirrors the slice-6 running-suspend, which also relies on replay-on-resume for mid-turn input.

**Review hardening (same PR, from the dual review + verifier).** A Codex (`gpt-5.5`/`xhigh`) pass, the verifier (opus), and the Claude review (also `gpt-5.5`-class) converged; seven gaps — six correctness, one cleanup — were fixed here, each correctness one with a test that fails on the vulnerable code:
- **A `user.message` no longer bypasses the confirmation gate.** Slice 7 is the first to leave a session *idle* while it still carries an unanswered `agent.tool_use`, and the `user.message → running + model_turn` trigger did not check for that: a message posted while awaiting approval woke a turn whose replay hands the model an assistant `tool_use` with no matching result — a request the Messages API rejects, producing a spurious `session.error`. `POST /events` now computes the session's unconfirmed-ask blocking set up front (`UnconfirmedAskEvents`) and a `user.message` resumes only when it is empty; while gated the message appends and rides the next replay once the gate clears. The confirmation case is checked **first**, so a batch mixing a gate-clearing confirmation with a `user.message` runs the confirmed tool rather than waking on the message past it. (All three reviewers found this independently — the strongest signal in the pass.)
- **A tool result can no longer answer an unconfirmed ask tool.** `ValidateToolResults` accepted a `user.tool_result` for an ask-gated `agent.tool_use` (it only checked kind and already-answered), so a self_hosted client could answer a gated tool before approval — bypassing the human gate and, on a later denial, double-answering the tool use on the append-only log. It now rejects a result for an ask-gated tool that has no confirmation. (Codex.)
- **A malformed toolset is a 400 at agent creation, not a turn-time wedge.** With `Tools` now resolving `permission_policy` (via the shared `resolveToolset`), an agent whose `agent_toolset` carried a bad policy — or a malformed `enabled` — was accepted at create (`parseTools` treated the entry as opaque) and then failed *every* turn when the brain resolved it, an unusable agent with no create-time error. `parseTools` now calls `toolset.Validate` on each `agent_toolset` entry, so the resolver's own check runs at creation. (Claude.)
- **A denied gate no longer provisions a sandbox for nothing.** The confirmation resume chose `tool_exec` vs `model_turn` from `HasUnansweredToolUse`, which counts client-executed `agent.custom_tool_use` as unanswered — so denying an ask tool while a custom tool was still outstanding enqueued a `tool_exec` the executor would claim, provision a container for, and find no built-in to run (a wasted provision; an infinite reclaim if provisioning failed). The resume now enqueues a `tool_exec` only when an allowed *platform* tool is unanswered (`HasUnansweredPlatformToolUse`); when the only remaining work is a custom tool it enqueues nothing and waits for the client's result, and when every tool is answered it resumes the brain — mirroring the non-ask suspend, which never runs an executor for a custom-only turn. (Claude.)
- **Confirmation gating is scoped to `agent.tool_use` only.** `confirmableToolUseTypes` also listed `agent.mcp_tool_use`, but the denial synthesis emits an `agent.tool_result`/`tool_use_id` — the wrong shape for an MCP tool (`agent.mcp_tool_result`/`mcp_tool_use_id`). MCP is not gated in v1 (the brain stamps a policy on nothing else), so the speculative entry was removed; gating MCP is slice-8+ work that must extend the denial synthesis with it. (Codex.)
- **Policy validation is uniformly lazy.** `resolveToolset` validated `default_config.permission_policy` eagerly while per-tool policies were validated only for enabled tools, so a malformed default rejected an agent even when every enabled tool overrode it or the toolset was off. Now a policy is validated only for a tool that actually resolves into the enabled set — a malformed policy on a disabled or overridden-away tool has no effect and is ignored. (Codex.)
- **(cleanup) One `classify` pass replaces the parallel `classifyTools`/`classifyPolicies`.** Both re-ran `resolveToolset` over the same entry each tool-calling turn, and `classifyTools` marshaled every built-in definition back to JSON just to recover its name; `classify` now resolves once and reads names from `toolset.Policies`' keys, so the two maps (name→event type, name→policy) can no longer drift. (Claude.)

### work API — environment-key auth + `/work/poll` (slice 8, first part)

### work API — the work-item lifecycle: get / ack / heartbeat / stop (slice 8, second part)

### work API — environment-key auth on a session's worker-facing routes (slice 8, PR C1)

### work API — the work-items list (slice 8, PR C-list)

### BYOC worker — the tool-exec driver over HTTP (slice 8, PR C2a)

### BYOC worker — the lease loop + `cmd/worker` binary (slice 8, PR C2b)

### BYOC worker — `traceparent` propagation across the process boundary (slice 8, PR C2b-2)

### work API — the work-item metadata update (slice 8, PR C-meta)

### work API — the queue-stats endpoint (slice 8, PR C-stats)

## Kubernetes sandbox provider (slice 9)

Delivery narrative: the four slice-9 entries in CHANGELOG.md § 0.1.0 (Kubernetes sandbox
provider; config-driven backend selection; Helm chart; the compose stack). Per-file
reference: [ARCHITECTURE.md](./ARCHITECTURE.md) § "Package reference".

---

## Harness decisions — evaluated and rejected (2026-07-16)

Reviewed against the loop-engineering playbook (state file / objective gate / skills /
automations) with all v1 slices done. Rejected: a gofmt PostToolUse hook (near-zero value
over CI's gate), a fifth code reviewer (four checkers + CI already run per PR; the
marginal yield is unmeasured), and a standing LESSONS.md ledger (CHANGELOG, this archive,
and CLAUDE.md already cover its content). `/loop` and git worktrees were **not**
rejected — they are planned future practice. This restructure put the first prerequisite
in place (the slim state file); a single executable gate (`make verify`) and an on-demand
review skill land in follow-up PRs.

---

## Eval test system (phase 1, #30)

The first test that drives a whole session the way a customer does — public REST → brain → a real model → work queue → executor → Docker sandbox → SSE idle — and grades the transcript deterministically. Every other loop test in the repo scripts the provider; nothing before this exercised the real product path, and #30 also flagged that `.env` presence alone was triggering a paid model call during an ordinary `go test`.

**Harness form.** A top-level `evals/` package of `*_test.go` only — no runner binary, because `go test` already gives subtests, timeouts, `-v`, and panic-safe cleanup. `TestEvals` composes the platform in one process exactly as `cmd/*` do: a `pgtest` Postgres, the real `api.NewHandler`, a `provider.Registry` routing `*` to the `.env` endpoint, and live `brain` and `executor` loops against `docker.New`. Only `main()` glue is bypassed (CI's compose job smokes that). A hand-rolled REST client speaks `map[string]any`, never the domain structs, so a wire regression a struct tag would round-trip past stays visible to a grader. The suite is opt-in through `RUN_EVALS`, a second gated tier in `internal/modeltest` alongside the provider smokes' `RUN_LIVE_MODEL_TESTS`: unset skips (an ordinary `go test ./...` makes zero paid calls even with `.env` present — the behaviour change #30 demanded), set-but-misconfigured fails rather than skips. A `TierEnabled` answers the one caller a `*testing.T` skip cannot serve — `TestMain`, which starts Postgres before any test can skip.

**Grading is deterministic and code-based**, never an LLM judge — a judge's own drift is indistinguishable from the drift the suite exists to catch. Each prompt demands a per-trial random nonce, so an exact-match check tests the agent rather than the grader's generosity. Every trial runs a core pack (reaches idle with `stop_reason.type == end_turn`; no `session.error`; every `agent.tool_use` joined by exactly one `agent.tool_result`; token usage populated; the idle observed on the SSE stream), and each finding is classed **P**latform (our bug, a red run to fix), **M**odel (the model wandered), or **E**ither, so a red run says whose problem it is rather than "probably the model". Artifacts land in `evals/artifacts/`: a `report.json`, a `summary.md`, and one transcript per failed trial — including an aborted one, fetched best-effort in the deferred recorder because a drive timeout is the failure triage most needs. The report reduces the endpoint to host:port and scrubs known secrets from every rendered artifact, because a credential in `MODEL_BASE_URL`'s query would otherwise ride a transport error's quoted URL onto disk.

**The ten tasks** exercise the built-in toolset at two strengths — `edit`, `grep` and `bash`'s failing command by a result contract tying a call to its own output, `read`'s slice byte-exact, and `bash`, `read`, `glob` and `write` on a required tool-use floor (write's effect further pinned by its written artifact, glob invocation-only since a bare path list has no stable order to pin — since tightened by #98 and #99) — single and multi turn, allow and deny, seeded and unseeded, with three negatives. Tasks 1–3 (`fib-quickstart`, `echo-notool`, `shell-state`) landed with the harness; tasks 4–10 added three mechanisms. **Seed planting** writes files into the session's container before turn 1 by pre-provisioning it; the executor adopts that same container (by session label) when it runs the first tool, so the agent sees the seeds — used by `edit-config`, `needle-search`, `perm-deny`, `view-range`. **Gated toolsets** (`always_ask` via `default_config`) and a **confirmation-aware drive loop** power the permission pair: `perm-allow` and `perm-deny` drive the bridge end to end — a gated tool suspends the session on a `requires_action` idle, the loop posts a `user.tool_confirmation` (allow or deny) referencing the event id `requires_action` named, and grading pins the pause, the `evaluated_permission == "ask"` stamp, the result sequenced after the approval, and — on deny — the synthesized `is_error` result carrying the deny message with the seeded file left untouched. `exit-code` pins a failed command's `exit code:` trailer, correlated to the failing call's own result (the load-bearing check; the model's reported code is a weaker secondary signal, since cat of a missing file conventionally exits 1); `journal-multiturn` pins event replay and sandbox reuse across two turns (the executor adopts the session's container by construction); `view-range` pins `read`'s `view_range` slicing byte-for-byte as an off-by-one guard.

Prompts are written the way the docs tell a user to write them — a prompt tuned until only our platform's quirks satisfy it stops being a regression test. Two that a refusal-prone live model (MiniMax-M3) balked at were reworded to exercise the platform rather than trip a safety reflex: `perm-deny`'s "delete a file in a `protected` directory" became a benign append the reviewer declines, and `view-range`'s "SECRET" marker copied "to another file" — which the model read as exfiltration — became a plain marker copied to a file. All ten run 10/10 green live.

Deliberately deferred and filed as issues: a daily scheduled CI run ([#96](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/96), needs repo `MODEL_*` secrets), `tool_choice`/`disable_parallel_tool_use` for phase 1.5 (on #30), and production sandbox reaping ([#64](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/64)). The harness reaps its own containers at stack teardown.

---

## Brain turn-fault correlation (plan 09) — archived 2026-07-23

[docs/plan/09_brain-turn-fault-span.md](./plan/09_brain-turn-fault-span.md), delivered in one PR
(**#164**) for [#92](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/92). The change and
its reasoning are the CHANGELOG § [Unreleased] entry; recorded here is only what a changelog cannot
hold.

**The issue's own "cheap version" was evaluated and rejected as inert — it does not compile into a
fix.** #92 offered `telemetry.Extract(ctx, item.TraceContext)` in `RunOnce` as the low-cost option,
"the same shape the executor's fault log had before the log-bridge PR improved it, and better than
nothing". Against this repository's source it is worth exactly nothing: `internal/queue/queue.go`'s
`Enqueue` captures a trace context only for `kind == ToolExec` and deliberately writes SQL NULL for a
`model_turn` ("capturing it there would only persist an unread payload"), a decision ARCHITECTURE.md's
Observability section states in the same words. `item.TraceContext` is therefore always nil on the
brain's path, `Extract` returns the context unchanged, and the log would have stayed as uncorrelated
as before while looking fixed. The issue was filed by a reviewer reasoning from the executor's shape;
the `issue-triage` subagent independently reached the same refutation from the same source. This is
the case CLAUDE.md's "verify every finding against the source before acting on it" is written for,
in its rarer direction: the finding was real, one of its two proposed remedies was not.

**Extending trace-context capture to `model_turn` was considered and deliberately deferred.** It is
the other way to make a turn's failure findable — parent each turn on whatever enqueued it — and it
is a strictly larger decision: it reverses the queue-level choice above at three enqueue sites (the
API's `user.message` trigger, the executor's resume-enqueue, the brain's own chained requeue), and
those are three *different* traces with three different notions of "end to end". Nothing #92 asks for
requires picking one. Without it the `model_turn` span roots the turn's own trace exactly as
`model_request` already did, so the trace topology gains a level and changes no shape, and the
`tool_exec` items a turn enqueues keep parenting on `model_request` — the executor's and BYOC
worker's existing correlation is untouched, as `TestToolExecEnqueueCapturesTurnTrace` still asserts.

**Why the fault log could not simply move onto the existing `model_request` span — and why the
obvious version of that argument is wrong.** The tempting phrasing is "the faults land on both sides
of it: liveness lookup and replay before it opens, settlement after it closes". The second half is
false, and the Claude reviewer caught it in six places before merge: `settleTurn` calls `commitTurn`
**first** and `span.Finish(ctx, false, err)` after, an ordering `internal/events/span.go` states
deliberately ("Finish runs after the settlement transaction so it can record whether the end event
actually committed"), so settlement runs *inside* the `model_request` span and a failed commit does
redden it. The true argument has two halves: (1) `claimLiveSession`, the reclaim-recovery append,
replay, request assembly and provider resolution fail before that span exists at all — they reach
`failTurn` with a nil span; (2) for every fault that does occur inside it, `runTurn` returns an error
and nothing else, so `sctx` never leaves the function and the span is already closed when `RunOnce`
sees the failure. A `SpanKindConsumer` span standing for the handling of one claimed item end to end
is the only frame that holds for both.

**Test method — the span id is the assertion, not the trace id.** A parent and its child share a
trace id, so a trace-id assertion cannot tell "logged on the turn's own span" from "logged on some
span in the turn's trace", and would have passed against a design that hung the record anywhere in
the tree. `TestTurnFaultLogLandsOnTheModelTurnSpan` asserts the span id, mirroring
`internal/executor`'s `TestFaultLogLandsOnTheToolExecSpan`, and was run against the unfixed brain
first: it failed on the absent `model_turn` span, so it cannot be passing for an unrelated reason.

**Review-hardening record.** Codex (`gpt-5.6-sol`, `ultra`) and the Claude-side reviewer both found
the code correct under adversarial reading — the named-return/defer pairing, the `err != nil &&
!found` predicate, span lifetime and nil-safety, the platform-fault-only reddening rule, and
cross-process trace parenting all survived — and both spent their findings on the prose instead.
Four corrections landed pre-merge: the inverted settlement/`Finish` ordering above (six places); the
claim that the old log carried "no session id", when `RunOnce`'s `fmt.Errorf("session %s: %w", …)`
put it in the message text (narrowed to "no trace, no span"); "the pull protocol's third deployment
point", which collided with the term's established `cloud`/`self_hosted` meaning in CLAUDE.md,
README and this file (now "the work queue's third claimant"); and an "always reachable from the red
span" claim about all three claimants that the BYOC worker's heartbeat path does not honour — its
lease-loss warnings log outside the run's span, so ARCHITECTURE now names that exception rather than
overclaiming. Two tests were added in response: the unset-status negative the verifier found
unpinned, and a shutdown test for `Run`'s surviving log. Every new assertion was mutation-checked —
the fault-log test against a `context.Background()` log and a removed `SetStatus`, the parenting test
against a discarded span context, the unset-status test against a `failTurn` that returns an error,
the shutdown test against the removed cancellation guard — each turning red on the mutation it
targets.

**Governance — the plan was authored after the fix, and says so.** `issue-triage` returned
`needs_plan: true`; the implementation was already written test-first from the issue by then, so this
plan is a decision record rather than a forward design. It lands `archived` in its one delivering PR
on the plan-07 precedent, and STATE.md is untouched — the work starts and finishes in the same PR, so
there is never an in-flight state for it to track.

---

## Reject unknown agent_toolset fields (plan 07) — archived 2026-07-22

[docs/plan/07_reject-unknown-toolset-fields.md](./plan/07_reject-unknown-toolset-fields.md),
delivered in one PR (**#151**) for [#26](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/26).
The change and its reasoning are the CHANGELOG § [Unreleased] entry; recorded here is only what a
changelog cannot hold.

**Why this was a plan, not a straight fix.** The trigger was wire-schema verification: the accepted
keys of every `agent_toolset_20260401` object had to be pinned from `anthropic-sdk-go` v1.58.0's
request (`*Params`) types before the strict check could be written, not improvised from the bug
report. The plan's field table is that pinned artifact; the verifier and both reviewers re-derived it
field-for-field against the SDK tag and confirmed no divergence — `DIVERGENCES.md` INFERRED #59
(the `always_allow` default for a genuinely omitted policy) is untouched, because the fix only stops a
*misspelling* from being read as an omission.

**The eager/lazy split was the load-bearing design decision.** Unknown-*key* rejection is eager —
it fires regardless of a tool's enable state, because a typo'd `permission_policy` on a disabled tool
is a latent fail-open that activates the moment the tool is enabled. That is deliberately distinct
from the pre-existing *lazy* validation of a policy's *value* (checked only for live tools), and the
two coexist without conflict because they inspect different things: `TestPoliciesValidatesLazily`
uses correctly-spelled keys with bogus values and stays green.

**Governance — a parallel drive-by fix leaves STATE.md alone.** #26 landed while the skills plan
(#54) legitimately owned STATE.md's single-focus tracker. Rather than evict in-flight tracking or
dual-track, this plan was authored and archived within its one delivering PR; the narrative lives in
CHANGELOG, so STATE.md never needed to name #26. Both the verifier and the Claude reviewer confirmed
this is only defensible *because* the plan lands `archived` (not left `in-progress` with nothing left
to do).

---

## Work Stop 204 (plan 04) — archived 2026-07-20

[docs/plan/04_work-stop-204.md](./plan/04_work-stop-204.md), delivered in one PR (**#122**). The
change and the reasoning behind it are the CHANGELOG § [Unreleased] entry; recorded here is only
what a changelog cannot hold.

**The generalizable lesson: "confirmed" did not mean re-derivable.** The registry's entry was not a
guess — it was CONFIRMED, and the measurement behind it reproduces to this day. What it lacked was a
check that the thing measured was the thing claimed. Two rules fall out, both wider than this
endpoint: a client-side workaround shipped by the reference SDK is evidence *for* the behavior it
works around, never against it; and a CONFIRMED entry earns re-derivation whenever the change under
review depends on it, because the artifact that confirmed it may never have supported it.

**Evaluated and rejected — keeping 200 + JSON as a deliberate leniency divergence.** Adversarial
review made the case seriously, and it was measured rather than asserted: 200 + JSON satisfies a
strict superset of clients (the generated method decodes it; the bypassing helper and the `ant` CLI
tolerate either shape). Rejected because "superset of clients" is the wrong objective for a
compatibility layer — the extra consumer it buys is one already broken against Anthropic's own
service, so the leniency bought compatibility with this platform rather than with the reference. Two
reviewers reached this position independently and both were overruled on that ground; recorded here
because the argument is a good one and will recur the next time a divergence looks harmlessly
permissive.

**What the review round corrected — including an error of ours.** CLAUDE.md ranks *public docs*
above the reference checkouts, and the plan asserted the typed schema ranked first. That was simply
wrong, and it mattered: the top-ranked source disagrees with the change. The conclusion survived
(the spec-side witnesses are one witness, not three), but the framing had to change from "not a
divergence" to a deliberate divergence from the published spec, left open for a recording (#78) to
close, with the compatibility break stated rather than glossed. Separately, the plan's aside that
the empty-poll response might follow Stop to 204 had its evidence backwards — the poller calls
`Work.Poll` with no bypass and its empty-queue branch needs a decoded body — so `200` + `null`
stands. Both corrections came from reviewers reading the primary sources rather than the plan.

**Why the mutation check was the load-bearing evidence.** Asserting the resulting work state cannot
catch a missing decoder bypass: the stop succeeds server-side either way, and the only damage is a
fictional `worker: force-stop failed` on every clean finish — invisible to a suite that checks the
database. Only removing the bypass and watching `TestWorkerForceStopAcceptsNoContent` fail with the
SDK's quoted error string proves the test constrains anything. A green suite was not evidence here;
a red one was.

---

## Docs restructure (plan 03) — archived 2026-07-18

[docs/plan/03_docs-restructure.md](./plan/03_docs-restructure.md), delivered complete in
three PRs (narratives in CHANGELOG § [Unreleased]): **#101** — docs/ARCHITECTURE.md
created as the as-built reference and HISTORY.md slimmed 530 → 217 lines under the
one-writer rule (verifier ×2, Codex 17 findings, /code-review 5 — all resolved against
source); **#102** — STATE.md reduced to a pure active-work tracker (63 → 23 lines), the
verifier's STATE checks rewritten; **the issue-triage PR** — `.claude/agents/issue-triage.md`
(Sonnet 5, read-only, strict-JSON `needs_plan` judgment) with its CLAUDE.md trigger rule,
archiving this plan.

---

## K8s silent short write (#103/#86) — investigation record (2026-07-19)

Narrative and the defect itself: CHANGELOG.md § [Unreleased]. Recorded here is only what a
changelog cannot hold — a refuted mechanism, the alternatives rejected, and the verification record.

**A confidently-argued mechanism that was wrong.** The first account blamed client-go teardown:
`StreamWithContext` returning before `cat` exits, letting `defer conn.Close()` cut stdin. Refuted at
source — `v4.go`'s `wg.Wait()` then `return <-errorChan`, gated by `io.ReadAll(errorStream)`, cannot
return before the remote process terminates; and `WriteFile` passes a non-nil stderr, so `cat`
holds fd 2 regardless of the fd 1 redirection. Recorded because CLAUDE.md warns that reviewers and
investigators produce exactly this kind of plausible, well-cited, wrong finding.

**Evaluated and rejected.**
- *Swapping `NewSPDYExecutor` for the WebSocket or fallback executor.* Measured, not assumed: with
  the old `exec cat` script the WebSocket executor lost the same 1 MiB payload 14/15 times (SPDY
  15/15). The loss is transport-independent, so the swap fixes nothing and would change every exec
  in the backend.
- *Dropping `exec` alone.* Empirically sufficient, but it rests on an inferred mechanism nobody
  instrumented. The length check makes the guarantee hold whatever the layer.
- *Verifying by re-reading the target (`wc -c < "$1"`).* Landed first and caught in review by both
  reviewers independently: re-reading asks what the path holds now, not what the stream delivered,
  so it fails a **successful** write to `/dev/null` or any device node (reproduced: old exit 14, new
  exit 0), to a file the sandbox user may write but not read, or to a path another process in the
  sandbox is touching — and it is TOCTOU besides. Replaced by counting the stream itself
  (`tee "$1" | wc -c`), which measures exactly the quantity that went missing.
- *A `sandbox.ErrShortWrite` sentinel.* No caller distinguishes it, and `sandbox.go` already commits
  the package to all-or-nothing semantics; a sentinel would legitimize the outcome it rejects.
- *Mirroring the check on the read side.* `ReadFile` has the same structural hazard (exit 0 + short
  stdout reads as success) and `readScript` already computes `sz`, but the evidence rules it out as
  the cause here and a naive fix false-positives on a file rewritten between the `stat` and the
  `cat`. Filed as #105 rather than widening this diff — closed there by marking the end of the
  stream rather than counting it (record below).

Also left out deliberately: `cat > "$1"` truncates the target before the first byte arrives, so a
detected short write still leaves a truncated file on disk. Making the write all-or-nothing is
already tracked by #71 (atomic `WriteFile`), and doing it here would widen this diff into that one.

**Local verification blocker.** The K8s contract suite could not be run to green on the development
machine: it fails with transport `EOF`s on unmodified `main` too (17/21/8 subtests), so it is
environmental. Node restart, cluster recreate, image sideload, a Docker Desktop restart and a
connection cooldown all failed to restore it; `kubectl exec` stays ~93-100% while the Go process
breaks under provision churn, and it is not FD limits (`ulimit -n` 1048576) or TIME_WAIT exhaustion.
Targeted evidence stands on its own (the two new tests, the #103 subtest, and the docker backend on
the same shared subtest all pass); CI's fresh kind cluster on Linux is the gate.

---

## K8s read-side short-read guard (#105) — design record (2026-07-19)

The hazard and the guard: CHANGELOG.md § [Unreleased]. Recorded here is only what a changelog
cannot hold — what was measured and rejected, and the sense in which nothing was reproduced.

**Nothing was observed.** Roughly 35 reads of 1 MiB, 4 MiB and 20 MiB files through the pod-exec
stream produced zero silent short reads. Three runs failed with a transport `error: EOF` delivering
**zero** bytes and a non-zero exit — the same environmental flakiness the #103 record above
describes, which `client.exec` already surfaces as `err != nil` rather than a `streamResult`, so
`ReadFile` already errored on those. The guard closes a structural hazard, not a reproduced failure.

**Evaluated and rejected.**

- *Comparing the bytes received against `readScript`'s existing `sz`.* The fix the issue asked for a
  decision on, and worse than the issue argued. Beyond the file rewritten between the `stat` and the
  `cat`, it breaks every procfs read: measured in-pod, `/proc/self/status` reports a `stat` size of 0
  while `cat` streams ~1.1 KB, so an ordinary `/proc/meminfo` would come back as a short read. It is
  also a re-read of the target, the mistake #103's review rejected.
- *Counting the stream on stderr (`cat "$f" | tee /dev/fd/3 | wc -c` under `pipefail`, the literal
  mirror of `writeScript`).* Byte-exact in every probe — 0 B to 20 MiB, as root and under
  `runAsUser: 65534` — and rejected on portability, not correctness. `/dev/fd/3` is a reopen rather
  than a dup: against a socket descriptor it fails with `ENXIO`, and under an in-pod uid transition
  (`su`, `setpriv`) with `EPERM` (measured), so every read in the backend would depend on what kind
  of stdio a container runtime hands an exec — an invisible dependency in the backend whose whole
  purpose is customer clusters nobody here has seen. Its one advantage over a marker, catching a hole
  in the middle of the stream, has no reachable input: client-go copies stdout with a single
  `io.Copy` (`streamProtocolV2.copyStdout`), which stops at its first error, so every loss is a
  suffix truncation. It also adds a second stream whose independent damage becomes a false positive
  on an otherwise good read.
- *A fixed marker constant instead of a per-call `nonce()`.* Cheaper, and it puts the literal in the
  repo — in `k8s.go` and in this file — so files a sandboxed agent routinely reads would contain it,
  turning a negligible false negative into one with instances on disk. `nonce()` already existed, is
  `crypto/rand`, and rides in argv.
- *A `sandbox.ErrShortRead` sentinel.* Same answer as `ErrShortWrite` above. A plain error falls
  through `toolset`'s default arm to the executor as a backend fault, which is where a retriable
  transport failure belongs — the model must not be handed a truncated file to route around, least
  of all through `edit`, which writes back what it read.

**Stated as inference, not instrumentation.** That a marker suffices rests on reading client-go's
stdout copy and concluding no mid-stream hole can reach the buffer. Nobody induced one; there is no
way to. This file already records that a confidently-argued, well-cited claim about this exact
transport was wrong once.

**Verification record.** Each half of the guard was proven able to fail, in throwaway copies rather
than by assertion: reverting `readScript` to `exec cat "$f"` turns `TestReadScriptMarksWhatItSent`
red; reverting the read buffer's room to `MaxFileBytes + 1` turns the live `ReadFileAtTheCap` red;
and flipping one byte while keeping the length turns it red at that subtest's content comparison
specifically. The procfs case that ruled out the `sz` comparison was measured in a real pod, not
argued: `/proc/meminfo` stats as 0 bytes and streams 1392. The gate split as the blocker above
predicts — locally `make verify` is red only in `internal/sandbox/k8s`, reproduced on unmodified
`main` in a fresh worktree, while CI's kind cluster ran that package to `ok` in 51.0s with the full
contract suite. Coverage 91.38-91.48% across runs.

## `internal/events/toolflow.go` characterization suite — verification record (2026-07-19)

Narrative: CHANGELOG.md § [Unreleased] → Added. Recorded here is only the verification, plus one
claim this project made and then had to retract.

**A claim of ours that was wrong.** The first version of the changelog entry said the two
`if extraRefs == nil { extraRefs = []string{} }` normalizations were "previously unguarded by any
test". Both reviewers challenged it and the measurement refuted it. With the new file absent, removing the
`extraRefs` line turns `./internal/brain/` red (`TestParallelToolCallsResumeOnFullSet`) *and*
`./internal/api/` red (five confirmation tests); removing the `extraConfirmed` line leaves
`./internal/brain/` green and turns `./internal/api/` red
(`TestConfirmationUserMessageDoesNotBypassGate`) — `brain_test.go:155` calls `HasUnansweredToolUse`
directly, and `internal/api/confirmation_test.go` covers the `UnconfirmedAskEvents` side. The trap is real and the
new tests name it directly; the novelty claim was not. Recorded because the wrong claim was the
entry's headline, and it survived self-review.

**Verification record.** Each case was proven able to fail by mutating `toolflow.go` in place,
running the suite, and reverting — never by assertion. Twenty-two single-edit breakages in total.
Caught on the first pass: dropped `r.session_id` / `c.session_id` predicates, `ORDER BY tu.seq` →
`tu.id`, both nil normalizations removed, `seen` re-keyed by kind+id, `wantUse` pinned to
`agent.tool_use`, the ask gate deleted, the platform variant widened to every tool-use kind or made
to ignore `extraRefs`, `confirmableToolUseTypes` widened in either query, the `Scan` error check
dropped, and the COALESCE reduced to a single key.

Seven survived the first suite and each is now closed by a named subtest, with the same mutation
re-run to prove the closure: the second and third `COALESCE` arms trading places in
`hasUnansweredToolUse` and in `ValidateToolResults`, and — caught only by the re-verification pass,
after the first fix drove the (1,3) and (2,3) pairs but not (1,2) — the first and second arms trading
places in `hasUnansweredToolUse` (every existing leg carried one key, or compared
only the first arm against a later one); the `c.type` predicate in `ValidateToolResults`' ask gate
and in `ValidateToolConfirmations`' already-confirmed check (no fixture had ever put a second event
carrying the gated id beside the confirmation); `ValidateToolResults` swallowing its payload-decode
error; and the ask gate restricted to built-ins. For the last of these the closure was double-checked
in both directions — with the new subtest removed the mutation goes green again, with it present that
subtest is the one that fails.

**Two mutations that cannot be caught, and were not chased.** Dropping `c.type = $4` outright leaves
`$4` unbound, and pgx rejects the query at run time (`expected 3 arguments, got 4`) — the suite fails
loudly on it, so it is a broken query rather than a silent regression.
Widening that same predicate to admit `user.tool_result` is unobservable through the ask gate: a
`user.tool_result` carrying the id makes the tool use *answered*, and the answered check fires first,
so both implementations return the same error. The observable form of the widening — admitting a type
that carries the id without answering it — is what the new fixtures use.

**Method note.** Two intermediate results in this audit were false positives from a careless harness
and are recorded so the next audit avoids them: a `grep FAIL` verdict counted build failures and SQL
syntax errors as "caught", and a mutation pattern indented with two tabs matched *both* confirmation
subqueries as a suffix of the three-tab one, so an occurrence-0 edit silently hit
`ValidateToolResults` twice while `ValidateToolConfirmations` was never mutated at all. Every verdict
above is from the corrected harness, which vets the build and distinguishes the two sites explicitly.

## Provider credential redaction (#83) — review-hardening record (2026-07-20)

What [CHANGELOG.md](../CHANGELOG.md)'s entry describes as one coherent redaction arrived through
three review rounds, each of which found the previous round's fix incomplete in the same way. The
record is kept because the *pattern* is reusable and the CHANGELOG cannot hold it: every gap was a
rendering of the credential nobody had thought to enumerate, and two of them were hidden by test
fixtures chosen for readability.

**Round 1 — verifier, after the first commit.** Confirmed the five leak paths closed and every test
failing-first, then found three residuals. An unparsable `base_url` leaked its password because
`NewRedactor`'s own `url.Parse` failed, so the site's comment claimed a coverage it did not have and
both of its blocks were unexecuted by any test. `isAuthHeader` missed `apikey` — no separator, so
none of the substring rules matched — which is Kong's key-auth default and Supabase's convention.
And the 4 KiB quote cap could sever a credential mid-token: demonstrated leaving 8 characters of the
key in the message, matching no registered secret.

**Round 2 — Claude review panel, six lenses with three adversarial refuters per finding.** Seventeen
findings were refuted under verification; three survived, all the same defect found independently by
three lenses, with 3/3 refuters failing to refute it and an end-to-end reproduction attached:
`url.Parse` stores a `base_url` password decoded while `url.URL.String()` re-encodes it, so
registering the decoded form matched nothing for any password containing a character RFC 3986
requires be escaped in userinfo. **The regression test passed only because its fixture,
`pw-secret-456`, was URL-safe — the single class of password for which the two renderings
coincide.** Re-verification then found a fourth rendering by the same reasoning: `net/http` derives
an `Authorization: Basic` header from userinfo whenever the request carries none, which is always
under the anthropic protocol, so an auth-echoing endpoint quotes the credential base64-encoded.

**Round 3 — external reviewer (Codex `gpt-5.6-sol`), reading only the first commit.** Independently
reproduced the same conclusions and added four. Three were leaks: custom auth header names
(`X-Auth`, `X-Signature`, `X-Credential`); userinfo carrying a username and no password, the
token-as-userinfo convention, where the username *is* the credential rather than an identifier
standing beside one; and `resp.Status`, which HTTP/1 lets a server fill with arbitrary text, being
interpolated unredacted beside the body that was not. The fourth was an over-redaction the fix had
itself introduced: splitting a header value on any space registered the second word of a value that
is not a credential pair, so `x-route-key: "pool alpha"` blanked "alpha" out of every diagnostic
naming the pool — the opposite of the carve-out's purpose.

**Decisions evaluated and rejected.** Shape-matching `Bearer`/`Authorization`-looking tokens, which
the issue floated, was rejected on evidence: the observed anthropic echo was a bare value with no
scheme prefix and no header name beside it, so a shape matcher would have missed the very leak the
issue was filed for, and `base_url` may point at any gateway whose token format is unknowable.
Chasing a credential re-encoded by Go's HTML-escaping JSON encoder was rejected as the same
speculative pattern-matching, and buys nothing against an endpoint that transforms deliberately.
Redacting a model's *successful* output was rejected as a category error — model output is a trusted
boundary, and scrubbing it would corrupt the content the session exists to record. Redacting a
`base_url` **username** when a password stands beside it was rejected because a username identifies
rather than authenticates, and masking it costs a diagnostic to hide nothing.

**Method note, worth repeating.** Two tests in this change passed against the unfixed code before
being corrected: the base_url fixture above, and a truncation test whose padding arithmetic left
only 3 characters of the key inside the quote budget while its assertion checked runs of 5 or more.
Both looked like coverage. The habit that caught them — and that the next change should copy — is to
overlay new test files onto a `git archive` of the *previous* commit and confirm each fails **for
the intended reason**, not on a nil error, a panic, or a build failure.

---

## Eval grader rigor (#99) — review-hardening record (2026-07-20)

[CHANGELOG.md](../CHANGELOG.md)'s entry says what changed. Kept here is what a changelog
structurally cannot hold: the acceptance evidence, and the two occasions on which a
confidently-argued claim of mine was refuted by someone checking it.

**The meta-defect three reviewers converged on.** Codex, `/code-review` and the verifier
worked independently and every confirmed finding had the same shape: **a grader whose own
behavior no mutation of itself could catch.** Deleting `ConfirmedResult`'s
dangling-confirmation join, or reverting `EvaluatedPermissionAsk` to first-call-only, left
the whole unit suite green. A grader that cannot fail when broken is not a test, and the
suite had several. Mutation testing became the acceptance bar for the rest of the work: a
behavior counts as pinned only when breaking it reds a test that *names* that behavior.

**Mutation matrix at the final state** — 17 probes, 16 killed (15 by the unit tier, 1 by a
live run), each run in a throwaway `tar`/`git archive` copy, never the checkout. Killed:
`ConfirmedResult`'s dangling-name join, its empty-`tool_use_id` reject and its content
check; `CallResult`'s terminal missing-`is_error` and its resultless-sibling skip;
`GlobPathList`'s empty-success reject, missing-`is_error` reject and absolute-path check;
`NotInToolTraffic`'s encoded scan, decoded scan and tool-result scan; `OnlyIf`'s
every-premise semantics; `fill`'s `{{RECALL}}` substitution; `EvaluatedPermissionAsk`'s
every-call sweep; `toolCallsWith`'s marker filling and its decoded-input matching.

**The one survivor, and why it is not a gap.** `FileLines` filling tokens into its *path*
survives everything. The reason is not a missing test: every `FileLines` call site passes a
token-free literal path, and the one tokened path is graded by `FileEquals` — so `fill` is
the identity there and the mutant is **equivalent** on the current task set, unkillable by
any run. The code stays as correct defensive symmetry with `Seed` and `FileEquals`. The
claim that *is* provable was proved by running it: mutating `FileEquals`'s path fill reds
`journal-multiturn` live with `file-equals:/tmp/provenance-{{NONCE}}.txt: sandbox: no such
file`. Recording an equivalent mutant as an equivalent mutant, rather than as a killed one,
is the point of the entry.

**Live acceptance: five runs, and the pattern beats the score.** Across five `make eval`
runs against a real endpoint (MiniMax-M3) at successive revisions — 10/10, 9/10, 10/10,
then after merging `main` 9/10, 10/10 — **no Platform-class grader fired even once.** The
two reds were `journal-multiturn`'s `file-lines` (Either: the model wrote turn 1 without a
trailing newline, so the appended line concatenated) and `perm-deny`'s `tool-called-with`
(Model, with zero tool calls: the model never ran the instructed command). That pattern,
not any single 10/10, is the evidence the classing works — a live model wandering produces
Model and Either reds, never a Platform red. Reporting the two 9/10 runs rather than
re-rolling to a clean sweep is part of the record.

**Two defects the live suite found that no reviewer did.** Substitution was split across
two helpers, so a grader searched for a literal `{{RECALL}}` while the model had said the
code back correctly — a token live on one side of a check and literal on the other is not a
bug a unit test written against the same misunderstanding will catch, so the two spellings
were merged into `(*Trial).fill` rather than documented. And the recall prompt called the
token a "code word" and forbade writing it anywhere, at which the model **refused** turn
two, reading the pair as a secret and the request to repeat it as an attempt to extract it
— the same trap `view-range` already avoids by not calling its marker a SECRET. A prompt
that sounds like a confidentiality rule tests the model's refusal reflex, not the platform.

**Two of my own claims were wrong, and neither was caught by me.** The verifier refuted a
mutation-verification claim I had made about `ConfirmedResult`'s dangling join: my probe
predated a structural check I later added, so by the time I reported it the mutation
survived for a different reason than the one I had tested. It also refuted my statement
that the `FileLines` survivor was "pinned by the live suite" — it is an equivalent mutant,
as above. Separately, a finding *I* raised in self-review — that `OnlyIf`'s premise and
`BashCommandWith` could disagree and open a window where neither grader fires — was refuted
against the source: `BashCommandWith`'s matched set is a **subset** of the premise's, so it
passing implies the premise holds. The direction of my concern was backwards. The habit
worth copying is the one CLAUDE.md already states and this change kept proving: verify
every finding, including your own, against the source before acting on it.

**A reviewer disagreement resolved by the user, not by argument.** Codex asked for
`ConfirmedResult` to grade *every* confirmed call; `/code-review` demonstrated that exactly
that strictness is a false-red generator, since the toolset gates every tool and a model
that writes a file then verifies it with a second marker-carrying command earns a second
confirmed result — a verification exiting non-zero would fail the trial with the platform
behaving perfectly. The two reviewers wanted opposite things, which is a decision and not a
defect, so it went to the user, who chose the simplification. `ConfirmedResult` now grades a
confirmed call the way `CallResult` grades a called one: one satisfying call is enough,
with the dangling-name join #99 asked for kept and run *before* the markers narrow anything.

**The vacuity argument needed a task-by-task check, not a general one.** `ConfirmedResult`
goes vacuous when no *matching* call was confirmed, and its comment justified that by
naming `EvaluatedPermissionAsk` as the sibling owning the window. The verifier found that
`perm-deny` — the only task where `ConfirmedResult` is Platform-class — did not deploy that
grader at all, and that the stamp it checks is not proof a suspension happened anyway. The
fix was both halves: `perm-deny` gained `EvaluatedPermissionAsk("bash", Platform)`, and the
comment now names each task's real owner of the window (in `perm-deny`, the seeded file
staying unchanged). A safety argument that holds "in general" but not in the one place the
grader is Platform-class is not a safety argument.
---

## anthropic-sdk-go v1.58.0 bump (#120) — wire-schema verification record (2026-07-20)

The bump itself is two lines of `go.mod`/`go.sum`. It is recorded here because CLAUDE.md makes the
pinned SDK this project's **authoritative typed wire schema**, so a minor-version bump is a
wire-schema event whose *outcome* has to be auditable even when — as here — the outcome is that
no field or enum drifted and no code change became necessary. Without this record the next bump has
to redo the same diff to learn that. (Stated that precisely rather than as "contract-neutral": one
documented bound *did* widen — see the custom tool `Description` below — it simply lands where this
repo enforces nothing.)

**What the range contains.** Two upstream releases: v1.57.0 (a "dreaming" API and tool-runner
permission gating) and v1.58.0 (MCP Tunnels). Endpoint count went 116 → 131 (`.stats.yml:1`).

**The decisive measurement.** Every SDK file carrying managed-agents schema this repo mirrors is
**byte-identical** across the two versions (`cmp`): `betasessionevent.go`, `betasession.go`,
`betasessionthread.go`, and — because a session's shape reaches past those three —
`betasessionresource.go` (the `Resources` union this repo emits at `internal/api/sessions.go:35`),
`betasessionthreadevent.go`, `betaagentversion.go`, `betaenvironment.go`, `betaenvironmentwork.go`.
The event taxonomy, the ID prefixes, and every session field this repo mirrors are therefore
unchanged by construction, not by inspection. The first three alone would *not* have been sufficient
proof, which is why the list is enumerated here.

**The three questions #120 asked, answered.**

- *Do the changed `betaagent.go` / `betamessage.go` / `betasession*.go` types alter a shape
  `internal/domain` or `internal/api` mirrors?* **No.** `betaagent.go:1288`, `betamessage.go` and
  `betamessagebatch.go` changed in **doc comments only** — no field, type, or enum moved (proven by
  diffing the three with comment and blank lines stripped: empty). Two of those comments did shift
  meaning, and neither reaches this repo. A custom tool's `Description` bound relaxed from 1–1024 to
  1–4096 characters (`betaagent.go:1288`), which costs nothing because `internal/api/wire.go:244`
  only requires the description be non-empty and never enforced a length bound to relax; and
  `betamessage.go:4850-4853` re-words model fallbacks ("the **four** override fields … **replace**
  the corresponding top-level field" → "The override fields … **set** the corresponding parameter"),
  alongside a fuller `speed` description — both on model-fallback and speed surfaces this repo does
  not implement.
- *Does `shared/constant/constants.go` add stop reasons or event types the taxonomy should carry?*
  **No.** It adds exactly three constants — `Tunnel`, `TunnelCertificate`, `TunnelToken`
  (`constants.go:206-208`) — belonging to the new tunnels product, not to the `{domain}.{action}`
  session taxonomy. No new stop reason. The remainder of that file's diff is gofmt realignment.
- *Do the new `betatunnel*` / `betadream` surfaces imply behavior worth a DIVERGENCES entry?*
  **No, and this was decided rather than skipped** — see below.

**Decisions evaluated and rejected.**

*A docs/DIVERGENCES.md entry for the tunnels/dreams surfaces* was rejected. The registry records
deliberate divergences from reference behavior and inferences about behavior not yet confirmed;
`/v1/tunnels` and `/v1/dreams` are neither. They are reference **product areas this repo has not
built**, and logging those would grow the registry into a mirror of everything upstream ships — work
the GitHub backlog already owns. A divergence needs two implementations that disagree; here there is
only one.

*Adopting the v1.57.0 tool-runner permission gating* was rejected as a non-change, and the reason is
worth keeping: that rewrite gates dispatch on `evaluated_permission`, holds `ask` calls for
`user.tool_confirmation`, and fails closed on an unrecognized permission — which is the SDK's
**client-side helper** catching up to behavior this repo already implements **server-side**
(`internal/brain/brain.go:391` stamps the field, `internal/events/toolflow.go:199-260` gates results
and validates confirmations, `internal/api/events.go:83,124,226` drives the `requires_action`
round-trip). The enums agree exactly: SDK `betasessionevent.go:1013-1015` (`allow`/`ask`/`deny`)
versus `internal/domain/agent.go:49-51`. The SDK's fail-closed rule is a client concern; this repo is
the authority *emitting* the field and only ever emits `allow` or `ask`. Read as convergence
evidence, not as a gap.

Likewise the `betasessionutil.go:45,66-70` accumulator fix — keep canonical `agent.message` events
(valid `processed_at`), drop only unreconciled previews — relies on precisely the distinction this
repo already maintains (`internal/api/events.go:583-585` emits `processed_at` null-until-processed;
the brain mints a preview-reserved message id).

**Citation durability.** The bump moved the live pinned-version label in three places —
`.claude/agents/verifier.md`, `docs/REFERENCE_PROJECTS.md`, and the Stop Work entry in
docs/DIVERGENCES.md — and every file:line that entry cites was re-read at v1.58.0 rather than
assumed: `lib/environments/poller.go:439-465` (the 204 / `WithResponseBodyInto` comment) and
`worker_test.go:118-120` (`WriteHeader(http.StatusNoContent)`) hold verbatim — `diff -rq` shows the
whole `lib/` tree identical between versions. `api.md` **did** change, so `api.md:656-673` was
checked rather than trusted: the work-resource section did not shift, and those lines are
byte-identical across both versions, still declaring the `BetaSelfHostedWork` return on Stop. The
v1.56.0 mentions surviving in CHANGELOG.md and archived `docs/plan/04` are historical records of
what was true when those PRs landed and were deliberately left alone.

**Evidence.** `make verify` green at total statement coverage **91.92%** (including the Docker and
K8s sandbox suites). Every SDK request type, response field, JSON tag, service method, option,
paginator, SSE helper and error decoder this repo uses is unchanged: the defining file of each is
among the byte-identical set, enumerated from the non-test import sites
(`internal/provider/anthropic/anthropic.go`, `internal/worker/{client,lease,toolexec}.go`;
`internal/worker/{lease,toolexec}_test.go` import the SDK too, and the compile-and-test pass covers
what they reference). One shape *did* change and is called out rather than smoothed over:
`sdk.Client`'s embedded `BetaService` gained `Dreams` and `Tunnels` fields (`beta.go:20`), so the
struct layout differs even though no call site's behavior does. "No code change required" is exact;
"zero runtime difference" would not be — the two new services are constructed at client init, and
the version-identifying `User-Agent` / `X-Stainless-Package-Version` headers change value.

**Process note — a rejected decision that was itself wrong, and got reversed in review.**
`issue-triage` returned `needs_plan: true` on the wire-schema-verification trigger. The
implementation initially declined to author a plan, reasoning that a plan is a forward-looking
decomposition across PRs while this is a single PR whose entire deliverable *is* the verification
outcome, so a plan created and archived in one commit would record that outcome in a file whose
status says the work had not started.

That reasoning was wrong, and is kept here because the failure mode is worth recognizing next time.
`.claude/agents/issue-triage.md`'s judgment criteria say the wire-schema trigger fires
"**unconditionally**, however well-scoped the issue already looks: the resolution itself belongs in a
plan, never improvised mid-implementation" — a rule written precisely to defeat the "but this case is
small" argument, which is the argument that was made. The lifecycle objection was also false: a plan
may land directly as `in-progress` (CLAUDE.md's plan bullet says so), which is what
[05_sdk-bump-1.58.0.md](./plan/05_sdk-bump-1.58.0.md) does.

Worth noting for anyone weighing reviewer disagreement by count: two of the three review passes
(the verifier and the Claude-side reviewer) examined this decision and endorsed it as defensible.
Only the Codex pass called it a blocking finding, and the Codex pass was right — it was the one that
quoted the governing sentence instead of reasoning from the rule's purpose.

---

## anthropic-sdk-go v1.59.0 bump — wire-schema verification record (2026-07-23)

The counterpart to the v1.58.0 record above, and its opposite result. That bump was
contract-neutral; this one is not. Two shapes this repo mirrors moved, both required code, and a
third — a constant whose Go identifier did not change but whose *literal* did — is the kind of drift
that no compiler and no existing test would have caught.

**What the range contains.** Two upstream releases: v1.58.1 (citation `ToParam` copy fixes, a Vertex
auth-scope default, and a new `general_harms` refusal category) and v1.59.0, whose release note names
managed-agents surface directly — "Managed Agents model effort, initial session events, and threads
delta streaming". Endpoint count is unchanged at **131** (`.stats.yml:1`); the OpenAPI spec hash
moved, so the change is to existing endpoints' schemas, not to the route table.

**The enumeration.** Every SDK file defining a shape this repo mirrors, `git diff`ed pairwise between
the tags rather than sampled. Unchanged: `betaagentversion.go`, `betasessionresource.go`,
`betasessionthread.go`, `betasessiontoolrunner.go`, `betaskill.go`, `betaskillversion.go`,
`betafile.go`. Changed: `betaagent.go`, `betaenvironment.go`, `betaenvironmentwork.go`,
`betamessage.go`, `betasession.go`, `betasessionevent.go`, `betasessionthreadevent.go`,
`shared/constant/constants.go`. Listing only the first group would have proven nothing, which is why
each member of the second is resolved below.

**Question 1 — does a changed type alter a shape `internal/domain` or `internal/api` mirrors?**
**Yes, three times; two of them are code, one is not.**

- *`betaenvironmentwork.go`* — one field added, `BetaSelfHostedWork.Secret`
  (`betaenvironmentwork.go:305`), `api:"required"`, "Credential payload used by the environment
  worker to execute this work item. May be populated when polling for work; null on all other
  retrieval paths." `internal/api/workapi.go`'s `workWire` documents itself as
  "the BetaSelfHostedWork response shape, **field for field**", so the bump made that comment false.
  **Mirrored** (`workWire.Secret`, always null — populating it needs the vault seam, a column only in
  v1), with the poll and get wire-shape tests extended to require the field and assert null.
- *`betaagent.go`* — two changes. `BetaAgentUpdateParams.Version` went `int64` (required) →
  `param.Opt[int64]` (`betaagent.go:2870`): "Must be at least 1 if specified. When supplied, the
  request fails if it does not match the server's current version; **omit to apply the update
  unconditionally**." `updateAgent` answered 400 "version is required" — after the pin moved, that is
  simply wrong against the authoritative schema, so it is **code**: version is now optional, rejected
  below 1, and the optimistic-concurrency check runs only when supplied. "Supplied" means *present
  and an integer* — an explicit `"version": null` is 400, not an unconditional update. The first cut
  of the fix treated null as omission and was caught in review: `param.Opt` represents null and
  omitted as distinct states, the wire types the field as an integer, and the pre-bump handler
  rejected null — so silently accepting it would have dropped the concurrency check for any client
  that serialized a nil pointer, a regression dressed as a relaxation. A later review pass then
  pointed out that both the null rejection *and* its neighbour — a field-less update being a legal
  no-op that still bumps the version, reachable with an empty body only because `version` became
  optional — are choices the schema does not settle, so they belong in docs/DIVERGENCES.md's INFERRED
  section rather than only in a changelog entry; the entry was added and the empty-body case pinned
  by a test. The reviewers disagreed on this: the verifier judged the null rejection generic strict-input
  validation needing no entry, the reviewer judged it an unconfirmed inference. Registering it is the
  cheaper error, and CLAUDE.md's rule ("a divergence not in the registry is a finding") is stated
  without exception. The second change,
  `BetaManagedAgentsModelConfigParams.Effort` (`betaagent.go:2117`) plus its five level types, is new
  behavior this slice does not build — **recorded** as a CONFIRMED divergence, tracked by #160.
  Everything else in that file's 435-line diff is the effort types' own scaffolding: the field-level
  diff of json-tagged struct members contains nothing but `Version` and `Effort` members.
- *`betasession.go`* — exactly two hunks, both `BetaSessionNewParams.InitialEvents`
  (`betasession.go:2084`). `createSession`'s strict key allowlist rejects it 400. **Recorded** as a
  CONFIRMED divergence, tracked by #161; half its union (`user.define_outcome`) is the post-v1
  outcomes surface (#77).

The remaining four changed files reach nothing this repo mirrors. `betasessionevent.go` is
**comment-only** (proven by filtering the diff to non-comment lines: empty) — one sentence re-words
why a turn ends `retries_exhausted`, dropping `max_iterations` as a listed cause; this repo cites
neither the sentence nor `max_iterations` anywhere. `betamessage.go` (and its non-beta twin
`message.go`) add the `general_harms` refusal category to two enums; this repo mirrors no refusal
category at all. `betasessionthreadevent.go` extends `event_deltas` to the *thread* stream and fixes
that method to forward its params — session threads are post-v1 (#53), and the **session** stream's
`event_deltas` already existed at v1.58.0, so nothing about the SSE surface this repo implements
moved. `betamessageutil.go`/`messageutil.go` fix client-side `ToParam` citation copying, in helpers
this repo does not call (its provider adapters build request params directly).

**Question 2 — does `shared/constant/constants.go` add stop reasons or event types the taxonomy
should carry?** **No new stop reason and no new session event type — but the file holds this bump's
one genuinely dangerous change.** `constant.EnvironmentDeleted`'s literal moved from
`"environment_deleted"` to `"environment.deleted"` (`constants.go:111,337`). The Go identifier is
unchanged, so nothing would fail to compile and no test that does not assert the literal would fail.
Traced to its consumers, it is safe here for a specific reason rather than by luck: the constant was
**repurposed** for the new webhook event types (`environment.created/updated/deleted/archived` and
`memory_store.created/deleted/archived`, `betawebhook.go:414` et al), and the `DELETE /v1/environments/{id}`
response — the only place this repo emits that literal — was simultaneously given its own dedicated
enum still carrying the old value (`betaenvironment.go:487`,
`BetaEnvironmentDeleteResponseTypeEnvironmentDeleted = "environment_deleted"`). So
`internal/api/environments.go:534`'s `"environment_deleted"` is still correct, and
`environments_test.go:249` still pins the right string. The webhook constants belong to a webhook
surface this repo does not implement; the session `{domain}.{action}` taxonomy is untouched, and the
three managed-agents stop reasons are still exactly `end_turn` / `requires_action` /
`retries_exhausted` (`betasessionevent.go`, `AsAny` switch), matching `internal/domain/event.go:103-105`.

**Question 3 — mirrored now, or recorded?** Resolved explicitly for each of the four new fields.
Mirrored: `secret` (the pin makes an existing self-description false, and null is the reference's own
value on every path but poll) and the optional `version` (the pin makes an existing rejection wrong).
Recorded with a tracking issue: `model.effort` (#160) and `initial_events` (#161) — both are real
behavior, not shape, and honoring them means provider-side and event-pipeline work that CLAUDE.md's
"simplicity first" places behind the backlog. Thread `event_deltas` needs no entry: it is a surface
(#53) this repo has not built, and per the v1.58.0 record's rejected decision, a product area with
only one implementation is not a divergence.

**Citation durability — the trap fired this time.** The live pinned-version label moved in three
places (`.claude/agents/verifier.md`, `docs/REFERENCE_PROJECTS.md`,
`internal/toolset/definitions.go`'s accepted-key comment — the last is safe to advance because the
`agent_toolset_20260401` input types are untouched by this range). Every `file:line` the registry
cites was re-read at v1.59.0 rather than assumed: `lib/environments/poller.go:439-465` (the 204 /
`WithResponseBodyInto` comment) and `worker_test.go:118-120` (`WriteHeader(http.StatusNoContent)`)
hold verbatim; so do `betaagent.go:1123-1124/1178-1179` (skill `Version` on both union variants),
`betasession.go:693-717` (the file resource variant), `betasessionresource.go:176-209`, and
`tools/agenttoolset/skills.go:123-146` (`resolveSkillVersion`). One **drifted**: the Stop Work entry's
`api.md:656-673` no longer covers the work resource — v1.59.0 added 17 lines to `api.md` and the
`BetaSelfHostedWork`-returning Stop method is now `api.md:683`. The citation was corrected in place
and the drift noted, since it is the concrete instance of a hazard the v1.58.0 record could only warn
about. The v1.56.0/v1.58.0 mentions surviving in CHANGELOG.md, `docs/HISTORY.md` above, and archived
plans 05/06/07 are historical records of what was true when those PRs landed, and were deliberately
left alone.

**Evidence.** Both code changes were driven by tests that fail against the pre-change code — the
recorded red run: `TestAgentUpdateOptimisticVersioning` 409 where 400 was wanted (version 0 reached
the version *comparison* instead of being rejected as below the documented minimum), and
`TestWorkPollReturnsWireShape` / `TestWorkGetReturnsItem` reporting `required wire field "secret"
missing`. `make verify` green afterward at total statement coverage **90.7%** (including the Docker and K8s
sandbox suites; the figure moves run to run — 90.65% to 90.72% across four runs here — and the gate
is ≥90%). No transitive dependency moved: `go mod tidy` after the bump touched
`go.mod`/`go.sum` only, in the two lines naming the SDK — so every SDK request type, response field,
JSON tag, service method, option, paginator, SSE helper and error decoder reached from the non-test
import sites (`internal/provider/anthropic/anthropic.go`, `internal/worker/{client,lease,toolexec,skills}.go`)
still compiles and passes unchanged. Unlike the v1.58.0 bump, `sdk.Client`'s own layout did not move
either — `beta.go`, `client.go` and `aliases.go` are identical across the tags, so no new service is
constructed at client init; the only unconditional runtime difference is the version-identifying
`User-Agent` / `X-Stainless-Package-Version` header value (`internal/version.go`).

The one non-obvious hazard in emitting a new null field was checked rather than assumed: the BYOC
worker polls with the SDK's **typed** decoder into a non-pointer `Secret string`
(`internal/worker/lease.go:163-175`), so `"secret": null` had to be provably harmless there. It is —
decoding the exact queued-item body this platform now emits yields `Secret == ""` with
`JSON.Secret.Valid() == false`, i.e. null is distinguishable from an empty credential rather than
being an error. `internal/worker`'s harness already polls against the **real** control-plane handler
rather than a fixture (the same property `TestWorkerForceStopAcceptsNoContent` relies on), so
`TestWorkerPollsRunsAndStops` exercises that decode end to end in the passing run.

**Two review findings refuted rather than fixed**, recorded because the refutations are the durable
part. (1) *"The `version < 1` and non-integer checks run before `checkID`, so a bad version against a
nonexistent id reports 400 instead of 404."* True, but pre-existing and house style, not introduced
here: `updateAgent` validated the body before the path id before this change too (a missing `version`
was already a 400 ahead of the 404), and `updateEnvironment` orders the same way — `decodeObject` →
`rejectUnknownKeys` → `checkID`. Reordering agents alone would make it the outlier. (2) *"CHANGELOG
and DIVERGENCES claim `secret` renders on five paths but only poll and get have tests."* The claim is
true by construction rather than by sampling — all five handlers render the one `toWire`
(`workapi.go:129,165,263,302,319`), so a field cannot be present on one and absent from another. The
finding's deeper point — that `wantFields` checks presence only, so no test would catch an *extra* or
renamed field — is a property of the test harness that predates this branch and is not this bump's to
redesign; the get path's `secret` assertion was added because that one was a genuine gap between the
record's wording and the test.

## Sandbox file primitives (#71) — review-hardening record (2026-07-26)

The fix itself is in [CHANGELOG.md](../CHANGELOG.md); this is what the dual review changed about it,
and what it decided not to change.

**The setup.** Codex on `gpt-5.6-sol` at the config's `ultra` effort, told explicitly not to run the
test suite (an earlier Codex pass in this session left a background `make verify` running in the
worktree, contending with the verifier's own for the Docker daemon and the kind cluster). The
verifier ran on **Opus 5, not its pinned `claude-fable-5`**: that model's quota was exhausted
mid-session and the first dispatch died on it. The `run-reviews` skill's "do not override to opus"
note dates from when the quota problem was believed over; a verifier that cannot run at all is worse
than one on a different strong model, and the override is disclosed on the PR. The Claude-side
`/code-review` is `disable-model-invocation` and was not run.

**What the review proved that the implementation had not.** The verifier replayed HEAD's tests
against `main`'s backends — every new contract row red on both, reproducing the quoted messages
including the destroyed directory — then broke six specific things in throwaway copies (dropping
`[ -h ]` from the walk, flattening the walk to one level, dropping either backend's `-d` guard,
making `discard` a no-op, deleting the `fileFault` case) and confirmed each breaks a test. That is
the evidence that the new rows are not tautologies, and it is not something the implementer's own
green run can establish.

**Fixed in the same PR.** Both reviewers arrived at the same seam from different directions:

- **The path classifier resolved `dirname` through the agent's PATH.** The sandbox filesystem is the
  agent's, so `dirname` is the agent's too. Measured against the pre-fix shell with a `dirname` that
  echoes its argument: the walk was **still spinning after 4 seconds**, and the file-tool exec that
  runs it carries no timeout of its own. Rewritten in bash builtins alone (`case`, `${p%/*}`), which
  no file on PATH can shadow. `mv` and `rm` have no builtin equivalent and remain resolvable — an
  agent can still make its *own* file tools misreport — which is the sandbox being the agent's own
  rather than a boundary between sessions, and is now what the docs say.
- **A component whose name ends in a newline was walked as its newline-less sibling**, because
  `$(dirname …)` strips trailing newlines and the tools accept such names (only NUL is rejected).
  Confirmed against the old shell: it called `…/block\n/x/y` clear while a regular file was in the
  way. The builtin expansion is byte-exact, and the k8s read path stopped computing a parent through
  a second substitution.
- **`[ -d target ]` followed by `mv` is a TOCTOU.** Something in the sandbox can make the target a
  directory in between, and then `mv` puts the file *inside* it and exits 0 — a reported success
  that wrote nothing where the caller asked. Both backends now ask again after the move, remove what
  went in, and refuse the same way. The race itself cannot be interleaved from a test, so the k8s
  script test stages its *outcome* instead: a shimmed `mv` on PATH that does what a racing `mv`
  would — create the target as a directory and move the file inside it — and the script is held to
  answering `ErrIsDirectory` and leaving nothing behind.
- **The interface promised more than the rename delivers.** A symlink *to a directory* satisfies
  `-d`, so it is refused as a directory rather than supplanted. The docs were corrected and a
  contract row now pins what both backends actually do with a symlink target.
- **Two documents claimed the file primitives were shell-free** — `docs/ARCHITECTURE.md`'s toolset
  row, and the bash tool's restart comment, which justified writing the restart's `head` through
  `WriteFile` on exactly that ground. The k8s backend has read and written through a script since it
  existed; this change put docker in the same position. Both now say what is true.
- **Residue and coverage.** Docker sheds its landed temporary file on the two failure paths that
  kept it, and a fake-daemon test pins the buffered case the live suite cannot reach (the streaming
  half is reachable through a short src; the buffered half needs a put that fails *after* landing
  bytes, which a real daemon will not do on demand).

**A second verification pass over the hardening itself.** Re-dispatched after those fixes, the
verifier returned PASS with eight findings, none of them behavior: three of the branch's own new
guards had nothing that failed without them (both docker discards, and the post-move recheck), a read
row for a newline-named blocker existed only for the shared shell and not for the k8s script, and
four written claims were imprecise — the permission-bit comparison attributed to "the reference"
rather than to the harness snapshot it came from, a CHANGELOG sentence placing the newline and
`dirname` tests in the k8s package where they are not, the shared shell's test helper claiming every
backend passes a path as an argument when docker interpolates it shell-quoted into the script text,
and a coverage figure quoted from an earlier run. All eight are answered; every assertion added for
the first three was checked by removing the guard it covers and watching that test alone go red.
That a hardening pass needs its own hardening pass is the point of the rule that the implementer
never certifies their own work — these were guards written *in response to a review*, and they still
arrived untested.

**Evaluated, and deliberately left for their own issues.** Each is real and measured; none of them
is what #71 asked for, and each would widen the change in a way a bug fix should not:

- **[#204] The target's permission bits are not preserved** (`755` → `644` on k8s; docker's tar
  header has always been a fixed `0644`, so this is convergence rather than a one-sided regression).
  The Claude Code harness's own atomic write stats the target and chmods the temp before renaming
  (`src/utils/file.ts:392,430,437` in the local snapshot — a harness-design observation, which is all
  that source is authority for, and not a wire behavior of the managed-agents reference). Doing the
  same means both backends or neither — otherwise the contract suite is back to papering over a
  divergence — and it grows the docker image contract by a `stat -c`.
- **[#205] A target that cannot be renamed onto fails unclassified.** A bind-mounted file
  (`/etc/hosts`) went from a k8s success to `exit 1` with no sentinel, which is #71's own failure
  mode one path further down. Device nodes are the one target the two backends still answer
  differently (k8s replaces the node; docker cannot land its temp under a mounted `/dev`). The two
  candidate shapes — classify it, or fall back to writing through where a rename is impossible —
  trade against the atomicity guarantee itself, which is a decision, not a fix.
- **[#206] A docker write costs one extra exec**: 2.2 ms → 15.3 ms per buffered write, measured on a
  warm directory. Skill materialization writes one file at a time and `internal/skills` accepts
  10,000 members, so a large-but-valid skill pays it 10,000 times. The seam that fixes it is a bulk
  write on `sandbox.Sandbox` — a new interface method and a new contract row.

**Reproducing the acceptance on macOS.** The gated k8s contract rows need
`MAP_K8S_HOST_ADDR=host.docker.internal`. `k8sHostAddr`'s kind branch returns the docker network's
IPv4 gateway, which on darwin lives inside the Docker VM rather than on the host where the fixture's
stub listens; the row fails identically on `main` without the override (verified at 79b8ac6) and
passes in ~14 s with it. The cluster must also be created from a shell with no `HTTP_PROXY` set, or
kind bakes it into the node and nothing can be pulled.

## MinIO chart readiness probe (#216) — review-hardening record (2026-07-26)

The fix itself is in [CHANGELOG.md](../CHANGELOG.md); this is what the dual review changed about it,
and the two things it decided not to change.

**The setup.** Codex on `gpt-5.6-sol` at the config's `ultra` effort, read-only. The verifier ran on
**Opus 5, not its pinned `claude-fable-5`** — that quota was exhausted and the first dispatch died on
it, the same override and the same reasoning as the #71 record above, disclosed on the PR. The
Claude-side `/code-review` is `disable-model-invocation`; an Opus 5 adversarial pass stood in for it
and is labelled as a stand-in rather than as `/code-review`.

**Fixed in the same PR.** The two reviewers converged on the causal story and split on the rest:

- **The CI guard was coupled to the prose of the comment beside it.** The first version grepped the
  whole rendered template for the substring `/minio/health/ready`, which also reads the comment that
  explains why that endpoint was rejected. Reproduced: a chart with the **correct** probe whose
  comment spells the old path fails the job — so the natural reaction to a CI failure, documenting
  the rejected endpoint, breaks the build. The step now extracts the rendered `readinessProbe` path
  and asserts it exactly, which is also structurally anchored to the probe rather than to the file.
  That exact assertion is narrower than the substring it replaced — a `/minio/health/ready`
  reintroduced under a *different* probe would have slipped past it — so the verifier's catch of that
  narrowing was answered by a second assertion over rendered `path:` keys, which restores that
  coverage for the plain block style every probe in this chart is written in — a quoted
  (`path: "/minio/health/ready"`) or flow-style (`httpGet: {path: …}`) scalar evades it, which the
  re-verification pass measured and is recorded here rather than answered with quote-stripping in a
  CI `awk` — while staying immune to the comment wording that caused the original defect. Three cases
  were exercised against the final step: the pre-fix chart, `/minio/health/ready` reintroduced as a
  `livenessProbe`, and a correct chart whose comment names the old path (the first two red, the last
  green).
- **The entry attributed a fresh-install `CrashLoopBackOff` to this probe, and it does not follow.**
  `initialDelaySeconds: 5` keeps the kubelet from marking the pod Ready before t+5s, while the
  object layer serves ~0.3 s in on the measured shape — so the old probe's Ready was accidentally
  truthful, guaranteed by the delay rather than established by the probe. The change hardens a latent
  defect (init slower than the delay: a populated drive, a heal, a cold or slow PVC, a contended
  node) and makes readiness honest for `helm install --wait` and rollout gates. Both reviewers
  separately noted that the crashloop which *does* occur on a fresh install is an ordering gap this
  does not close: nothing orders the controlplane after MinIO, so it exits by the same fail-fast path
  when the headless Service publishes no endpoint at all. The entry now says both things.
- **Comment placement.** `openbao.yaml` has the identical construct — a readiness probe whose
  endpoint choice needs justifying — and puts a three-line comment above `httpGet:`. Matched.

**Evaluated and rejected: an explicit `timeoutSeconds` on the probe.** Codex's strongest finding was
that `/minio/health/cluster` reaches the drive where `/minio/health/ready` does not, and Kubernetes
defaults `timeoutSeconds` to 1 — so on a stalled network-backed PVC three consecutive probes could
time out and withdraw the only endpoint of a single-replica headless Service, which routes around
nothing and merely breaks DNS for running clients. The mechanism is real; the magnitude is not.
Measured against the pinned release with 3,000 objects on the drive and sustained `mc` write+list
load: `/minio/health/cluster` p50 1.6 ms, p99 2.2 ms, max 2.4 ms — indistinguishable from
`/minio/health/ready` at p50 1.6 ms / p99 2.1 ms, and 0 of 200 probes within two orders of magnitude
of the 1 s default. Against that margin, adding the knob would also break the chart's probe
convention: every *readiness* probe in this chart is `initialDelaySeconds: 5` + `periodSeconds: 10`
(the controlplane's liveness probe is the one 10/20 outlier), and not one probe of either kind sets a
timeout — OpenBao's own strict health endpoint included.

**Evaluated and rejected: `/minio/health/cluster/read`.** `/cluster` gates on write quorum and
`/cluster/read` on read quorum, which is the milder gate when a StatefulSet has several pods. At one
node with one drive the default parity is zero, so both quorums are 1 and the two endpoints are
equivalent — confirmed from the live headers (`X-Minio-Write-Quorum: 1`, `X-Minio-Read-Quorum: 1`).
The chart hardcodes `replicas: 1` with no value to raise it, so the multi-node trap the milder gate
would avoid is not reachable from the chart's own surface.

## anthropic-sdk-go v1.61.0 bump — wire-schema verification record (2026-08-02, plan 21 slice 1)

The third bump record, and the quietest of the three: the range moved no shape this repo mirrors,
required zero code, and its one genuinely new fact is a documentation sentence — the reference's
event-list params are now documented as keyed on `processed_at`, which this platform deliberately
does not do (new DIVERGENCES entry, below). The bump exists for plan 21's acceptance goal: the
outcomes surface verifies against the latest SDK release, and v1.61.0 is the latest
(released 2026-07-24; confirmed against the upstream tag list on 2026-08-02).

**What the range contains.** Two upstream releases: v1.60.0 (2026-07-23 — the
`model_context_window_exceeded` stop reason, apijson param-unmarshal fixes (#73), RawJSON
HTML-escaping in the marshaler, `ant` naming in auth errors) and v1.61.0 (2026-07-24 — the
`claude-opus-5` model constant, request-side `tool_addition`/`tool_removal` blocks, client-side
fallback-credit token types and the server-side `fallbacks: "default"` option). Endpoint count is
unchanged at **131** (`.stats.yml`); the spec hash moved, so all change is to existing endpoints'
schemas. The outcome surface plan 21 mirrors is untouched — `betasession.go`, `betawebhook.go`,
`betadeployment.go` are byte-identical between the pins, and `betasessionevent.go`'s only change is
comment-only (below).

**The enumeration.** Every SDK file defining a shape this repo mirrors, `git diff`ed pairwise
between the tags rather than sampled. Unchanged (each proven by an individually-run empty pairwise
diff): `betaagentversion.go`, `betaenvironment.go`, `betaenvironmentwork.go`, `betasession.go`,
`betasessionresource.go`, `betasessionthread.go`, `betasessionthreadevent.go`,
`betasessiontoolrunner.go`, `betaskill.go`, `betaskillversion.go`, `betafile.go`,
`betavaultcredential.go`, `betadeployment.go`, `betawebhook.go`. Changed, each resolved:

- *`betaagent.go`* — exactly one added line: `BetaManagedAgentsModelClaudeOpus5 = "claude-opus-5"`.
  `BetaManagedAgentsModel` is a `= string` alias, the platform mirrors no model list (design
  principle 4: model ids are opaque config-resolved strings), so this is a no-op — but the insertion
  shifts two live registry citations (see citation durability).
- *`betasessionevent.go`* — **comment-only, proven** (the diff filtered to changed non-comment
  lines is empty): the `BetaSessionEventListParams` docs now say each `created_at[gt/gte/lt/lte]`
  filter is "Compared against the event's `processed_at` value" and `order` is "ordered by the
  event's `processed_at`" where both previously said `created_at`. Param names, tags, and types are
  unchanged. This platform filters on the `created_at` column and orders by `seq` (≡ created_at —
  appends serialize per session), and keying on processed_at is incoherent under its deliberate
  null-until-settlement stamping model (every queued inbound event would vanish from filtered lists
  mid-turn; causality would invert; the settlement batch shares one timestamp). **Recorded** as a
  new deliberate-divergence entry cross-linked to the processed_at-stamping entry, with the
  single-source caveat (SDK comment and CLI usage string are generated from the same OpenAPI
  descriptions — one witness, not two) and the #78 recording flag. Note: a pre-merge draft of plan 21
  misattributed this hunk's content (it described the v1.59.0 bump's own comment hunk); the
  verifier pass on PR #254 caught it before merge, so the plan landed correct, and the sweep
  here re-derived the content from the tags independently.
- *`betamessage.go`* (800 insertions / 25 deletions) — the bump's bulk, all on the beta **Messages** surface this platform
  does not mirror: `tool_addition`/`tool_removal` are **request-side param blocks only** (in
  `BetaContentBlockParamUnion` and the new mid-conversation-system content union; the literal string
  `tool_change` appears in no Go source, only the changelog) — no response variant, no streaming
  event, no session-event variant exists, so a model cannot emit one and the platform's inbound
  content-block allowlist (`internal/events/inbound.go`, text/image/document plus search_result on
  tool results) 400s one posted from outside before it reaches the log. Fallback-credit expansion:
  request param retypes, a new required `usage.fallback_credit` on beta usage types,
  `stop_details.fallback_credit_token`, and two new beta-header gates — all on beta Messages
  request/response shapes the platform neither emits nor decodes (the adapter reads its four usage
  counters through the non-beta types). No-op.
- *`message.go` / `betamessage.go` stop reasons* — v1.60.0 adds `model_context_window_exceeded` to
  the non-beta `StopReason` (and one beta doc-comment line; the beta enum value already existed at
  v1.59.0 — three occurrences). The adapter passes stop reasons through as opaque strings with no
  switch; the brain classifies turns on tool blocks, not the label, and both registry entries that
  reason about stop labels already name `model_context_window_exceeded` explicitly. No-op.
- *`shared/constant/constants.go`* — **additions only: 21 inserted lines, 0 deletions**, so the
  v1.59.0 bump's dangerous class (unchanged identifier, moved literal) is structurally empty this
  time — a moved literal would appear as a minus/plus pair. Seven new constants (`Default`,
  `MCPToolReference`, `MCPToolsetReference`, `NotApplied`, `Redeemed`, `ToolAddition`,
  `ToolRemoval`), all belonging to the fallback/tool-change surface above. No new stop reason, no
  new session event type; the managed-agents stop-reason union is still exactly
  `end_turn`/`requires_action`/`retries_exhausted`.
- *`api.md`* (+10) — new params/response type rows for the fallback and tool-change types, all
  above line 683; the route table is unchanged. The Stop Work entry's citation drifts 683 → 693
  (corrected in the registry; the second drift of the same row — the v1.59.0 record noted the
  first).
- Everything else: `betatoolrunner.go`/`lib/betafallback` (client-side helpers the platform does
  not import — the sole non-test mention in `internal/` is a design-reference doc
  comment, the brain's refusal-handling note citing `executeTools`), `betamessagebatch.go` (no batches endpoint
  here), `beta.go` (+2 header constants; `anthropic-beta` is accepted and ignored),
  `internal/apijson`/`packages/param`/`unmarshalcompat.go` (the v1.60.0 unmarshal rework — a
  rewrite of the shared decoder core, not only the param path: exactness became a
  struct-tracked score, unknown enum values coerce instead of failing strict decodes,
  default-tagged constants gained their own decoder — the platform's exposure is its SDK
  *response* decodes, all exercised green under the new pin; it never unmarshals SDK
  param types),
  auth/packaging.

**Behavior-fix exposure.** The RawJSON HTML-escaping fix lands on the adapter's hot path:
`param.SetJSON` passthrough bytes are now compacted and HTML-escaped (a literal `<` becomes its
`\u003c` escape) — JSON-equal, not byte-equal. No golden-byte test exists to break (the provider fake
decodes bodies and compares semantically; no fixture contains escape-sensitive bytes), and the
adapter's comment claiming "verbatim" serialization was updated to say field- and value-preserving.
The apijson param fix has no platform exposure (nothing unmarshals SDK param types).

**Citation durability — 35 live citations re-read at v1.61.0, 32 hold, 3 drifted.** All three are
mechanical line shifts with the underlying claims intact: the Stop Work entry's `api.md:683 → 693`
(the +10 api.md lines), and the `claude-opus-5` insertion shifting `betaagent.go:2117 → 2118`
(model-config `Effort`) and `betaagent.go:2866-2870 → 2867-2871` (optional update `Version`) — all
corrected in the registry. The live pinned-version label moved in the same three places as the
v1.59.0 bump (`.claude/agents/verifier.md`, `docs/REFERENCE_PROJECTS.md`,
`internal/toolset/definitions.go` — safe to advance: the `agent_toolset_20260401` input types are
untouched) plus the registry's twelve live evidence labels; version-history prose ("v1.59.0 added a
required `secret`…") stays as written, and CHANGELOG/HISTORY/archived plans keep their historical
citations per the standing precedent. Plan 21's own SDK ranges were re-confirmed at the tag (its
`_ongoing` citation widened by four lines to include the quoted doc comment).

**Evidence.** `make verify` green on the bumped pin at total statement coverage **90.80%** (an independent verification rerun printed 90.79% — the figure moves run to run and both clear the >=90% gate; Docker
and K8s sandbox suites included). `go mod tidy` touched only the two lines naming the SDK — the
SDK's own go.mod is byte-identical between the tags, so the bump drags in no new module. The sweep
itself ran as four parallel investigations (diff enumeration; live-citation re-read; list-semantics
analysis; provider-stream impact) whose reports cross-checked each other's file lists, and the
provider/worker/toolset suites — the packages that drive the SDK against the platform's own API —
were additionally run standalone under the new pin before the full gate.
## GCP staging environment (#20 slice 4b) — acceptance record (2026-08-02)

The first real execution of `deploy/gcp/`: both Terraform configurations applied against a
live project (`opensdlc-managed-agents`, project number 460647310105), the platform
deployed on GKE in mode 1, the acceptance battery driven by the **real `ant` CLI 1.21.0**
built from the read-only checkout, and the teardown proven by destroy → apply. This also
narrows [#75](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/75) — images
published and a real `helm install` accepted end to end, on GCP rather than generically.

**What was created.** `foundation/` applied 13 resources — three service accounts, the KMS
key ring and crypto key, three empty Secret Manager secrets, and five API enablements. `bootstrap.sh` filled them
and created the GCS HMAC key; run a second time it skipped every one, leaving exactly one
version per secret and exactly one HMAC key — the idempotency its unit suite simulates,
here against the live API. `environment/` applied 24 resources: a zonal GKE cluster, the
platform and tainted sandbox node pools, Cloud SQL with its database and user, Artifact
Registry, the GCS bucket, ten IAM and Workload Identity bindings, and six more API
enablements.

**Three configuration defects, all found by running it.**

1. *The Cloud Build API was in the wrong configuration.* Fixed before the run in #253: the
   API lookup that names the build identity cannot answer until the API is on, and only
   `foundation/` runs early enough to enable it.
2. *The Workload Identity bindings raced the cluster.* Their member string names the pool
   `PROJECT.svc.id.goog`, which is created by the first cluster configured with
   `workload_identity_config` — but it is built from `var.project_id`, so Terraform saw no
   dependency and scheduled both bindings in the first wave. They failed with `Identity
   Pool does not exist` roughly ten minutes before the pool existed. Fixed with an explicit
   `depends_on` on the cluster.
3. *Cloud SQL chose the wrong edition.* With `edition` unset the API selected
   ENTERPRISE_PLUS, which rejects every `db-custom-*` tier — including `var.db_tier`'s
   default — with `Invalid Tier (db-custom-2-7680) for (ENTERPRISE_PLUS) Edition`. Naming
   `edition = "ENTERPRISE"` settles it; the `connection_pool_config` block is accepted
   alongside it, which is the combination the instance was created with.

**The image build.** One Dockerfile whose three stages (`build`, `gate`, `server`)
produce four published images from two of them. The chart composes
`registry/repository/COMPONENT:tag`, so the server stage is pushed under three
per-component names — `controlplane`, `brain`, `executor` — as three tags on one build, and
all three resolved to the same digest (`sha256:39c0cad9…`), so nothing can drift between
them. `--target gate` is the fourth. `deploy/gcp/cloudbuild.yaml` is that build.

**Acceptance, all through the real CLI over `kubectl port-forward` (nothing public).**

- *Sessions and the async loop.* Agent, environment and session created with wire-correct
  id prefixes (`agent_`, `env_`, `sesn_`, `sevt_`). A bash turn closed the full loop —
  `agent.tool_use` → executor → `agent.tool_result` → `agent.message` — and the sandbox
  reported hostname `map-sesn-…`, so the tool ran in the per-session pod, not the executor.
- *Sandbox placement and bounds (slice 2, live).* The pod landed on a sandbox-pool node
  carrying `map.opensdlc.dev/sandbox` in both its nodeSelector and its toleration, with
  `allowPrivilegeEscalation: false` and `NET_RAW`/`SETUID`/`SETGID` dropped.
- *`file-answer`.* A file uploaded through the API, mounted as a session resource and
  materialized into the sandbox; the model read the mount and returned the recall token
  that appeared in no prompt.
- *`skill-answer`.* A skill uploaded, injected as Level-1 metadata, materialized, and
  followed: the model read `SKILL.md`, then `answer.txt`, and returned that token exactly.
- *HITL.* An `always_ask` toolset suspended the first tool call with
  `stop_reason: requires_action`; a `user.tool_confirmation` with `result: allow` released
  it and the turn completed.
- *The egress gate on K8s.* For a `limited`, vault-attached session the executor attached
  the gate as a **native Kubernetes sidecar** — an entry in `initContainers` with
  `restartPolicy: Always`, which is where to look for it. The sandbox held only the
  placeholder `vltph_…`; the origin received a value whose SHA-256 equalled the stored
  secret's, so substitution happened at the gate and the credential never entered the
  sandbox. A host outside `allowed_hosts` was refused by the gate with `403 host not
  permitted by the environment's networking policy`.

**Traces reach Cloud Trace.** An OTel Collector with the `googlecloud` exporter, the chart
pointed at it via `otlp.endpoint`. Cloud Trace recorded the platform's own spans —
`model_turn`, `model_request`, `tool_exec`, and the HTTP server spans for `POST
/v1/sessions` and the events endpoint, plus the gate's `GET /internal/v1/gate/config`.

Two things about this are worth writing down for the deploy guide. First, on a
Workload-Identity cluster the node service account's project roles do **not** reach pods:
GKE stops serving the node identity through the metadata server, so the collector needed
its own annotated KSA bound to a Google service account holding `roles/cloudtrace.agent`.
Granting the node account `roles/editor` — which this project already does — changes
nothing. Second, the Cloud Trace v1 `traces.list` API shards its results: the first page
came back empty with a `nextPageToken`, and following it produced 22 traces. An empty first
page is not an empty result.

**Two measurements, recorded rather than discovered.**

- *Sandbox capacity before scheduling degrades (#64).* Pods shaped exactly like a sandbox
  (100m CPU request, the pool's selector and toleration) were scheduled until they stopped
  fitting: **68 sandbox pods** across two `e2-standard-4` nodes, then `Unschedulable —
  Insufficient cpu`. The binding constraint is **CPU requests, not the 110-pod cap**. With
  no production caller of `Sandbox.Destroy`, every session holds its 100m forever, so this
  pool degrades at roughly 68 sessions — which is what turns #64 from tidiness into a
  sizing constraint.
- *Workspace disk per session.* A session that wrote a three-file Python project and ran
  its tests used **0.43 MiB** of ephemeral storage; sessions doing trivial work used
  0.04–0.09 MiB. At these rates the 100 GiB sandbox boot disk is dominated by the image,
  not by workspaces, and disk is nowhere near the binding constraint that CPU is.

**Teardown proof — destroy → apply, against the three things a rebuild can fail at.**

1. *The second apply succeeds with no KMS name collision.* `environment/` destroyed all 24
   resources cleanly — the object-bearing GCS bucket included — and re-applied all 24. The
   foundation's key ring and crypto key survive untouched because `environment/` only reads
   them.
2. *The surviving secrets reconcile against freshly created resources.* The 64-byte
   database password from Secret Manager authenticated against the **new** Cloud SQL
   instance (`AUTHENTICATED as map on map`) — the destroy took the old user with it and the
   apply reapplied the surviving password through `password_wo`. The stored GCS HMAC pair
   was proven by an **authenticated call** rather than by resolving its access ID: PUT, GET
   and DELETE of an object in the rebuilt bucket, over the S3-compatible endpoint.
3. *A fresh vault credential round-trips on the rebuilt stack.* Deliberately fresh, not an
   old one surviving: `environment/` owns Cloud SQL, so the destroy takes the vault
   ciphertext with it by design. A credential created after the rebuild produced a
   placeholder in the sandbox and the real secret at the origin, hashes equal.

**Two observations that are not defects.** The three platform pods restart two or three
times on a first install while Postgres finishes coming up — the DSN host does not resolve
yet, they exit, and the backoff resolves it without intervention. And the real `ant` CLI
cannot upload a multi-file skill to this server: its form encoder names each part
`path.Base(file.Name())` (`internal/apiform/encoder.go`), so it can only ever send bare
basenames, while the server requires names qualified under the skill's top-level directory.
The zip upload form carries the paths inside the archive and works, which is how the
`skill-answer` skill was uploaded. Whether the reference API accepts bare basenames was not
established here.

**A credential exposure this run caused, and closed.** The first `.gcloudignore` written
for the build did not carry `#!include:.gitignore`. A custom `.gcloudignore` *replaces*
gcloud's default behaviour rather than extending it, so every gitignored-because-secret
file in the checkout entered the uploaded source archive — `.env` (three live API keys) and
both `terraform.tfvars` among them — on all three builds submitted before the fix.
`.dockerignore` does not help: it filters what reaches the build, after the upload. Measured
both ways with `gcloud meta list-files-for-upload`: without the include those files upload,
with it they do not, and the build context still carries the Dockerfile, `go.mod`, `go.sum`
and 318 `internal/` files. The archives were deleted, the bucket's seven-day soft-delete
retention cleared (three recoverable copies purged, verified zero remaining), and the
Cloud Build bucket removed. Rotating the exposed keys is the operator's call.

**Cleanup, and what it missed.** `environment/` was destroyed after the run and the IAM
added by hand for the OTel collector removed; `foundation/` is retained by design
(Decision 9) and its KMS key ring can never be deleted in GCP. The teardown left no
cluster, no Cloud SQL instance, no Artifact Registry repository and no buckets — and this
record originally stopped there, saying nothing billable remained. **That was wrong**, and
a project-wide audit the same day found what it had missed.

`terraform destroy` does not reclaim the PersistentVolumes GKE creates for a
StatefulSet's PVCs. The chart's bundled Postgres, MinIO and OpenBao each get one, the GKE
PD CSI driver creates a Compute Engine disk for each, and those disks are not in
Terraform's state — so the destroy took the cluster and left them. **Six** were still
billing: two 8 GiB (Postgres), two 8 GiB (MinIO) and two 1 GiB (OpenBao), one set per
build-up because the environment was created and destroyed twice, 34 GiB at roughly
$3.40/month with nothing able to re-attach them. The two OpenBao volumes held the vault's
own state, so this was a data-remanence question as well as a cost one. They were deleted;
`gcloud compute disks list` then returned nothing project-wide.

**There is a trap in cleaning these up**, and it is the reason this is written down rather
than fixed silently: only the older three still carried `goog-k8s-cluster-name=map-staging`
and `goog-terraform-provisioned=true`. The newer three had **no labels at all**, so a
label-filtered sweep removes exactly half the leak and reports success. Identify them by
the PVC name in the disk's `description` instead. `docs/deploy-gcp.md` states this as a
required teardown step rather than an optional tidy-up.

The audit also found an **active GCS HMAC key** for the storage service account that had
outlived the bucket it was made for — a working S3-interop credential surviving the
teardown — together with the three Secret Manager secrets holding it and the database
password. All four were removed. What is retained, and costs about $0.06/month, is the
foundation's single enabled KMS key version; that is Decision 9 working as intended, not a
leak.
