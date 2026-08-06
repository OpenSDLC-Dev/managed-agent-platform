---
status: in-progress
issue: "#64"
---

# Sandbox teardown: the reaper, and the workspace that survives it (plan 24)

Nothing in production destroys a sandbox. Both backends implement an idempotent
`Destroy`, and its only callers are tests — every session leaves its container or Pod
holding its CPU request forever, which is what turned the GCP staging pool's measured
ceiling of 68 sandbox Pods into a ceiling of 68 *cumulative* sessions
(docs/HISTORY.md § the two measurements; docs/deploy-gcp.md's sizing warning). Since
#29/#296, the leak has a sharper edge: adoption refuses a spec-mismatched sandbox with
`ErrSpecMismatch` and deliberately deletes nothing, so a deployment that changes
`EXECUTOR_IMAGE`, `EXECUTOR_WORKDIR`, or a session's environment networking strands every
existing session in a reclaim loop with no exit — the platform has no replacement
lifecycle, and this plan builds the missing half. Three comment debts in the gate code
name "the standalone teardown reaper" as the long-term owner of orphan cleanup
(internal/sandbox/docker/docker.go, gate.go); this plan is that reaper.

Tracking issue: [#64](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/64).
It also settles the workspace-continuity half of
[#28](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/28): continuity across
a teardown is **supported within caps**, carried by a blob checkpoint — not by daemon-local
volumes — precisely so a resume does not depend on which executor or daemon serves it. The
multi-executor *affinity* half of #28 (two daemons growing two live containers for one
session) stays open, narrowed: the checkpoint bounds a fork's damage to
data-since-last-checkpoint, but does not prevent the fork.

## Ground truth (pinned 2026-08-06, worktree base 1e65a63)

Everything below was read or measured in this repo, this week.

- **No production `Destroy` caller**: every `.Destroy(` hit under `cmd/` and `internal/`
  is in `_test.go` files or `internal/sandbox/sandboxtest`. The ownership label
  `dev.opensdlc.managed-agent-platform.session-id` is written at create
  (docker.go / gate.go / k8s.go) and read only by adoption's `ours` checks — never listed.
  `sandbox.Provider` has exactly one method, `Provision`; the Docker API client has
  container endpoints only for a single ref — no list.
- **The wire cannot see a sandbox.** No Managed Agents endpoint exposes container
  existence, so eventual teardown is wire-indistinguishable from immediate teardown. The
  reference docs pin only: "Delete a session to permanently remove its record, events, and
  associated sandbox"; "A `running` session cannot be archived" (and the same for delete);
  self-hosted lifecycle is "Managed by you"; no cloud sandbox TTL is documented anywhere.
- **A teardown work item is structurally impossible**: `work_items.session_id` is
  `REFERENCES sessions(id) ON DELETE CASCADE` (0001_init.sql) — the delete that needs the
  teardown destroys the item that would carry it.
- **`sessions.status` is a stored column** (0001_init.sql:75, CHECK
  idle/running/rescheduling/terminated). Archive is one-way (`COALESCE(archived_at, …)`,
  no unarchive endpoint here or in the SDK). Neither `archiveSession` nor `deleteSession`
  checks status today. The gate-token layer is already archive-aware — `Authenticate`
  joins `s.archived_at IS NULL` (fail-closed, internal/gatetoken/gatetoken.go), and
  gateconfig re-applies the same check — so archive needs no token revoke; delete cascades
  the rows.
- **A session awaiting tool confirmation is status `idle`** (brain's requires_action
  suspension; internal/api/events.go dispatches confirmations only on idle). Approval
  latency is human latency — it exceeds any TTL.
- **Agent-written state spans four writable roots**, not one: workdir (`/workspace`),
  `/tmp`, `sandbox.ShellStateRoot` = `/var/lib/map-shell` (the bash tool's persistent
  cwd/env snapshots), and `/mnt` — with outcome deliverables under `/mnt/session/outputs`
  (internal/sandbox/hardening.go WritablePaths; internal/executor/harvest.go). On Docker
  each root is an anonymous volume removed with the container; on K8s each is a pod-scoped
  EmptyDir.
- **The harvest treats a missing outputs dir as a clean empty snapshot** (`cd … || exit 0`)
  and settles with delete-all + insert-all under the session row lock, then best-effort
  deletes replaced blobs (harvest.go). An empty tree therefore *erases* previously
  published deliverables from `GET /v1/files` and blob storage, reporting success. The
  harvest reaches the sandbox through the same `provisionSandbox` as tool_exec
  (harvest.go:136).
- **`queue.Claim` serves `tool_exec` for cloud environments only**
  (`AND (w.kind <> 'tool_exec' OR e.kind = 'cloud')`, queue.go:209); a self_hosted item is
  served only by the worker's Poll lane. The BYOC worker builds the *same* sandbox
  providers with the *same* label (cmd/worker/main.go), holds no database pool, and in the
  compose topology shares the executor's daemon (`/var/run/docker.sock` bind-mount).
- **Concurrency plumbing**: the executor's one pgxpool (default MaxConns
  `max(4, NumCPU)`, internal/store/store.go) feeds the queue's lease keeper, which already
  names "an Extend blocked on an exhausted pool" as the hazard its timeout bounds
  (keeper.go). The repo's only advisory-lock precedent is transaction-scoped
  (migrate.go:43). The executor's Run loop processes one claimed item at a time.
- **Transfer physics**: Docker's `GET /containers/{id}/archive` streams a tar and works on
  a *stopped* container; exec does not. K8s has no archive API — every byte rides an exec,
  and a `RestartPolicy: Never` pod that has Failed cannot exec at all. `blob.Store.Put`
  requires the exact size up front (blob.go). The sandbox image contract guarantees `tar`
  and nothing else — no zstd, no gzip (#206). The k8s bulk-write path already demonstrates
  the safe inbound route: stream an archive in and extract it *inside* the sandbox as the
  sandbox user; Docker's archive PUT is the opposite — the daemon extracts host-side as
  root. The write-tool family is unusable for restore: bulk writes chmod every member 0644
  (#212), killing executable bits.
- **The skills/files sentinels are agent-writable files inside the workdir**
  (`skills/.materialized`, `.files_materialized`); the skills skip-probe checks an
  id↔version bijection plus SKILL.md *presence* (internal/skills/extract.go) — a restored
  stale sentinel would suppress re-materialization across sandbox generations.
- **Deployment surfaces**: the shipped Helm executor Role grants create/get/delete on
  pods — the comment says "repeated Get (not List/Watch)" on purpose
  (executor-rbac.yaml) — so a lister needs an RBAC change. A blob-less executor is a
  documented, supported deployment at all three surfaces (cmd env docs, compose comments,
  helm optional secretKeyRefs). controlplane already carries `BLOB_*` wiring for
  skills/files.
- **`SpecMismatchRefusesAdoption`** (sandboxtest/contract.go): a mismatch refusal deletes
  nothing and the original spec must still adopt — any reap criterion derived from a
  caller-supplied spec would break this contract; every criterion below derives from the
  session's lifecycle in the database instead.

## Design decisions

1. **One owner: a reaper goroutine in the executor.** The executor is the only process
   holding both the sandbox provider and the pool. Teardown is eventual (one reap
   interval), which the wire cannot observe. No teardown work kind, no controlplane
   provider, no leader election: N executors reap concurrently because `Reap` is
   idempotent, `Owned` is endpoint-local (each executor sees only its daemon/namespace —
   natural sharding), and a per-session advisory lock serializes the one pair that must
   not interleave: reap versus provision.
2. **`sandbox.Provider` grows two methods.** `Owned(ctx) ([]domain.ID, error)` lists the
   distinct session IDs of every container/Pod carrying the ownership label on this
   endpoint (gate containers included). `Reap(ctx, sessionID) error` destroys everything
   owned for that session — sandbox, Docker gate pair, anonymous volumes — revoking the
   session's gate token first (idempotent, the #197 ordering), needing no live handle, and
   succeeding as a no-op when nothing is owned. Docker gains one API-client endpoint:
   `GET /containers/json?all=1&filters={label}`. K8s gains a `List` with a label selector
   and the Role gains the `list` verb. Both extensions land in the shared contract suite,
   mutation-tested.
3. **Reap criteria, from the database only.**
   | session state | action |
   |---|---|
   | row gone (deleted) | reap; delete the session's checkpoint blob. *As built (slice-3 review hardening): requires the `deleted_sessions` tombstone (id + environment kind) the API delete writes in its deleting transaction — a missing row alone also describes a holding that was never this deployment's (another deployment or a test suite sharing the Docker daemon/K8s namespace), and those are skipped. All tiers are cloud-only as built: the TTL row's `kind = 'cloud'` exclusion applies to the terminal tiers too, a shared daemon making a self_hosted session's sandbox reachable but not the platform's* |
   | archived or terminated | reap; keep the checkpoint blob until the row is deleted |
   | running | never — structurally unreachable for the terminal tiers (slice 1's guards); for the TTL tier the advisory lock **plus D4's under-lock recheck** close the race |
   | idle past the TTL | checkpoint, then reap |
   The TTL tier additionally requires, in the same query: the session's environment is
   `kind = 'cloud'` (a worker's sandbox carries the same label and the wire-only worker
   cannot take the lock); **no queued/starting/active work item** for the session (a
   pending harvest or tool_exec must find the tree it was enqueued against); and **no
   unanswered confirmation ask** (HITL-idle is still mid-turn — the approved command must
   run in the context the human saw). `user.interrupt` does not reap — it is a steering
   primitive; an interrupted-then-abandoned session falls to the TTL like any other.
4. **Concurrency discipline.** One `pg_advisory_lock` keyed on the session id, held on a
   dedicated acquired connection: the reaper takes it for checkpoint+destroy, and
   `provisionSandbox` takes it around provision+restore. Each executor pins at most two
   connections (its serial work loop's provision, its reaper), bounded against the pool's
   default; the reaper processes sessions serially. A crashed holder's lock releases with
   its connection — safe, because the restore-consumption marker (D6) makes the
   half-finished state detectable rather than adoptable. The pre-lock criteria query is
   only a **candidate filter**: a classification is stale the moment it returns (a
   `user.message` can flip the session to running, and a provision can acquire and
   release this very lock, between classify and lock), so after acquiring a session's
   lock the reaper re-evaluates every reap predicate in one fresh query and proceeds only
   if the session is still eligible. The recheck is sufficient because every sandbox
   touch is preceded by state the recheck reads: a turn flips status to `running` and a
   tool claims a work item **before** its executor reaches the lock-taking provision
   path — so in-flight use either blocks the reaper's acquisition or is visible to the
   locked re-read, and work arriving after the recheck blocks on the lock and finds a
   fresh sandbox restored from the just-written checkpoint, losing nothing.
5. **Checkpoint scope is the agent's durable state, not one directory**: workdir +
   `/var/lib/map-shell/<session>` (bash cwd/env survive a resume) + `/mnt/session/outputs`
   (a resumed session's next grading harvest must not erase published deliverables).
   `/tmp` is explicitly not preserved. The `.materialized` sentinels are stripped from the
   archive at capture, so a restored workspace re-materializes skills and files instead of
   trusting a restored, agent-writable marker. Format: plain tar (the image contract
   guarantees only `tar`), gzipped executor-side for storage
   (`workspace/<sesn_id>/checkpoint.tar.gz`, overwrite-latest); spooled to an executor
   temp file first because `blob.Put` needs the exact size.
6. **Restore fires on a marker, never on "the container is fresh".** A fresh container
   exists for two reasons — a post-reap resume, and the routine cattle path of a container
   dying mid-turn — and only the first may restore: rewinding a crash-recreate to
   reap-time state would contradict the committed event log. Migration 0018 adds the
   marker (session id → blob key, taken_at, state `ready`/`consumed`). The TTL reap writes
   `ready` after the upload, inside its lock hold, before the destroy. Provision's create
   path, inside its lock hold, restores when the marker is `ready` and flips it `consumed`
   only after the extraction completes — so a crash mid-restore leaves `ready` standing,
   and the rule *a `ready` marker plus an existing container means a half-restored
   orphan: replace it* turns the half-restored tree from silently adoptable into
   detectably replaceable. A consumed marker is kept (not deleted) so the blob's
   provenance stays queryable; the next TTL reap overwrites both.
7. **Transfer paths.** Capture: Docker `GET …/archive` per root (streams, works on a
   stopped container); K8s one in-pod `tar -cf -` exec per root, stdout spooled. Restore:
   `WriteFileStream` the tar to a sandbox temp path, then one in-sandbox exec extracts it
   as the sandbox user — the k8s bulk-write route, preserving modes and symlinks, never
   Docker's daemon-side root untar and never the 0644-flattening write-tool family.
   Before restore ships anything in, the executor walks the spooled tar with `archive/tar`
   under the skills-extraction contract: member paths must be clean and relative with no
   `..`, types are restricted to regular files, directories, and symlinks (no devices, no
   setuid interpretation — mode bits are applied by the in-sandbox tar as the unprivileged
   sandbox user), and the decompressed budget is counted from actual bytes.
8. **Caps and degradations, all loud.** One knob each: `EXECUTOR_REAP_INTERVAL` (default
   60s), `EXECUTOR_SANDBOX_IDLE_TTL` (default 24h; 0 disables the TTL tier),
   `EXECUTOR_CHECKPOINT_MAX_BYTES` (default 2GiB, counted on the spooled tar). Over-cap:
   reap *without* checkpoint — an agent must not pin its sandbox immortal by filling the
   disk — logged and counted as its own metric outcome. A sandbox that cannot be read
   (K8s Failed pod; any exec/archive failure) degrades the same way. A blob-less
   deployment disables the TTL tier at startup with one log line; the terminal tiers run
   in every deployment shape (*as built, for cloud sessions only — see the criteria
   table*). Metrics: `sandbox.reaped` counter{reason} (*as built in slice 3:
   `sandbox.sessions.reaped` counter{tier}*), `sandbox.checkpoint` /
   `sandbox.restore` counters{outcome} + duration histograms, ids in logs never in labels.
9. **The wire guards land first, as their own slice.** Archive and delete of a `running`
   session are rejected 400 `invalid_request_error` (the reference documents the refusal;
   its exact status/message are unobserved — INFERRED divergence entries). No token work
   rides along: `gatetoken.Authenticate` already refuses an archived session's token
   (fail-closed), and delete cascades the rows. `deleteSession` later (slice 3) gains a
   best-effort delete of the session's checkpoint blob — controlplane already carries
   blob wiring, and a checkpoint whose container vanished with a dead daemon would
   otherwise orphan forever.
10. **What was evaluated and rejected**: a teardown work kind (FK cascade, above);
    Docker `AutoRemove`/self-exiting containers (PID 1 never exits; making it exit
    requires giving the zero-credential sandbox a credential; K8s has no analogue; the
    AutoRemove window races name-keyed idempotent provision); terminal
    `ErrSpecMismatch` (a rolling upgrade makes mismatch transient — the TTL tier heals
    the wedge instead); daemon-local named volumes / PVCs for continuity (single-daemon
    assets defeat horizontal scaling — the blob is the only shared substrate);
    create-time generation labels (they cannot close the restore gap D6's marker closes;
    multi-daemon affinity remains #28's, deferred with it).

## Out of scope

- **BYOC worker lifecycle.** The platform reaper is executor-only; the reference's own
  contract for self-hosted is "Managed by you". `Owned`/`Reap` land on the shared
  interface, so a worker-side sweep or `--max-idle` twin is buildable — filed as a
  follow-on issue, not built here.
- **Multi-daemon affinity / generation labels** — stays with #28, narrowed as above.
- **Warm pools** (#238) — explicitly sequenced after teardown by plan 20's decision.

## Slices (each lands as its own PR, TDD-first, `make verify` green)

1. **Wire guards.** `archiveSession`/`deleteSession` reject `running` with 400; one
   INFERRED DIVERGENCES entry (the reject status/message are ours); this plan lands
   `in-progress`; STATE.md picks up the tasks. Mutation-test both guards.
2. **`Owned`/`Reap`.** Provider interface + both backends + Docker list endpoint + Helm
   RBAC `list` verb + contract-suite rows (owned-after-provision, reap-removes-all,
   reap-idempotent, reap-unknown-noop, gated pair + token revoke on the backend-specific
   suites). No production caller yet — the suite is the consumer.
3. **The reaper, terminal tiers.** Executor goroutine: `Owned` → per-session criteria
   query → advisory lock → re-verify the criteria under the lock (D4) → `Reap`, for
   deleted/archived/terminated sessions;
   `EXECUTOR_REAP_INTERVAL`; the deleted tier's checkpoint-blob delete;
   `deleteSession`'s best-effort blob delete; metrics; compose/helm/README wiring.
4. **Checkpoint/restore engine.** Migration 0018 (marker table); capture (three roots,
   sentinel strip, spool, validate, gzip, `Put`) and restore (`ready`-marker gate,
   `WriteFileStream` + in-sandbox extract, `consumed` flip, half-restore replacement
   rule) behind the provision lock; the tar-walk validation contract; caps; no TTL yet —
   the engine's only trigger in this slice is its test suite.
5. **The TTL tier.** The idle criterion with its three exclusions (cloud-only, no pending
   work, no unanswered asks), `EXECUTOR_SANDBOX_IDLE_TTL`, blob-less disablement,
   degraded no-checkpoint paths, and the acceptance run below. The
   `session_checkpoints` row's cleanup owner was decided on slice 4's PR review and
   then corrected by the acceptance run: it dies in the API's **deleting transaction**
   (not the reaper's deleted tier as first assigned — a session whose sandbox the idle
   tier already reaped never reappears in `Owned`, so a reaper-owned row would linger
   forever). Archives this plan.

## Acceptance (compose stack + kind cluster, recorded in docs/HISTORY.md)

- Archive and delete each remove the session's container within one reap interval; a
  gated session's gate container and token go with it; a running session's archive/delete
  answer 400 until an interrupt lands.
- With a short TTL: an idle session's sandbox is checkpointed and reaped; a later
  user.message rebuilds it and the agent's earlier workspace file, bash cwd, *and* a
  previously harvested deliverable all survive — the deliverable stays downloadable
  through `GET /v1/files` after the next grading cycle.
- A session whose sandbox was killed mid-turn (docker rm) resumes on a fresh empty
  workspace — no stale restore (the D6 marker stays `consumed`).
- The #29 wedge heals: flip `EXECUTOR_IMAGE`, watch the mismatched session's sandbox
  reap after the TTL and the session complete on the new image.
- K8s: the same archive/TTL rows on a kind cluster prove the RBAC change and the exec
  capture path.
