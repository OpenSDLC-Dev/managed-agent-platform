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
is the backstop). No heading has been removed since — plan 34 re-checked, and removed none
from this file or from [docs/history/](./history/).

A later trimmer should know two things. **What is under a cited heading is generally
recorded nowhere else** — that is the standing reason not to delete body prose, and it is
why the 2026-07-18 prune stopped where it did. And **the heading is not the unit of
citation**: much of what points here quotes a sentence, a number or a rejected alternative
rather than naming a section, so preserving every title can still orphan the thing being
cited. The docker-deadline record in [docs/history/](./history/) is the worked example —
`internal/sandbox/docker/api_test.go` quotes one clause of it, and a heading-only check
sees nothing. A count is not given because it depends entirely on how a citation is
enumerated; the shape, not the number, is the argument. That is why plan 34 assessed this
file and cut nothing from it.

Older periods archive by month to [docs/history/](./history/) — so far
[2026-07](./history/2026-07.md) — with relative links re-based for the
new directory and in-repo citations re-pointed in the moving PR (plan
28). What remains below is the current period.

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

## Idle outputs harvest acceptance (plan 38, #263, run 2026-09-04) — ✅ passed

Verified against a docker-compose stack built from `feat/idle-outputs-harvest` @ cbc652b (the review-hardened revision — idle harvests attach to a live sandbox rather than provisioning one; the shipped tip adds three edge-case correctness fixes — transient-attach propagation, a corrupt-agent-state skip, and a grading-takeover requeue — that do not touch this harvested happy path), driving Anthropic's own `eval-driven-agent-development` workshop (`src/create-slides.ts`, `@anthropic-ai/sdk` 0.93.0, `ANTHROPIC_BASE_URL=http://localhost:8080`, the workshop agent on `claude-sonnet-4-6`), **without** the `HARVEST_VIA_OUTCOME` workaround.

`create-slides food` on a plain session (`sesn_jet01k8b2fnzn7k4tztf0mx4`): the agent wrote its deck under `/mnt/session/outputs/`, the session folded to `idle` with stop_reason `end_turn`, and **no `span.outcome_evaluation_*` or `user.define_outcome` event appeared**. `GET /v1/files?scope_id=<session>&limit=100` then listed `output.pptx` (37,253 bytes, `scope: {type: "session"}`, `downloadable: true`) — the harvest attached to the session's still-live sandbox, walked `/mnt/session/outputs/`, and published, with no outcome cycle. The same workshop against `main` returned `{"data":[]}` through its 10-retry backoff (issue #263's 2026-09-03 comment). An earlier run of the same case against the pre-revision base 0f0239c listed both `output.pptx` and `build_pptx.py`.

The existing outcome path is unchanged: the harness suites `TestOutcomeCloudSettlementChainsHarvest` (grading still chains a harvest, asserting `chain_grading=true`), `TestHarvestDiscardsWhenCycleSettled` (an interrupt mid-grading-harvest still discards) and `TestOutcomeInterruptDuringGrading` stay green.

Verifier subagent (pinned model) on cbc652b: PASS, `make verify` green at 90.12% total statement coverage, the three new guards (no-provision idle harvest, corrupt-state drain, delegation-bound harvest) mutation-checked. Dual review (Codex `gpt-5.6-sol`; Claude review as an Opus 5 Workflow agent): the revision answered five findings from the review of the base commit; the remaining notes were addressed before merge (see the PR). The two API-side idle folds (`user.interrupt`, last-running-thread archive) are out of scope and tracked by #586.

---

## Scheduled deployments (plan 37, #51) — archived 2026-09-01, all six slices delivered (#517, #519, #522, #524, #529, #531, #532, #535, #536)

A **deployment** binds an agent to an environment, credentials, resources and initial
events; `POST /run` fires it by hand, an optional 5-field POSIX cron schedule
(`internal/cron`, POSIX union semantics, DST literal wall-clock matching) fires it on the
clock, and every settled attempt is one persistent `drun_` run row. The plan's slices
landed as: the cron engine and migration `0031` (#517), the seven `/v1/deployments`
routes with the agent-archive refusal and the environment-delete message (#519, #522,
#524, slice 1), the `createSessionInTx` extraction (#529, slice 2), `POST /run` +
`sessions.deployment_id` + the durable `succeeded_at` (#531, slice 3 — migration `0032`,
#520's read-path half), the scheduler — 30s tick on every replica, DB clock, the
partial-unique-index occurrence claim, savepoint settlement, auto-pause on the fourteen
pausing types, one-hour bounded catch-up, three instruments and two spans (#532,
slice 4) — and the two run lists with #520's render half (#535, slice 5). This close-out
(#536) is slice 6. What the plan deliberately excluded is named in it and filed: webhooks
(#261), budgets (#432), jitter, an overlap brake, and the reference's 1,000-deployment
cap — the wire consequences all registered in DIVERGENCES.md (thirty-three plan-37
entries), with #78 tracking the recordings that would settle the inferences and #523 the
deferred agent-archive index.

---

## Memory stores (plan 36, #52) — archived 2026-08-26, all eight slices delivered (#482, #484, #485, #487, #489, #491, #494, #496)

Workspace-scoped collections of text documents, attached to a session through `resources[]`,
mounted at `/mnt/memory/<slug>`, read and written with the ordinary file tools, every write an
attributed immutable version, in sync across the sessions that share them — on both `cloud` and
`self_hosted`. The plan's nineteen design decisions and seven scope decisions landed across
eight PRs: the SDK bump to v1.66.0 (#482, slice 0), the `/v1/memory_stores` surface (#484,
slice 1), memories and versions (#485, slice 2), attachment and the `memory_store_id` filter
(#487, slice 3), cloud materialization + the three-phase run-end sync + the brain block (#489,
slice 4), the per-item sessions token (#491, slice 5), the BYOC worker's half (#494, slice 6),
and this close-out (slice 7). What the plan reserved but deliberately left out of scope, each
with its pointer: no `/v1/dreams` consolidation surface (scope decision 3), no 30-day version
pruning (scope decision 5), and a run-boundary sync cadence rather than the reference's
inside-the-loop 15 s (decision 11) — all in DIVERGENCES.md.

## BYOC memory — the `self_hosted` acceptance (plan 36 slice 6, #494)

The plan's `self_hosted` end-to-end acceptance is the real `ant beta:worker poll` (v1.26.1, SDK
v1.66.0) serving a session with a store against this server. The harness was built — `ant`
v1.26.1 from the pinned checkout and the four slice-6 server binaries compile clean in WSL, and
the model tier from `.env` is the one slice 4 used — but the full manual `self_hosted` live-model
stack (a throwaway Postgres and MinIO, `controlplane` on the local secrets cipher, `brain`
against the live model, an environment-key mint, an agent, a session, and the reference worker's
in-process runner) was judged disproportionate to the incremental evidence it would add over
what already covers the path, and the live transcript is left as a follow-up the built harness is
ready for. What substantiates the `self_hosted` path, all merged:

- `internal/worker/TestMemoryStoreServedThroughTheLeaseLoop` drives the exact wire path the real
  worker would: a real in-process control-plane API over HTTP against a real Postgres, the poll
  minting the `wtk_` token into the item's `secret`, the worker decoding it, landing the store in
  the sandbox with its marker and baseline, running the tool, and the run's end pushing what the
  agent wrote as a `session_actor` version (a create for the new path there; the head-sha
  precondition guards the update and delete routes the sibling wire tests exercise). The only
  things it does not use are the `ant` binary itself and a live model in a real container.
- The `cloud` real-model acceptance below (slice 4) exercises exactly those missing parts — the
  real `ant` CLI and a real model reading and writing a store mounted in a Docker sandbox —
  against the same `internal/memsync` machinery the worker shares.
- The verifier passed slice 6 twice, field-by-field against the reference worker's
  `lib/environments/memories.go` at v1.66.0, once with a scratch-copy mutation test that proved
  the shutdown flush's detached context by reverting it and watching the test fail.

Together these cover the acceptance's substance; the `ant beta:worker poll` transcript is the one
artifact the deferred live run would add, tracked as #495 with the harness ready for it.

## BYOC memory (plan 36 slice 6, #494) — review-hardening record

Two rounds past the verifier's first PASS, both judged against the reference worker
(anthropic-sdk-go v1.66.0 `lib/environments/memories.go`):

- **The Codex reviewer found two real defects.** *P1:* a control-plane stop cancels the run's
  context before the run-end sync and force-stops the item with no later run, so the agent's
  memory writes were permanently lost — and the BYOC worker has no reaper to catch them, unlike
  the cloud half. The reference always runs a bounded, `context.Background()`-based push-only
  `FlushWrites` on a non-clean end; the worker gained the twin (`flush`/`flushStore`, deferred so
  it runs on every exit, on a `context.WithoutCancel` context bounded to 30 s, inside the token's
  post-stop validity window). *P2:* the progress-driven heartbeat could trip `StallTimeout`
  during a large per-store sync that was still advancing; progress is now reported while the
  heads page and after each settlement action.
- **The verifier's second pass raised three non-blocking notes, all addressed.** The flush now
  continues past a per-file transport error rather than abandoning the store's remaining uploads
  (the reference's per-file-log-and-continue, the bounded context the only hard stop); the
  push-only test now deletes a store memory locally so its assertion pins push-only rather than
  leaning on a full sync never deleting a present, unchanged file; a materialize-progress
  assertion was added. A cosmetic log-ordering note — a spurious per-file warn when the bound,
  not the file, cut the pass off — was recorded and waived.
- **CodeRabbit found a docs gap:** ARCHITECTURE.md described the worker's memory sync as only the
  two run boundaries, so a reader would conclude a stopped run loses writes; the flush is now
  named there as the third sync path, and the two faulted-run mentions qualified to exclude it.

The flush is a fidelity gain, not a divergence — it makes the worker match the reference — so
DIVERGENCES.md gained nothing for it. The worker's two 400 carve-outs that *do* diverge (an
archive turns the rest of the sync pull-only; the occupancy cap is refused-but-not-remembered)
were registered when slice 6 landed.

## The sessions token — review-hardening record (plan 36 slice 5, #491, 2026-08-25)

No CLI acceptance of its own: the lane is reachable only through the test seam until slice 6
lifts the `self_hosted` attachment refusal, and the v1.66.0 reference worker's `HandleItem`
runs against the in-process server in `internal/api/worktokensdk_test.go` instead. The
verifier at 0ad97c5 (build, vet, fmt, 724 targeted runs green, three of four mutants red, the
matrix driven with curl against a built `controlplane`) read PASS with notes.

**Review round** (every finding verified against the source before acting):
- *The Claude reviewer (Opus 5) and the verifier* — the lane admitted the store's own read and
  its versions' list/get, and the comment and the registry attributed them to the worker,
  which calls neither (`git grep "Beta.MemoryStores"` at v1.66.0: five sites, all
  `.Memories.`). Narrowed to those five; a store's history and its actors stay the
  management key's to read.
- *Claude reviewer* — a session-wide interrupt (`CancelSession`: `stopped`, lease null) killed
  the token before the reference worker's post-stop flush (`Cleanup` → `FlushWrites`, 30 s on a
  context of its own), losing every edit since the last 15 s sync — and unlike `cloud`, whose
  held sandbox is synced at the next run's start, a BYOC workdir is removed at the item's end.
  Fixed as a join condition, not a column: a stopping or stopped item's token lives a minute
  past the stop's request. The Codex reviewer's reading of the same conditions — `stopping`
  keeping the whole matrix — is by design (the wind-down rides the token), now said so and
  pinned.
- *Claude reviewer* — registry and ARCHITECTURE defects, all reworded: "narrower than the key
  that polled it" (false on the memories), the SDK quote missing its "may be", the reversal
  record the rewrite dropped, and a tracker sentence claiming recording item 10 covers the
  environment key's status on a memory route, which the plan classes as the unrecordable
  kind. Also: the attachment probe's `ErrNoRows` on a session deleted since `Authenticate`
  (coalesced to the 404), `Cache-Control: no-store` on the poll as on the two console
  issuance routes, and the refusal loop widened (redact, versions, escaped paths).
- *Codex reviewer (gpt-5.6-sol, xhigh)* — two findings evaluated and rejected: an events
  stream open when the token dies runs to its end (true of every credential here — a stream
  authenticates at its open — and the holder polled with an environment key that reads every
  session's events in the environment anyway); a request authenticated at its open completes,
  its lock waits included — the policy for every credential here, so the docs say the token
  ends for every request after, not "at once".
- *Verifier* — the SDK test spent its whole 30 s deadline because no turn ends without a
  brain; an idle end_turn planted once the stream is up ends it through the runner's
  `MaxIdle` within seconds.
- *Claude reviewer, second pass* — the grace introduced one edge: the settlement of an
  abandoned wind-down (`FinalizeAbandoned`, a `stopping` item whose lease lapsed) stamps
  `stopped_at`, and the presumed-dead worker's token could run on into the minute in which
  the session is re-armed for another. First closed by a revoke in the settlement; then,
  when the Codex reviewer's third pass showed that settlement fires on a healthy flush too
  (the frozen lease lapses 15–30 s after a graceful stop, the flush runs to 45 s), by the
  queue itself: a wind-down is abandoned only once its lease lapsed *and* `queue.WindDown`
  has passed since the request — the one constant the token's window reads too — so no
  delete is needed. The heartbeat renewal that carries a token through a long run is
  pinned. The Codex rejections were judged sound on the code; `read_only` at the hands
  accepted as design.
- *Codex reviewer, second pass* — a grace counted from `stopped_at` left a graceful wind-down
  on its frozen lease (the worker learns of the stop half a TTL later — 15 s for a 30 s
  flush) and let a late settlement restart the clock; it is counted from
  `stop_requested_at` now, for a stopping or stopped item alike, 45 s of the 60 spoken for.
  Also: a `memories/` trailing slash the lane admitted to the mux's 404 is refused; the
  SDK test plants its idle event until the worker returns (its third pass: a failed plant
  ends the run at once and is reported first, and the wall-clock assertion it called
  flaky under load is gone — the run context's 30 s deadline is a deadline, not a
  wall-clock bound: the worker's deferred force-stop still gets a context of its own);
  the registry's reading (1) no longer overreads the SDK's "may be".
- *Codex reviewer, fourth pass; the verifier's final run* — the settlement race closed, the
  window's length pinned at 45 s alive and 61 s dead on both sides (a 30 s mutant had
  survived), `FinalizeAbandoned`'s own re-check of the window pinned, decision 15's text
  annotated as landed. One design point carried into slice 6 rather than settled here:
  the enqueue dedup (`WHERE state IN ('queued', 'starting', 'active')`) lets a send
  trigger enqueue a replacement exec item while one is `stopping`, so a second worker
  can run — and, once slice 6 lands, sync memory — while the first's token still flushes.
  Pre-existing for tool runs since plan 35, and for memory bounded by the sync's sha
  preconditions; whether `stopping` should bar replacements for `WindDown` is slice 6's
  call, with the reference worker's own semantics in view.

## Memory stores end to end — real `ant` CLI, real model, a `cloud` sandbox (plan 36 slice 4, run 2026-08-25) — ✅ passed

The plan's "End-to-end acceptance" names the cloud half slice 4 records: a store with a
passphrase at `/facts/secret.md`, a session attached `read_write` with `instructions`, a
`user.message` asking for the passphrase and for today's date at `/log/today.md`, the store and
its versions afterwards, a second session attached `read_only` whose write is refused, and the
filter listing both. Recorded with `ant` v1.26.1 against the branch's own `controlplane`,
`brain` and `executor` at b3e2f02 (#489), and re-recorded the same way at 492916a with the
same outcome (one log file written that time, `/log/today.md`, and the second session's
`write` refused with the reference's wording); the four code commits after 492916a changed
the content gate, the held-mount counter, the read phase's bound and the removal budget,
none of which this transcript exercises differently — three binaries in WSL on a
throwaway Postgres,
Docker sandboxes on `debian:stable-slim`, the model from `.env` (`MiniMax-M3` over the
Anthropic protocol). One CLI fact the first attempt taught: an agent created without
`--tool '{type: agent_toolset_20260401}'` has no tools, and this model then prints its
tool calls as prose in its own markup — the transcript kept is the second, tool-bearing run.

- `memory-stores create` → `memories create --path /facts/secret.md` → `sessions create
  --resource '{type: memory_store, …, access: read_write, instructions: "Read /facts before
  answering. Record what you learn under /log."}'` → the element renders with the seven keys.
- `sessions:events send` with the ask; the session is `idle` 25 s later. Its transcript:
  `bash ls /mnt/memory/project-notes/ && date` → `facts`; `read …/facts` → the tool's own
  "is not a regular file"; `bash ls -la` twice; `read /mnt/memory/project-notes/facts/secret.md`
  → `The secret passphrase is orchid-lantern-42.`; `write …/log/today.md` and `write
  …/log/index.md` → "wrote 89 bytes" / "wrote 131 bytes"; the final `agent.message` is exactly
  `orchid-lantern-42`.
- `memories list --view full` → three memories: `/facts/secret.md` unchanged, `/log/index.md` and
  `/log/today.md` with the bytes the agent wrote (the date it read, `Tuesday, August 25, 2026`).
  `memory-versions list --session-id <sesn>` → the two `created` versions, each
  `created_by: {type: session_actor, session_id: <sesn>}` — the run-end sync's push, attributed.
- The second session, `access: read_only`: `read …/log/today.md` returns the first session's
  content; its `write …/log/today.md` → `is_error: true`, "write: /mnt/memory/project-notes/log/today.md
  is inside read-only directory /mnt/memory/project-notes" (the re-record's wording; the
  b3e2f02 run said "read-only memory store directory"), and the store's
  `memories list` afterwards is unchanged. `sessions list --memory-store-id` → both sessions.

**Evals** (`RUN_EVALS=1`, the same model, in WSL at b3e2f02): `memory-recall` PASS (16.62 s —
the passphrase read from the mounted store) and `memory-write` PASS (4.01 s — the write the sync
then pushed, read back through the memories route by the `MemorySynced` grader); the pinned set
is nineteen.

**Gate**: `make verify` in WSL at cb899a3, the PR's last code commit — 54 packages `ok` and
one `FAIL`, `internal/mcp`'s `TestListToolsRefusesAResponseTooLargeToRead`, #380's
deterministic WSL2 failure in a package this branch does not touch (three isolated reruns
FAIL with #380's own message, and so does the same test at `origin/main` ca7c5e1 on the same
box; CI's `coverage` job passes it); `make cover-gate`: total statement coverage **90.45%**.
The new bash-backed `memsync` tests ran among the `ok` (their unreadable-directory case
skips itself under WSL's root and runs on CI's runner). The run needed
`GOFLAGS=-timeout=30m`: `internal/api` took 591 s and `internal/executor` 613 s on this
8-CPU box — the executor past `go test`'s 10-minute package default now, where CI's runners
still fit — and two earlier runs of the gate died at the default with a sub-second test
"running" (slower, not hung), each leaving the pgtest fixtures its timed-out binaries never
removed to slow the next; #490 holds the trend and the ways out. The runs at 5b436e3 and
492916a had 55 `ok`, 0 `FAIL` and 90.40% / 90.38%; the review round's commit 0269af9,
55 `ok`, 0 `FAIL` and 90.40%. The run at b3e2f02, before the round, had 54 `ok` and one `FAIL`: `internal/mcp`'s
`TestListToolsRefusesAResponseTooLargeToRead`, #380's open WSL2 flake in a package this branch
does not touch (three reruns went FAIL/FAIL/ok with #380's own message), and 90.37%. The gate,
the acceptance stack and the evals took the Docker daemon in turn, never together.

**Review round** — the Codex reviewer (gpt-5.6-sol, xhigh), the Claude reviewer (Opus 5, four
passes) and the verifier, all against cdd6540, the commit the acceptance above was recorded on;
the verifier's verdict was FAIL, on the first item. Fixed in the same PR:

- **Every sync push's head row named a version that was never written.** `pushMemory` minted a
  version id for the head row and `insertSessionVersion` minted another for the version row, so
  the wire's `memory_version_id` 404'd and a redact of the head never saw a head — the transcript
  above shows it (`/log/index.md`'s head `memver_57f7…` against its one version `memver_zwmg…`).
  Both reviewers and the verifier found it; the version row now carries the head's id, and
  `danglingHeads` pins zero.
- **A listing that failed read as deletions or as an empty tree.** The hash pipeline's status is
  `xargs`'s, so a `find` that could not enter a directory listed the rest and exited 0 (the files
  it hid then read as local deletions), and a `sha256sum` without `-z` (BusyBox's) listed nothing
  with exit 123 — the same shape as an absent directory, which a one-memory store would answer
  with a `DeleteRemote`. The command now guards absence itself (`[ -d ] || exit 0`), fails under
  `pipefail` when a stage does, and a non-zero exit skips the store — at materialization too,
  which had re-materialized over such a directory — and `memsync/shell_test.go` runs both
  commands under a real bash.
- **The wipe-guard rebuild never re-stamped the marker**, so an `rm -rf` of a mount left the
  directory pull-only for the sandbox's life with every later write withheld in silence; a
  rebuild re-stamps, as the reference's `stampAndPull` does, and the withheld pushes are counted.
- **Stores settled in mount order** — a slug of a name that can change between attachments — so
  two sessions could lock the same rows the opposite way round; store-id order now. And a store
  whose settlement failed failed the run's commit, re-running every tool for a memory the
  results never depended on; each store settles in a savepoint and a failure skips it.
- **The brain rendered any lookup error as "NOT AVAILABLE: the memory store no longer exists"**;
  a failed lookup renders the line unqualified, the repositories block's rule.
- **A store-side `/a/b → /a` change could never apply** (`rm -f` left `a/`, and the bulk writer
  refuses a directory target), and 2,000 removals of 1,024-byte paths were one `rm` past the
  shell's single-argument cap; removals prune their emptied parents, in bounded commands.
- **The cap**: a deletion in the same settlement made no room for a create, and the 2,001st
  memory's refusal was remembered by digest as though the bytes were at fault. Decision 11's
  text is amended: the store's fullness is its state, retried each run.
- The eval's bash write detector counted a `cat` of the path as a write, blaming the platform for
  a model miss; the read-only refusal wording is the reference's `is inside read-only directory`
  again (decision 12 promised it verbatim; the transcript above notes the earlier wording); the
  bulk shells' mode pattern admitted a setuid digit and their cleanup `rm -f` lacked `--`; a
  description's newline could forge a bullet in the brain's block; the executor's pool floor is 4
  (the reaper pins two connections mid-sync); a failed removal skipped the metrics.
- Docs: four citations named the CLI where the file is the SDK's `lib/environments/memories.go`;
  the marker's registry entry said "not a divergence" where the reference leaves an
  altered-marker directory unsynced and we pull into it (now a CONFIRMED entry of ours); the
  run-boundary entry misstated the reference's cadence (its worker syncs inside its tool loop,
  at most every 15 s); ARCHITECTURE and the fragment had the file tools guarding an
  altered-marker directory (they see `read_only` and archived only), "four" instruments where
  five land, and the worker using `memsync` "from slice 4"; the evals' timings here are the
  log's (16.62 s, 4.01 s) and the tool-call counts it does not hold are gone.

A second Codex pass on the fix diff and the verifier's re-run found six more, fixed in the same
PR: a local file at an ancestor or descendant of a memory the store created wedged every later
apply (the pull's batch failed on a file where a directory had to be) — the store wins, the file
is removed; the "any stderr skips the sync" rule would have skipped every sync on an image
whose locale warns on each command — `pipefail` judges the pipeline instead; a mount an agent
replaced with a symlink would have had listings and removals follow it — both commands
`cd -P` and refuse a path that resolves elsewhere; the brain's unresolved line repeated the
attachment's access as fact — it says the store's state could not be checked and may be
archived; the eval's detector judged the whole command, so a `2>/dev/null` made a `cat` a
write — it judges the segment naming the path; the wipe-guard test wiped the mount between
runs, where the next materialization repairs it before any sync — it wipes inside a run,
through the reaper's standalone sync, and goes red without the re-stamp; and a rolled-back
store's partial counts reached the sync span's attributes. The plan's slice-4 rows now say
which of them #488 holds. The PR's bot threads added two, fixed: a mount the sandbox already
held was reconciled only after the tools ran, so a store's change reached a session one run
later than decision 7 and the registry say — an existing mount is synced before the tools
too; and deletions settled in path order behind the creates that needed their room under the
cap or their path (`/a/b` gone, `/a` written) — deletions settle first. A third, that an
empty directory holding only an altered marker is re-materialized, is refuted: nothing
unvouched-for can be pushed from an empty directory, and landing the store there is the wipe
guard's own rebuild. The Claude reviewer's thread on the same push found one more, fixed: a
NUL byte in an agent-written file is valid UTF-8 that Postgres text refuses, so its push
failed the store's whole savepoint on every later run — `ValidateContent` refuses U+0000
(inert on the API lane, whose body check already does), so the file is remembered as refused
by digest and the rest of the store syncs; and the push outcome nothing returned is gone.
CodeRabbit's pass and a second Claude thread on the last commits added, fixed: the sync a
run opens with reported no progress to the lease keeper (the caller's callback now); the
removal command's budget measured the paths unquoted, where an apostrophe is four bytes
(the quoted token now); the read phase was bounded only by the listing, so a bash-planted
flood of files could be read whole into memory (at most a store's worth of changed files,
each read to the content cap, or the store is skipped); the eval's write detector counted a
failed `cp`; the brain's block said the sync ran after each tool call; the fake sandbox
spelled the marker and split removals on spaces; a shell test's table asserted one side;
the mount root was declared twice; and four doc sentences were qualified.

Refuted with evidence: a read-only mount "leaving bash modifications visible" — decision 12's
pull-only mode stops pushes and nothing claims local edits are reverted; the file-tool guard
"bypassable through symlinks" — lexical by design, `bash` being unconfined and the store
protected by the sync, which the guard's comment now says. Accepted and recorded: the sync
buffers a store's changed bytes in memory, bounded by the store's own caps and only for what
changed (a comment says so); a manifest's partial trailing record and a refusal keyed by a
non-UTF-8 path (re-read once a run) stay as they are. Owed and filed as #488: the docker
integration test under `SANDBOX_RUN_AS_USER` and the telemetry assertions the plan's
verification row named, which this slice did not deliver.

## Attachment and filter — real `ant` CLI against `resources[]` and the sessions filter (plan 36 slice 3, run 2026-08-25) — ✅ passed

The plan's slice-3 verification row asks for `ant beta:sessions create --resource '{type:
memory_store, …}'`, `beta:sessions:resources list` and `beta:sessions list --memory-store-id`,
recorded into HISTORY. Recorded with `ant` v1.26.1 (pinning anthropic-sdk-go v1.66.0) against the
branch's own `controlplane` on a throwaway Postgres at d2b25c7 (#487). Two CLI facts the transcript
needed: `--model` takes a mapping (`{id: …}`), not a bare id — the CLI's own error says "string
was used where mapping is expected" — while `--agent` accepts the id string or `{type: agent, id:
…}` (the transcript spells the latter); and a resource is one `--resource '{…}'` mapping per
element, or the `--resource.type`/`--resource.memory-store-id` inner flags for a single one.

- A create with two stores — one `read_only` with `instructions`, one bare — renders the two
  elements in request order with exactly the seven documented keys and no `id`: `access`
  `read_only`/`read_write`, `instructions` the string/`null`, `description` the store's/`""`,
  `mount_path` `/mnt/memory/user-preferences` and `/mnt/memory/notes`. `retrieve` and `resources
  list` echo them; `resources list --limit 1 --max-items -1` auto-pages across the memory element,
  neither repeating nor skipping one.
- `resources retrieve` and `resources delete` with the `memory_store_id` as the resource id → `404
  session resource … not found`.
- `memory-stores update --name "Renamed Preferences"`, then `retrieve` → the session still reads
  `User Preferences` and its mount path: the attach-time snapshot.
- A second session through the inner-flag form → 200 with the same element shape.
- `sessions list --memory-store-id` → the one session for the preferences store, both for the
  notes store, an empty page for a well-formed absent id, `400 memory_store_id must be a valid
  memory store id` for `memstore_X`.
- The refusals, each `400 invalid_request_error`: an absent store (`memory store … not found`),
  the archived one (`… is archived`), the same store twice (`… is attached more than once`),
  "Notes" beside "notes" (`… both mount at /mnt/memory/notes`), `access: append`, the
  `self_hosted` environment (`memory_store resources are not supported on self_hosted environments
  yet`), and a repository at `/mnt/memory/notes` (`mount_path … is reserved for memory stores`).
- `memory-stores delete` on the notes store, then `retrieve` → the element is kept as snapshotted,
  and `sessions list --memory-store-id` still returns both sessions.

**Review round** (every finding verified against the source before acting):

- *Claude, high* — the docs commit had swept in four Windows shell-cache binaries (~989 KB) under a
  literal `%SystemDrive%/` directory that a reviewer's sandbox left unexpanded at the repo root —
  #338's failure mode in a new spelling. The commit was rewritten without them and
  `/%SystemDrive%/` joins `.gitignore`'s root-output block.
- *Both reviewers* — the "no id, no timestamps" claim was asserted with `wantFields`, which
  tolerates extra keys, so adding an `id` to the element would have stayed green. `wantExactFields`
  now pins both elements' seven keys.
- *Codex* — the store row's `FOR SHARE` had no regression test. `TestSessionMemoryAttachLocksTheStoreRow`
  races an uncommitted archive and an uncommitted delete against the create, committing only once
  `pg_stat_activity` shows the create waiting on the row lock (the console-key test's technique);
  the mutant without the lock never waits, and the poll's timeout is how it fails.
- *Codex* — the fragment said the element is stored "as the reference renders it", crediting the
  reference with the `access` echo and the slug that are this platform's inferences; it now says
  "in the reference's response shape" and names the readings as its own. A registry sentence
  called the wrong two 400s parse-time (the duplicate and the ninth are; the collision is not).
- *Claude, nits taken* — `resourceID` was left dead by `resourceKey` and is gone; a `sesrsc_` id
  was minted for every element and discarded for memory ones, so it is minted only where used; a
  test comment described a cipher-less server the fixture never was; the brain test now seeds a
  repository beside the memory element, so a decoder tripped by the new shape fails the test
  rather than falling silent. Not taken: a repository may still mount at `/mnt`, an ancestor of
  the memory root — not new with this slice (file mounts under `/mnt/session/uploads` have the
  same shape), and slice 4, where a mount actually lands, is where to decide it.

**Gate**: `make verify` in WSL at 1081654 — the branch's last commit but for the docs-only one that
wrote this paragraph — 55 packages `ok`, nothing failed, total statement coverage **90.48%**; the
run at d2b25c7 before the review round read 90.44%. Each ran with no native suite sharing the
Docker daemon: the verifier, the reviewers and the gate took their turns in sequence.

## Memories and versions — real `ant` CLI against the memory routes (plan 36 slice 2, run 2026-08-25) — ✅ passed

The plan's slice-2 verification row asks for every `ant beta:memory-stores:memories` and
`:memory-versions` subcommand with each of its flags, recorded into HISTORY. Recorded with `ant`
v1.26.1 (pinning anthropic-sdk-go v1.66.0) against the branch's own `controlplane` on a throwaway
Postgres at 7795065 (#485), after the review round below. Two CLI facts the transcript needed:
Git Bash rewrites a `/notes/a.md` argument into a Windows path before `ant.exe` sees it
(`MSYS_NO_PATHCONV=1` stops it), and `--precondition` takes the whole object, `type` included.

- Four creates under `/notes/`, `/notes/deep/` and `/todo.md`; a fifth from a **piped YAML body**
  (`path: /piped.md`, `content: from stdin`); the creates' default projection renders `content`
  `null`; `retrieve --view full` serves it, and so does `retrieve … <memory-id>` in the **positional
  form** with no `--view` — retrieve defaults to `full`.
- `list` walks the five in byte-wise path order; `--path-prefix /notes/ --depth 1` returns
  `/notes/a.md`, `/notes/b.md` and one `memory_prefix` for `/notes/deep/`; `--limit 1 --max-items -1
  --view full` auto-pages through the path-keyed cursor, none repeated.
- Creates at `/notes` and at `/notes/a.md/x` → `409 memory_path_conflict_error`, each naming
  `/notes/a.md`'s memory; `notes/no-slash` → `400 path must start with "/"`.
- `update --content alpha2 --precondition '{type: content_sha256, content_sha256: <sha(alpha)>}'` →
  200 and a `modified` version; the same precondition again → `409 memory_precondition_failed_error`
  naming both digests; `--content alpha2` once more → 200 with no new version; `--path /notes/a2.md`
  → a `modified` rename; a flagless update → `400 update requires content or path`.
- `memory-versions list --memory-id` → `created`, `modified`, `modified`, newest first;
  `--session-id sesn_…` → an empty page (no session writes yet); `--operation modified --view full
  --limit 1` → the head with its content; `retrieve --view full` on the `created` version → `alpha`.
- `redact` on the head → `400 … is the current head … and cannot be redacted`; on the `created`
  version → 200 with `path`, `content`, `content_sha256`, `content_size_bytes` null and
  `redacted_at`/`redacted_by` set.
- `delete --expected-content-sha256 <wrong>` → the same 409; with the right digest →
  `memory_deleted`; `retrieve` → 404; the lineage now ends in a `deleted` row carrying the path.
- `memory-stores archive`, then a create → `400 … is archived`; `redact` on `/todo.md`'s head → 200,
  and `retrieve --view full` reads `content: ""`; `memory-stores delete` → the versions list 404s.

**Review round** (every finding verified against the source before acting):

- *Codex, high* — Go's JSON decoder rewrites an invalid UTF-8 byte to U+FFFD before any validator
  runs, so `{"content":"<0xff>"}` was accepted and stored altered while the registry claimed a
  400. Fixed at the one chokepoint every JSON object body passes through, `decodeObject`: a body
  that is not valid UTF-8 is a 400 before parsing, on every route that decodes a JSON object (the
  work-stop body's one bool is read outside it and stores nothing; its own `fixed` fragment);
  raw-body tests prove no memory or version row is written.
- *Both reviewers, the verifier* — the memory-versions list ignored the `view=full` cap of 20 the
  SDK documents for both resources. Fixed with the memories list's clamp; tested at 25 versions.
- *Claude* — the `service_account_id` filter was ignored, so an audit client filtering by a service
  account got the whole history. Now a filter lane like `api_key_id`'s, answering the empty page a
  platform that never writes that actor must answer; the registry entry says so.
- *Claude, the verifier* — three readings unregistered: the type-only precondition's 400, the
  2,001st memory's 400, explicit nulls on memory bodies. Three INFERRED entries under #78.
- *Codex* — three INFERRED entries cited recording items that do not exercise their claim (invalid
  UTF-8 under item 9, a slash-less `path_prefix` under 13, a repeated redaction under 6/15). Each
  parenthetical now names what the item lacks.
- *Claude, nits taken* — a lineage index on `(memory_store_id, memory_id, created_at DESC, id
  DESC)` while 0029 is still unmerged (the filter runs over a history #476 never prunes); two
  citations corrected; the ARCHITECTURE row says `internal/api` is `memsync`'s only importer until
  slice 4; a comment stops claiming the collation test pins the handler's clause (the verifier
  showed the clause cannot go red on the musl fixture — the column's collation is what the test
  pins). Not taken: selecting `content` under `view=basic` only to discard it — a cost, not a
  defect, left for a later pass.
- *CodeRabbit, five threads, all taken* — `memsync.ValidatePath` accepted invalid UTF-8 (an
  invalid byte ranges as U+FFFD, which neither the Cc/Cf rule nor NFC catches; inert on the API
  lane behind `decodeObject`, load-bearing for slice 4's sync lane, so a rule of its own now);
  `getMemory` under an unknown store answered the memory's 404 where every sibling route answers
  the store's (the test now pins the message on all eight routes); `writeError` asserted error
  types where the rest of `internal/api` uses `errors.As` (its claim that a lint gate fails was
  wrong — this repo runs none — the style point stood); a `CHECK` on `memory_versions.operation`,
  as on every enum column in the schema; `slices.Equal` for a hand-rolled helper.
- *The implementer's seven declared deviations*, judged by both reviewers and the verifier: refusing
  a non-NFC path rather than normalizing it (the plan's own rejection table); handlers before tests,
  discharged by mutation probes; a separate `apiErrorWithFields` type; a slash-less `path_prefix`
  matched literally (registered); a repeated redaction returning the unchanged row (registered); the
  type-only precondition refused (now registered); `created_by` rendering null when absent — not a
  deviation, the spec says "null when no writer is recorded".

**Gate**: `make verify` in WSL at 919ace5 — the branch's last commit but for the one that wrote
this paragraph — 55 packages `ok`, nothing failed, total statement coverage **90.43%**. The run
before it, at 7795065, lost one assertion to a timing race in a package the branch does not touch:
`internal/queue`'s `TestEnqueueNotifiesWorkChannelOnCommit` ("woken before the enqueue committed"),
green when re-run alone and filed as #486. An earlier run at 6ca82b0 timed out `internal/executor`
at Go's ten-minute limit while a reviewer's native test run shared the Docker daemon — the same
contention slice 1 recorded, and the reason the verifier and the gate now run in sequence.

## Memory stores — real `ant` CLI against `/v1/memory_stores` (plan 36 slice 1, run 2026-08-25) — ✅ passed

The plan's slice-1 verification row asks that `ant beta:memory-stores create|retrieve|update|list|
archive|delete` work against this platform. Recorded with the real `ant` CLI v1.26.1 (which pins
anthropic-sdk-go v1.66.0) against the branch's own `controlplane` on a throwaway Postgres, at
34c908e (#484), after the reviewers' round below (the later CodeRabbit refinement, 3b899d7, changed
nothing the transcript exercises). Every command below is the CLI's own spelling — the id
travels as `--memory-store-id`, `--metadata` takes a YAML mapping, `--include-archived` is a bare flag.

- `create --name "User preferences" --description "What the user likes" --metadata '{team: infra,
  tier: gold}'` → the eight-field `memory_store` object, `archived_at: null`, `updated_at ==
  created_at`; `retrieve` → the same body.
- `update --description "" --metadata '{tier: null, env: prod}'` → `description: ""`, metadata
  `{env: prod, team: infra}` (the null deleted `tier`, the omitted `team` stayed), `updated_at`
  advanced, `created_at` unchanged.
- `list --limit 1 --max-items -1` → the CLI auto-paged four stores newest-first through this
  platform's `k1|` cursor, one per page, none repeated.
- `archive` → `archived_at` set and **`updated_at` unchanged** (the spec's definition of the field;
  the first commit bumped it, the review round removed the bump); `archive` again → the same
  `archived_at`; `update --name Renamed` → `400 memory store … is archived`; `list` omits the store,
  `list --include-archived` and `retrieve` serve it.
- `delete` → `{type: memory_store_deleted, id}`; `retrieve` → `404 not_found_error`.
- A 256-character `--name` → `400 name cannot exceed 255 characters`; `list --limit 101` → `400 limit
  must be an integer between 1 and 100`.

**Review round** (the reviewers' findings, all verified against the source before acting):

- *Both reviewers, the verifier* — archive advanced `updated_at`, which the OpenAPI spec defines as
  "when the store's name, description, or metadata was last modified". The vault archive it copied
  has no such definition to honor. Fixed: archive touches only `archived_at`, and an update that
  changes none of the three fields (an empty body, a null bag, the stored values sent back) skips
  the write — asserted on create, after a real update, after archive and across no-op updates.
- *Codex* — `validateMetadataCaps` checked only the upper bound of the documented 1–64 key range, on
  every resource with a metadata bag. Fixed; pinned on create and on patch. *CodeRabbit* then made
  the fix precise: a bound on the merged map would have left any row that acquired an empty key
  before the check stuck, every patch answering 400 until the caller guessed `{"": null}`. The lower
  bound is therefore enforced on what a caller sends — a create's bag, a patch's upserts — and a
  planted legacy key is asserted patchable and deletable, with no cleanup migration needed.
- *Codex* — the paging test never forced tied timestamps, so `created_at < $t` alone would have
  passed. Now a 21-row walk with every `created_at` equal, plus the default limit of 20 and
  `limit=100`.
- *Both reviewers* — the two "404" ids contained letters outside the id alphabet, so `checkID`
  refused them before the prefix or the row lookup was consulted. Now a well-formed unknown id
  reaches `ErrNoRows`, and a NUL-byte id proves `checkID` runs (Postgres would 500 on the byte).
- *Codex, refuted* — "create must 400 on `description: null` / `metadata: null`, the spec types them
  non-nullable". The spec types what the SDK sends, and the SDK never sends null; this platform reads
  a null string or bag as the unset value on every create route (`stringField`, `parseMetadata` —
  agents, vaults and sessions alike), and a memory-store-only 400 would diverge from that rule, not
  from the reference, whose answer to an explicit null is unrecorded.
- *Verifier* — the sessions-list filter entry still called memory stores "unimplemented", README's
  reserved-seams line still listed them, and the fragment counted an amended entry as new. All three
  corrected.

**Gate**: `make verify` in WSL at a831c26 — 54 packages `ok`, 0 `FAIL`, total statement coverage
90.39%; on the reviewed code (bdf9a10, the same tree as the merge) — 54 `ok`, 0 `FAIL`, **90.43%**.
A run in between, at 34c908e, failed only on `internal/mcp`'s `TestListToolsRefusesAResponseTooLargeToRead`,
the open WSL-only flake #380, which this branch does not touch; another timed out `internal/api` and
`internal/executor` at Go's ten-minute package limit because the verifier's native run of the same
suites shared the Docker daemon — the uncontended re-run above is the one that counts.

---

## anthropic-sdk-go v1.66.0 bump — wire-schema verification record (2026-08-25, plan 36 slice 0)

The fifth bump record. Unlike v1.63.1's, whose managed-agents content was all schema this platform
does not build, this range's weight is **behavior**: v1.66.0 is the release that gives the reference
worker a memory-store engine, and plan 36 slices 5 and 6 are bound by it. A plan citing a checkout
the repo refuses to read is the doc/code lag the verifier exists to catch, so the pin moves before
the plan's first line of memory code. Three releases in the range, all three of v1.64.0/65/66 landing
inside nine hours on 2026-08-19; **82 files changed, +35,084/−16,789**; endpoint count **131 → 144**
(`.stats.yml`), the spec and config hashes both moved, and the SDK's own `go.mod`/`go.sum` are
byte-identical between the tags, so the bump drags in no new module.

**What the range contains.** The thirteen added endpoints are the **GA (non-`?beta=true`) Files and
Skills routes** v1.65.0 promoted — `POST|GET /v1/files`, `GET /v1/files/{id}[/content]`, `DELETE
/v1/files/{id}`, `POST|GET /v1/skills`, `GET|DELETE /v1/skills/{id}`, `POST|GET
/v1/skills/{id}/versions`, `GET|DELETE /v1/skills/{id}/versions/{version}`. No managed-agents route
was added or removed, and the promotion is routing-neutral here: `internal/api/server.go` already
registers both families on the bare path, and a Go mux pattern carries no query. The three changes
that matter to this repo:

- **The built-in tool config became a union** (v1.66.0). `BetaManagedAgentsAgentToolConfig`, a single
  struct discriminated by an eight-value `name` enum, is replaced by
  `BetaManagedAgentsAgentToolConfigUnion` with eight variants, one per tool, and the params side by
  `…ParamsUnion` with eight `Of…` arms. Each variant carries **`type` beside `name`**, and the
  union's `AsAny()` dispatches on `type`, not `name`. The two web variants additionally carry
  `allowed_domains`, `blocked_domains`, and `max_content_tokens` (web_fetch) / `user_location`
  (web_search), with a documented grammar: plain hostnames, at most 64 entries, an empty list
  rejected, the two lists mutually exclusive.
- **The reference worker learned memory** (v1.66.0). `lib/environments/memories.go` does not exist at
  v1.63.1 and is 1,150 lines at v1.66.0 — the download/sync engine, its marker file, its merge rules
  and its bounds. `lib/environments/worker.go` grew 526 → 811 lines across the range and now
  **decodes `work.secret`**: URL-safe base64, padded or not, of a JSON object whose `sessions_token` becomes the
  Bearer for the item's per-session calls; a session with stores and no token **fails the item**.
  `tools/agenttoolset` gained `AllowedRoots`/`ReadOnlyRoots` and turned `UnrestrictedPaths` into a
  hard error.
- **A fourth actor** (v1.64.0). `service_account_actor {service_account_id}` joins the memory-version
  `created_by`/`redacted_by` union, with a matching `service_account_id` list filter.

**The enumeration.** Every SDK file defining a shape this repo mirrors, `git diff`ed pairwise between
the tags. **Byte-identical:** `betasessionresource.go`, `betamemorystore.go`,
`betamemorystorememory.go`, `betaenvironmentwork.go`, `betasessionevent.go`, `betasessionthread.go`,
`betasessionthreadevent.go`, `betaskill.go`, `betaskillversion.go`, `betavault.go`,
`betavaultcredential.go`, `betadeployment.go`, `betadeploymentrun.go`, `betaagentversion.go`,
`betawebhook.go`, `betatunnel.go`, `betatunnelcertificate.go`, `betasessionutil.go`, `betaparse.go`,
`betamessagebatch.go`, `betamodel.go`, `aliases.go`, `field.go`, `config/**`, `mcp/**`, `option/**`,
`toolrunner/**`. There is **no `betaoutcome.go`** at either tag — outcome types live in
`betasession.go` and `betasessionevent.go`. Changed, each resolved:

- *`betaagent.go` (3,155 → 4,774 lines, +2,373/−754)* — the union split above. Verified byte-for-byte
  unchanged inside it: `BetaManagedAgentsAgentToolsetDefaultConfig`, `BetaManagedAgentsMCPToolConfig`,
  `BetaManagedAgentsCustomSkill`, and the six tool-input payload types, which only moved.
  `BetaAgentNewParams`/`BetaAgentUpdateParams` gained **no field** — their only diff is a doc example
  and the two union renames.
- *`betasession.go` (16 changed lines, **zero line movement**)* — the `…Union` renames propagated into
  the `Configs` union comments, plus one model doc example. Every `betasession.go` citation in the
  repo survives unmoved, which is why the six stale ones found below are a pre-existing defect rather
  than this bump's.
- *`betafile.go` (net zero, reordered)* — v1.65.0's GA split: `FileMetadata` → **`BetaFileMetadata`**,
  `DeletedFile` → `BetaDeletedFile`, `DeletedFileType` → `BetaDeletedFileType`, and `BetaFileScope`
  moved down. The five `BetaFileService` methods now return the `Beta*` types.
- *`betamemorystorememoryversion.go` (434 → 470)* — the fourth actor and its filter, in v1.64.0. The
  `BetaManagedAgentsMemoryVersion` shape itself is otherwise unchanged.
- *`betaenvironment.go` (+1)* — two doc-comment edits that **newly state** description semantics:
  "null when unset" on the response, and "Omit to preserve; null clears to null; an empty string is
  stored as an empty string" on update. Checked against `internal/api/environments.go` and registered
  as a divergence: this platform's column is non-null `text`, so an unset description is `""` and a
  null clears to `""`.
- *`betasessiontoolrunner.go` (+36)* and *`betatoolrunner.go` (+5)* — v1.64.0 behavior: the send
  retry-window rewrite (retries for the live lease TTL rather than three attempts, carried on the ctx
  by the new `internal/sendwindow`), `adoptContainer`, the `mid_conv_system` arm dropped, and
  `NextMessage` no longer yielding the final message twice. Nothing on the wire; the platform's own
  `internal/worker/lease.go` is the analogue worth comparing when #383's successors come up.
- *`lib/environments/poller.go` (+53)* — `AutoStop` and an idle-log throttle. Client-side only.
- *`betamessage.go` (+3,317)* and *`message.go` (+3,679)* — v1.65.0's computer and browser toolsets,
  entirely Messages-API. 95 exported types added to the former and 103 to the latter; the only
  removals in either are the `mid_conv_system` block param and its projections.
  **`BetaStopReason` is unchanged** — the same eight constants at both tags, diffed identical.
  No managed-agents surface.
- *`shared/constant/constants.go` (+50 lines, 17 constants added, 1 removed)* — `−MidConvSystem`,
  `+ServiceAccountActor` (v1.64.0), eleven browser/computer/skill constants (v1.65.0), and the five
  per-tool `name`/`type` constants `Edit`/`Glob`/`Grep`/`Read`/`Write` (v1.66.0). No
  `knownPrefixes`-relevant constant moved.
- *`internal/apierror`, `internal/requestconfig`, `packages/ssestream`* — a new `Error.WorkspaceID`
  read from an **`anthropic-workspace-id`** response header on both the unary and SSE error paths.
  A header the reference emits and this platform does not; registered.
- *`api.md` (1,295 → 1,482)* — the GA rows. Everything below the Messages type index shifts ~+186.

**Citation durability.** `docs/DIVERGENCES.md` carries **85 SDK line citations: 59 hold at v1.66.0,
25 shifted, 1 gone as cited** (the `api.md` Stop-Work range the work/stop entry already flagged as its
stale v1.58.0 citation, whose drift chain now reads 656-673 → 683 → 693 → 698 → 884). Every shift is
mechanical — insertions above the cited line — and each was re-read at its new number before the
registry was changed. Two citations were broken by **content**, not by line, and those are the
findings that mattered: the web-domains inference, whose whole premise ("configured by no wire field")
v1.66.0 falsifies, and the toolset-echo entry's "byte-identical to v1.61.0's — the inter-pin diff
touches no toolset type", which the union split ended. A third, the accepted-key-set entry, cited a
type that no longer exists. The other 57 citations in the repo: 38 hold, 5 shifted (the five in plan
36's own Ground truth, re-numbered with it), 2 changed by content (`internal/api/files.go`'s two
`betafile.go` citations — the `FileMetadata` type renamed and `BetaFileScope` moved — corrected), 6
were deliberately anchored at v1.66.0 by the plan and
become valid the moment this lands, and **6 were already wrong before this bump** — `betasession.go`
citations in `internal/api/sessionresources.go` comments that drifted +86 when plan 35 moved the pin
to v1.63.1 and were missed because that record's audit covered the registry, not code comments. Since
`betasession.go` did not move in this range, v1.66.0 neither worsened nor fixed them; they are
corrected here.

**The labels.** 93 literal `v1.63.1` matches (`git grep -o` at the merge base) across 21 files: **45 live** — 36 in the registry's
evidence clauses, plus `go.mod`, two `go.sum` lines, `docs/REFERENCE_PROJECTS.md`,
`.claude/agents/verifier.md` and four code comments — all advanced; **48 historical**, all kept (plan
36's and archived plan 35's own from-version statements, the v1.63.1 bump record above, three
changelog fragments, the registry's own bump chronicle, and the two "since v1.63.1" comments in
`internal/events`). Six were **ambiguous**, and each got a decision rather than a coin flip. Five bare
`(v1.63.1)` provenance tags on the `(no output)` placeholder — in `toolset.go`, `executor.go`,
`executor_test.go`, `toolexec.go` and `toolexec_test.go` — are rewritten to the "since v1.63.1" form
their two siblings in `internal/events/inbound.go` already use, so they stop drifting at every bump;
the behavior still holds at v1.66.0, so either reading was factually safe and the one that cannot rot
was taken. The sixth, the registry's "unchanged through v1.63.1" durability clause on
`betasessionevent.go`, is advanced to v1.66.0 and strengthened: the file's diff across the whole range
is not merely comment-only, it is empty.

**One line of repo code changed.** 33 distinct root-package SDK identifiers (48 counting the option, param, respjson and ssestream qualifiers) are used across eleven files;
All but one are present at v1.66.0 unchanged. The one break is `acceptance/dcf_test.go:54`'s
`[]anthropic.FileMetadata`. The mechanism is worth recording because it is the opposite of what a
rename usually does: `anthropic.FileMetadata` **still compiles** at v1.66.0 — it is now the *GA* Files
type, a different struct with `ExpiresAt` and no `Scope` — so the declaration is fine and the compiler
reports the failure a screen below, at the `Beta.Files.List` append, where the pager now yields
`BetaFileMetadata`. Every downstream field read is identical between the two structs, so the fix is
the declaration alone. The hazard: a blind rename is right *here* and would be silently wrong anywhere
else the old name appears. There is exactly one occurrence in the repo.

**One behavior change rides with the bump**, and it is wire-visible. `rejectConfigKeys` accepted
`{enabled, permission_policy}` plus `name` on a `configs[]` entry, so a v1.66.0-shaped `tools` — which
carries `type` — got a 400 at agent create/update, and again on every stored spec at session create.
Slice 0 accepts `type` **when it equals `name`** (a mismatch stays a 400, INFERRED: nothing states what
the reference does when the two disagree) and renders `type` on the resolved echo unconditionally,
request or no request, because the response union has no other discriminator to dispatch on. The web
tools' four new keys stay **refused**: the fence here is operator-side (`WEBTOOL_ALLOWED_DOMAINS`), and
a silently dropped `allowed_domains` would read to its author as a fence in force. That refusal is
registered CONFIRMED against **#481**, which is where honoring the wire fields will happen; the
registry's "configured by no wire field" inference is closed in place, rewritten rather than deleted so
the reversal stays auditable. A fourth entry records `service_account_actor` as an arm this platform
will **never** emit — there is no service-account identity here and no `svac_` prefix — so the
memory-version actor union will render three of its four arms when plan 36 slice 2 writes the rows.

**Evidence.**

- The read-only enumeration ran as four parallel investigations (registry citation durability, 85;
  non-registry citation durability, 57; the `v1.63.1` label classification, 93 matches; the compiled
  SDK identifier surface, 48 qualified identifiers) plus a direct per-file diff of every changed SDK file, with
  the overlaps re-derived independently. No contradiction survived reconciliation. Every SDK fact
  above was read at a tag with `git show <tag>:<file>`; nothing was checked out.
- The real `ant` CLI (v1.26.1, which pins SDK v1.66.0; built from the read-only checkout) against
  this branch's controlplane, run natively in WSL on a throwaway Postgres: `beta:agents create
  --model.id claude-opus-5 --tool '{"type":"agent_toolset_20260401","configs":[{"name":"bash",
  "type":"bash",…},{"name":"web_fetch","type":"web_fetch",…},{"name":"grep","enabled":false}]}'`
  → 200, every built-in entry rendered with `"type"` equal to its name, the `grep` entry that was
  sent without one included, and `beta:agents retrieve` echoing the same; `"type":"read"` on the
  `bash` entry → 400 `configs[0].type is "read" but must equal name "bash"`; `"allowed_domains"`
  on `web_fetch` → 400 `unknown field "allowed_domains" in configs[0]` (2026-08-25).
- `make verify` on the bumped pin, in the WSL Ubuntu clone (Docker and the local kind cluster
  present, so the store, API and both sandbox suites ran rather than skipped): build, crossbuild,
  vet and fmt-check clean, 54 packages `ok`, no failure, **total statement coverage 90.42%**
  against the 90% gate (2026-08-25, branch `feat/plan-36-slice-0-sdk-bump` at 24de645); re-run on
  the reviewed final commit 3c50fc5 after the review fixes: 54 `ok`, no failure, 90.45%.

## The session delegation bound (#447) — the designs it beat, 2026-08-23

The narrative is in CHANGELOG.md and the wire argument is in DIVERGENCES.md. What neither holds
is which designs were considered and why three were dropped — and one of the rejections is the
reason the shipped design needed a migration at all. Five rejections are recorded below, and
they are not all the same kind of thing: the first three are the designs proper, alternatives
for where the count lives and where the cut falls, of the four evaluated. The last two are a
counting rule and a naming decision, which arrived attached to those designs and are kept here
because each was argued and each could be argued back.

The issue's triage called for a plan file, and this section is what stands in for it. A plan
would have decomposed work that turned out to be one migration and one refusal; what actually
needed writing down was the alternatives — and a plan file archives on delivery, which is
exactly when this becomes worth reading.

**The issue undercounted the problem: there were three escape routes, not two.** #447's body
names the fresh work-item row (route 1) and the `woke` clearing (route 2). Grounding the design
found a third: `chainInput` matches a peer's `agent.thread_message_received` past the watermark
**regardless of `processed_at`**, and input beats the cap. So two threads that both stay
perpetually `running` never wake each other (`WakeThread` only flips `idle` ones), never insert a
fresh row, never set `woke` — and still reset the count every single turn. A fix closing only
routes 1 and 2 would have left the loop alive. All three are one mistake seen from three angles:
at the layer where `chainInput` and `WakeThread` work, a peer agent's message and a client's
message are the same event class.

**Rejected: the baton — carry the chain count across the wake instead of rehoming it.** The
elegant option, and the only one needing no migration: `EnqueueThread` would copy the outgoing
count into the new row's `metadata` rather than letting it take the `DEFAULT '{}'`. Two leaks
killed it, both outside the delegation path and so easy to miss. `Brain.settle` — the path every
*failed* turn and every tool-less turn takes — calls plain `queue.Requeue`, which clears the key,
so a ping-pong pair on a flaky provider resets its baton on every failed turn and never reaches
the cap. And `queue.CancelThread` stops the row outright, so an operator interrupting one thread
would hand the pair a fresh budget. A counter that any unrelated path can zero is not a bound.

**Rejected: derive the count from the log — how many of this thread's last N turns were
settlement-only.** No migration at all and the cleanest crash story, since it re-reads rather
than projects. It failed on two counts. Its reset markers include any real tool call by any
thread, so one unrelated child running `bash` in a poll loop masks a looping pair indefinitely —
a bound that a busy sibling switches off. And it transcribes `EventType.Inbound()` and the six
delegation tool names into hand-written SQL matched on `payload->>'name'`, where a seventh
delegation tool, or a change to `turnEvents`' payload keys, silently converts every delegation
call into a reset marker and the bound stops existing **with no test failing**.

**Rejected: cut at the settlement rather than refusing at the claim.** Two of the four candidates
did this, and it is where the sharpest defect was. On a *mixed* turn — one holding both a
delegation call and a real tool call — the fate block's `gated`, `idle`, `capped` and `chain` are
all false, so an unguarded cut fires, the `case idle:` arm idles the thread, and `opts.Then`
still enqueues the `tool_exec`: a sandbox command left in flight against a session that has
already folded idle and become archivable, with `events.ResumableThreads` selecting only
`running` threads so nothing would ever read the result. Both settlement-side candidates also had
to gate `delegate.wake` at the cap so a cut thread would not be re-woken — and that gate makes
`send_to_agent` answer its byte-pinned `Message sent.` for a message that will schedule nothing,
the exact lie it already refuses to tell for a target idle on `retries_exhausted`. Refusing at
the claim needs neither: it never intervenes in a turn's fate, and the peer really is woken.

**Rejected: count only settlement-only turns.** Two candidates did. `commitTurn` computes
`settlementOnly` as `len(delegated) == len(turn.toolUses)`, so a model that puts one trivial tool
call in the same turn as each message would never move the counter at all — while spending
exactly as much. Counting every turn that reaches `commitDelegatedTurn` closes it, and rests on
an invariant checkable in one sentence: the six delegation tools are the only channel by which
one thread reaches another, so continuing an agent-to-agent loop *requires* a delegation call.

**Rejected: borrow the reference's `budget_reached` stop reason** (the user's decision,
2026-08-23, after the evidence below). It would have given operators a first-class,
machine-readable stop reason for one constant and one `stopRank` entry. Against that: the SDK
defines `budget_reached` as "the session's tracked list cost reached its budget" and instructs
the client to "raise the budget to continue" — a recovery that does not exist here, where
`budget` renders `null` and a POST of one is a 400. Emitting it would hand a reference-compatible
client a name it knows attached to a quantity we do not measure and a remedy it cannot perform,
and would spend the rank #432 will want for the real thing. A coined
`session_delegation_exhausted_error` on a plain `end_turn` is #442's precedent, one level up.

**The reference negative, established rather than assumed.** Four independent sweeps — the pinned
SDK at v1.63.1, the `ant` CLI, the public docs, and an exhaustive census of every published
numeric limit rather than a search for guessed field names — agree the reference publishes no
bound of any kind on agent-to-agent activity. Three findings make that trustworthy: `max_uses`
existed on the toolset and was *deliberately removed* for managed agents ("the roster entry has
no `max_uses`, `max_tokens`, or `caching` fields"); the idle stop-reason union is closed at four
values, none an activity-exhaustion state; and the message events carry no counter of any kind.
One correction the sweeps produced against an earlier reading: the reference's list cost is not
purely token-denominated — it prices "Session running time, at $0.08 per hour" — so `budget`
carries a weak wall-clock denominator. It still counts no exchanges. A methodological caveat
worth keeping: the 25-concurrent-thread cap is published in the docs and appears **nowhere** in
the typed schema, so SDK absence is not reference absence, and a schema-only sweep would have
reported "uncapped" wrongly.

## The registry's pointer invariant, made executable (#452) — decisions, 2026-08-22

The narrative is in CHANGELOG.md. What it cannot hold is which of the issue's three candidate
fixes were taken, why one was not, and the second rot mechanism the work uncovered on the way.

**Adopted: the `Tracked:` / provenance split, enforced rather than restated.** #445 repaired 60
stale pointers by hand and #453 wrote the rule into the Format legend — `Tracked: #N` names an
**open** issue; a closed one may appear only as `(delivered)` or a trailing `landed for #N`
clause. That split is what makes the invariant checkable at all: only the first class can ever go
stale, and it is the smaller one. So `tools/registrycheck` parses the two apart and state-checks
the head alone — the live tracker because it must be open, and a `(delivered)` citation because
GitHub must at least still know it. Both false positives the #445 prototype hit are pinned as tests rather than
described: a tail may legitimately name an open issue (`the transport landed for #45, and the
fallback itself is #348`), and so may a `(delivered; …)` parenthetical (`#50 (delivered; … the
open #166)`). A depth-blind `strings.Split(";")` truncates twelve of the file's heads, six of which carry a
comma inside a parenthetical too; that is a test, not a comment.

**Evaluated and rejected: retrofitting provenance to cite plans and PRs instead of issues.** The
issue's second candidate is right that a plan number and a PR number carry no state to falsify
them. But neither does a past-tense issue citation, and the guard is the proof — it never
state-checks a tail, because `landed for #74` is true whether or not #74 is open. Rewriting 54
such clauses would cost the reader the identifier they are most likely to want, the issue holding
the discussion, to buy something the split has already bought. Kept as guidance for new
provenance, which may cite whichever of the three is most useful: the file already mixes all
three, and one clause (`*Landed for docs/plan/20_gcp-deployment.md slice 2.*`) names no issue at
all — which is why the guard requires none of a `Landed for`.

**Adopted: where a tracker is shared, the pointer carries a parenthetical.** #453 gave 51
pointers a clause naming what their entry still leaves open and left 33 bare, on no principle but
the accident of which trackers happened to close. The rule that closes the gap is derived rather
than hardcoded: an issue named as the live tracker by *more than one* entry cannot, by itself,
say what any single one of them leaves open, so it must carry a parenthetical. That names #78 —
the recording tracker, live on 82 entries — without the guard ever knowing the number 78, and it
picked up #56 and #432 by the same reasoning.

**Evaluated and rejected: making the parenthetical rule check truth rather than presence.** It
cannot. A regex sees that a clause exists, never that it is honest, and the 33 were written by
reading each entry in full for that reason. The rule is still worth having: it makes the omission
loud, and an omission is the failure mode that actually occurred.

**What the rule found: five pointers their own issue can no longer settle.** #56 was "Console SSO
and RBAC" when five entries named it. It is now "Multi-tenant activation (post-v1) — the
remaining half of the original RBAC/SSO issue", and settles none of them: three record shipped,
argued divergences with nothing outstanding at all, and the two that do leave something open are
waiting on a recording, which is #78's job. This is a second rot mechanism and it is invisible to
an issue-state check — the issue stayed open while its scope moved out from under the pointer.
Nothing but writing the parenthetical would have surfaced it, which is the strongest argument the
convention has. The three become provenance (`*Landed for #56's SSO/RBAC half (plan 31 slice
2).*`) and the two are re-pointed at #78.

**Adopted: the guard is split at the network boundary, not filed on one side of it.** The shape
rules — clause grammar, an INFERRED entry with no live tracker, the shared-tracker parenthetical,
and a bare `(line NN)` cross-reference, the mechanism that had drifted 77 lines before #453
caught it — are offline and free, so they run in the merge gate. They run there as the package's
own test rather than as a `verify` prerequisite, which is the shape
`internal/modeltest/docs_test.go` already uses to hold README's tier table to the tree. Only the
issue-state lookup needs the network, and `make verify` is offline and credential-free by design,
so that half is `make registry-check`, outside the gate, run by
`.github/workflows/registry.yml`.

**Evaluated and rejected: the verifier's docs-consistency rung as the guard's only home.** It has
`Bash` and `gh issue view` is already allowlisted, so it could run this. But the verifier is
scoped to the change under review, and pointer rot is caused by an event outside every diff —
someone closing an issue elsewhere. A diff-scoped check would fire only when someone happened to
touch the registry, which is precisely the silence #452 describes. The rung is the wrong shape
for a fact that rots on its own.

**Evaluated and rejected: leaving the state rung to the schedule alone.** The issue proposed a
scheduled workflow beside `evals.yml`, and that is what runs daily. But a schedule only catches
rot after the fact, and the cheapest moment to fix a pointer aimed at an already-closed issue is
the PR writing it — so the workflow also runs on pull requests that touch the registry or the
guard, where it costs five API calls.

---

## Multi-agent session threads (plan 35, #53) — archived 2026-08-21, all six slices delivered (#434, #435, #436, #437, #440, #443)

Another of the seams v1 deliberately reserved, now built — scheduled deployments
and memory stores are still on that list. A session had always been one agent's
event log; the reference's `sthr_` thread resource says it is a *tree* of them, and
#53 had sat behind the question of how much of that tree a wire-compatible
self-hosted platform has to own. The answer this plan settled: all of it except the
sandbox — a coordinator's agents are concurrent threads on one shared container,
which is what makes the topology cheap enough to be the default rather than a tier.

The design is the plan's fifteen decisions, and three of them carried the weight.
**Session status is a fold** over live threads (`running ≻ rescheduling ≻ idle`), so
a session with four busy children reports one status without any thread knowing
about the others. **Delegation is settlement-executed**: the six tools touch no
sandbox, so the transaction that commits the turn calling one also does what it
asked and answers it there — no work item, no driver, no second round-trip. And
**the BYOC worker stays thread-unaware**: a `self_hosted` session's session-level
view widens to carry child threads' `agent.tool_use` and the results answering it,
so a customer-run worker built against the single-agent wire runs a coordinator
session's tools without knowing threads exist.

- **Slice 0 — SDK bump v1.61.0 → v1.63.1** (#434). The plan-05/11 ritual: pin,
  pairwise diffs, three live version labels advanced, every `v1.61.0` evidence label
  in DIVERGENCES re-read, and registry entries plus issues #430–#433 for the v1.62
  surface deliberately excluded. Four v1.63.x toolset/runner behaviors converged.
- **Slice 1 — roster resolution and the session snapshot** (#435). `multiagent`
  accepted on agent create/update and resolved into stored members; session create
  snapshots full member definitions, the `self` member from the session's overridden
  coordinator spec. Inert at runtime — a coordinator session still behaved as a
  single-agent one, which is what made the slice safe to land alone.
- **Slice 2 — the thread resource and the primary thread** (#436). Migration 0025,
  the `sthr_` prefix, five routes, and a backfill giving every existing session a
  listable primary thread. Thread status events beside the twelve session ones.
- **Slice 3 — the thread execution substrate** (#437). Migration 0026,
  `(session, thread)`-keyed turns, the status fold, the runnable set every exec
  driver re-derives, MCP discovery per thread, outcome grading moved to session
  quiescence, thread-scoped interrupts, and the `tool_exec` re-arm. Child rows came
  from a test seam: the substrate was coherent and the wire unchanged before
  anything could reach it.
- **Slice 4 — coordinator delegation** (#440). The six tools answered in the commit
  that emits them, the agent-to-agent message pair, the 25-thread cap, the
  `self_hosted` view rule and the worker's coordinator-mode scan, and the roster's
  skills union. Its acceptance ran against the real `ant` CLI and a real model on
  both deployment kinds, and the `coordinator-team` eval trial ran live. Both those
  records, and the slice's review rounds, are the sections immediately below.
- **Slice 5 — close-out** (#443). This record, the ARCHITECTURE and README rewrites,
  and the plan archived.

What was deliberately left undone is filed, not forgotten: **#441** (the BYOC worker
did not then cancel an in-flight call once its use was answered, as the executor
already did) and **#442** (a chain-loop bound, `WakeThread` guard parity, and an ask-gated
integration test). The inferences this plan had to make — the six tools' schemas and
answers, the `self_hosted` view widening, the session-status fold — are registered in
DIVERGENCES.md and tracked by **#78** against a future recording of a real
coordinator session.

## Coordinator delegation acceptance — real `ant` CLI, real model, cloud and `self_hosted` (plan 35 slice 4, run 2026-08-20) — ✅ passed

The transcript the plan asks for, against a stack built from the branch (compose, its own
project and its own network) and a live model endpoint. The `ant` CLI was v1.23.0, built
from the read-only reference checkout.

**Cloud.** `beta:agents create` a `researcher`, then a `lead` whose `--multiagent` roster
names it — the response echoed the roster eagerly pinned (`{"id":…,"type":"agent","version":1}`),
slice 1's rule. `beta:sessions create` from `lead`; `beta:sessions:threads list` returned
the primary alone, `parent_thread_id: null`, its id the session's token under `sthr_`.
One `user.message` then produced, on the session view and in this order:
`session.thread_status_running` + `session.status_running`, the coordinator's
`agent.message` and its `create_agent` `agent.tool_use`, then **`session.thread_created`,
`agent.thread_message_sent`, the child's cross-posted `session.thread_status_running`**, the
`create_agent` result, a second turn calling `wait_for_agents`, and the primary parking on
`session.thread_status_idle`/`end_turn` — with no `session.status_idle`, because a child was
running (decision 4's fold). The child's own list showed its
`agent.thread_message_received` (`from_session_thread_id` the primary), its own status
events carrying `session_thread_id`, its turn's `agent.tool_use` rendered
`session_thread_id: null` on its own surface, its `agent.thread_message_sent` back
(`to_session_thread_id` the primary), and its `end_turn` idle. The report then woke the
parked coordinator — `session.thread_status_running` on the primary — which summarised and
idled, and only then did **one** `session.status_idle` close the session: decisions 6, 7
and 4 end to end, on a model this platform never told about threads.

A second task spawned a second child; a `user.interrupt` naming that child's
`session_thread_id` was accepted and echoed with the thread named and `processed_at`
already set (the INFERRED stamp-on-append slice 3 registered), and left its sibling and the
session alone. It settled nothing, because the child had already reported in the seconds
before it landed — the documented outcome for an interrupt with no turn to end, not a
demonstration of stopping one; the tests carry that case. Archiving the idle child returned
`terminated` with `archived_at` set; archiving the primary returned the designed 400,
"the primary thread cannot be archived; archive the session".

**`self_hosted`.** The same shape with a `toolworker` whose agent carries
`agent_toolset_20260401`, so the child's turn calls `bash`. The child's `agent.tool_use`
appeared **on the session-level list carrying its `session_thread_id`** — decision 13 (i)'s
view rule, the event a cloud session keeps on the child's log alone. `ant beta:worker poll`
— the reference's own thread-unaware runner, which never calls a thread endpoint — claimed
the `tool_exec`, found that call, ran it and posted a plain `user.tool_result`; the result
landed on the child's log, the child reported, the coordinator summarised, and the session
idled. A worker that does not know threads exist served a child thread, which is the whole
of reading (a).

**The eval trial, and what its first live run found.** `coordinator-team` failed on its
first run against a real model, and every part of that failure was the model's. The
coordinator was offered four tools no agent had declared and used them: two `create_agent`
calls, two child threads created and started, `wait_for_agents` returning the park verbatim
("Wait started. Reports arrive as messages; do not conclude yet."), the primary parked idle
and woken when the children ended. Neither child called `submit_result` — both answered in
plain text and ended their turns — so the coordinator was told twice, in the platform's own
words, that a child had ended its turn without reporting, followed that notice's advice by
re-messaging both with `send_to_agent`, and finally answered that it could not retrieve the
codes. The spawn graders and both thread-row graders passed; the two content graders caught
it. The fix was the trial's, not the platform's: each worker's system prompt now names
`submit_result` as how it reports, which is configuration a real deployment writes the same
way, and leaves the coordinator half — the half this slice built — still under test. The
re-run passed 1/1.

Two things this run cost, both repaired and neither in the branch. The compose file pins
its default network name (`managed-agent-platform_default`) so the executor's gate setting
can name it verbatim, which means `-p <other-project>` does **not** isolate a second stack:
this run's first attempt joined an already-running stack's network, where `postgres` and
`openbao` resolve to whichever container answers, and its brain applied migrations 0025 and
0026 to that stack's database — leaving a 0.3.0 deployment whose `ON CONFLICT
(session_id, kind)` no longer matched an index. Both migrations were reverted there and the
original index and primary key restored, proven by a zero-row insert that still resolves the
conflict target; the acceptance was then re-run under an override giving it its own network (#438).
Separately, `deploy/compose/openbao-init.sh` truncates `init.json` with the redirect on
`bao operator init` before that command can fail, so a failed init leaves an initialized
vault whose root token is gone and no path forward but new volumes (#439).

## Multi-agent session threads (plan 35, #53) — slice 4's review rounds: seven defects the tests never reached, two defects in the tests themselves, and four findings refuted, 2026-08-20

Slice 4 is where the six delegation tools became reachable, and the review rounds
found what a green gate could not. The defects all lived in states no test drove —
an interrupt arriving in a particular order, a cache read in the same turn that
invalidated it, a page served a moment before an append — not in code the tests ran
and misjudged. Three reviewers ran: the Codex pass, the Claude `/code-review` pass on
Opus 5, and CodeRabbit, whose three review submissions opened eighteen threads
between them.

**Seven defects, fixed before merge.**

- **A thread-scoped interrupt naming the primary left the coordinator asleep.**
  `internal/api/events.go` decided per thread whether a batch stopped the primary, so
  whether it re-woke depended on the order the threads came back in. The fix reads the
  primary's own arm **once, before the loop**, which covers both spellings: a
  session-wide interrupt reaches the primary through the expansion, and a
  thread-scoped one naming the primary keys straight to `""`.
- **A delegation tool called by the wrong half of the topology was answered as an
  unknown tool.** All six names are now classed as settlement work inside a rostered
  session while each thread is still *offered* only its half, so a cross-half call
  gets a sentence naming the tool its own half provides instead. The classing runs
  before the agent's own tools are read, so a genuinely custom tool of that name still
  wins it back.
- **`wait_for_agents` could answer from a stale cache.** The wait cache was not
  invalidated by a spawn in the same turn, so a coordinator that created an agent and
  immediately waited could be told nothing could still report. Cleared at the child
  insert.
- **The BYOC worker's walk could run a call already answered.** A coordinator's walk
  spans the whole log, so its window is arbitrarily wide and a result can land between
  a page being served and the walk finishing. `dropAnsweredSince` re-checks from the
  anchor the walk started at, filtering on **all four** walk types rather than results
  alone — the anchor may itself be a `tool_use` or a confirmation, and a
  results-only filter would never encounter it, so the "one bounded pass" claim would
  have been false.
- **A `deny` could be released by a confirmation.** The worker's new permission gate
  read `evaluated_permission != allow || allowed[id]`, which runs a denied call if any
  allow confirmation names it. Not reachable through the API — `ValidateToolConfirmations`
  refuses a confirmation naming a non-`ask` call — so this was the driver failing to
  re-derive a gate the control plane happened to hold. Bound to `ask` explicitly, since
  the whole point of that check is that the driver trusts nothing upstream.
- **A missing `session_threads` row faulted the settlement** rather than being treated
  as "no notice to deliver", which would have failed a turn deterministically instead
  of degrading.
- **Two roster members could share a name.** A coordinator spawns a member *by name*,
  so a duplicate makes the roster ambiguous at the one point that matters. Rejected at
  snapshot time, naming both members and their indices.

**Three findings refuted with evidence** in those rounds, rather than "fixed" (a fourth is in the last paragraph):

- A `skill_`-prefixed fixture rename: applied, it 404s — `checkSkillID` accepts
  catalog short names, which is what the fixture uses.
- A watermark-bounded rescan, offered as a fix for the whole-log walk's cost: it would
  strand an ask-gated call released while the work item is live. The Claude reviewer
  reached the same conclusion independently.
- Asserting an exact child-thread roster in `ThreadPerAgent`: it would turn a model's
  redundant spawn into a `Platform`-class eval failure, which is a grading error, not a
  platform defect.

**Two earlier rounds bought tests rather than fixes.** `TestAnsweredDuringTheWalkIsNotRun`
called `dropAnsweredSince` directly, so it stayed green if the walk stopped calling
it, passed the wrong anchor, or ran it in the wrong mode;
`TestTheWalkRechecksThroughTheRealPath` drives the walk itself and wraps the control
plane to append a result *after* a page has been served — an interleaving nothing can
time from outside — with both modes in one table. Separately, the verifier's own
re-run found the cross-half shadowing win-back load-bearing and untested;
`TestACustomToolNamedLikeTheOtherHalfKeepsItsName` pins both directions. Each was
proven red first against the untouched production code.

**A later round turned the review on the tests themselves, and found two of them
lying.** `TestAnsweredDuringTheWalkIsNotRun` reused one slice across both
`dropAnsweredSince` calls; the helper filters in place (`kept := uses[:0]`), so the
first call writes through the caller's array, and the case passed only because the
element it drops is last — drop the first instead and the second assertion would
have counted a corrupted `[second, second]` as two and passed on garbage. And
`TestTheWalkRechecksThroughTheRealPath`, written the round before, called `t.Fatalf`
from its wrapped HTTP handler's goroutine: `t.Fatalf` is `runtime.Goexit`, so an
append failure there would have abandoned the response and surfaced as a scan error
or a hang rather than the failure that happened. The same round closed two coverage
gaps — `wrongRole`'s coordinator arm, whose absence lets a coordinator report to a
parent it does not have and idle the session with every report unread, and the
executor fixtures that spelled a delegation name in raw JSON while the guard filtered
through the constant — and refuted a claim that the duplicate-name refusal had no
API-level test, which `TestRosterRejectsTwoMembersSharingAName` has had all along.

That same verifier re-run passed the code and failed the registry: `list_agents` had
gained a `stop_reason` and the shadowing rule had changed, and neither had reached
DIVERGENCES.md. Both were registered before merge, along with the duplicate-name
refusal, under the repo's rule that a slice lands its divergences in the PR that
introduces the behavior.

Two consequences were filed rather than absorbed: **#441** (the BYOC worker did not
then cancel an in-flight call once its use was answered, as the executor already did —
the slice-3 changelog fragment was corrected at the time to stop claiming both drivers
do, and corrected back when they did) and **#442**
(a chain-loop bound, `WakeThread` guard parity, and an ask-gated integration test).

## anthropic-sdk-go v1.63.1 bump — wire-schema verification record (2026-08-19, plan 35 slice 0)

The fourth bump record, and the first since v1.59.0's to move shapes this repo mirrors: v1.62.0
(2026-08-06) is the release that carries every managed-agents type change in the range — advisor,
budgets and list-cost usage, `session.usage`, `redacted`, `inference_geo`, `budget_reached` — and
none of it is in the platform's scope, so all of it lands as **registry entries and four issues,
not code**; the range's other content is **behavior** in the reference's client-side toolset and
runner (v1.62.0's typed refusals and `executeTools` early return, v1.63.0's toolset changes,
v1.63.1's runner placeholder), four of which were converged in-bump. The bump exists
for plan 35: the threads surface it will build (`betasessionthread.go` and its unions) verifies
against the latest release, and v1.63.1 is the latest (confirmed against the upstream tag list on
2026-08-18). Endpoint count is unchanged at **131** (`.stats.yml`); the spec hash moved; the SDK's
own `go.mod`/`go.sum` are byte-identical between the tags, so the bump drags in no new module.

**What the range contains.** 61 files, +4166/−356. `shared/constant/constants.go` gains exactly one
constant (`SessionBudgetReached`, the webhook event type), drops one `ModelNonStreamingTokens`
row the platform never reads (`claude-opus-4-1-20250805`), and moves **no literal** — the eleven
removed non-comment lines across the four session files are Go retypes to unions whose JSON tags
are unchanged (`Multiagent.Agents`, `SessionMultiagentCoordinator.Agents`, `SessionThread.Agent`,
`agent.message` content), so the dangerous class of the v1.59.0 bump is structurally empty again.
The v1.62.0 changelog headline "skills auto-loading from GitHub" left **no Go-SDK schema surface**
(the only `Skill` hunks are union projections onto the new advisor unions; the CLI checkout has
no matching string) — stated here so nobody hunts for it later.

**The enumeration.** Every SDK file defining a shape this repo mirrors, `git diff`ed pairwise
between the tags. Byte-identical: `betaenvironment.go`, `betaenvironmentwork.go`,
`betavaultcredential.go`, `betasessionresource.go`, `betafile.go`, `betaskill.go`,
`betaskillversion.go`, `betasessionthreadevent.go`, `betaagentversion.go`, `tools/agenttoolset/
skills.go` and `search.go`, `lib/environments/worker.go`, `config/federation.go`. Changed, each
resolved:

- *`betasession.go` (+392), `betasessionevent.go` (+413), `betasessionthread.go` (+215),
  `betaagent.go` (+48), `betawebhook.go`, `betadeployment.go`, `beta.go`* — the v1.62.0 types:
  `AdvisorParams` and the roster/coordinator/thread unions admitting `{type:"advisor"}`;
  `BudgetLimit`/`BetaMonetaryAmount`/`BetaCurrency`, `budget` on session create/update and on the
  session (`api:"required"`), `session.updated` and deployments (nullable), the `budget_reached`
  stop reason on both idle events, the `session.usage` event in the list/stream/thread-stream
  unions, `SessionUsage` + thread usage gaining `active_seconds`/`list_cost`/`server_tool_use`, the
  `session.budget_reached` webhook; the `redacted` block on `agent.message`, both thread-message
  events, deployment and **inbound** `user.message` content; `model.inference_geo` on params and
  response. Session-events list/stream/send params are byte-identical. **Registered**: one
  CONFIRMED entry for the unbuilt surface (each reachable piece is a 400, none a silent drop —
  `budget` on create/update, `redacted` inbound), the `effort` entry extended to `inference_geo`
  (still a silent drop, by the same reasoning), and one INFERRED entry for the session's
  `budget: null` render (the tag says required, the deployment twin says nullable, `archived_at` is
  the precedent). Issues #430 (redacted), #431 (advisor), #432 (budgets / usage / `session.usage`),
  #433 (`inference_geo`). The `agent.message` content retype is decode-compatible: the platform
  emits text blocks only.
- *`tools/agenttoolset/fs.go`, `agenttoolset.go`, `skillarchive.go`* (v1.63.0, #226) and *`bash.go`,
  `agenttoolset.go`* (v1.62.0, #213) — behavior, not shape. **Converged in-bump:** an inverted `view_range` selects nothing (empty
  content, no error — the platform's error text had been byte-identical to the reference's old one);
  skill-archive extraction materializes only regular files and directories (`zipEntryIsPlain` — a
  Unix-host symlink/FIFO/device entry is skipped, an `S_IFDIR` entry without the trailing slash is
  a directory, a non-Unix entry is data whatever its bits — and, a Codex review finding, the upload
  form now refuses a `SKILL.md` that is such an entry, since it would validate and never
  materialize); `fsErrorMessage` now answers an unmapped
  error with Go's bare errno text (`read-only file system`) or `"i/o error"`, never `op /abs/path:` —
  the platform's passthrough of the sandbox's strerror lowercases its first rune to match. **No
  change:** `ToolError` typed refusal and `ErrBashTerminated` (in-process API, wording unchanged);
  symlink-loop rejection (`too many levels of symbolic links`) — the platform has no workdir
  confinement (registry) and its sandbox contract refuses a symlink leaf outright, a pre-existing
  difference under the same entry.
- *`betasessiontoolrunner.go`* — v1.63.1 (#236): the runner posts `(no output)` for an empty text
  block in `user.tool_result` / `user.custom_tool_result` ("The Sessions API rejects empty text
  blocks; a tool that succeeds silently must still produce a postable result"); v1.63.0 (#226):
  `isFatal4xxStatus` excludes 409. **Converged in-bump:** both platform halves post `(no output)` for a silent
  success (the executor had posted `[]`, the worker no content), and every inbound text block is
  refused when empty — INFERRED, since the SDK witnesses the rejection for tool results only and
  records no status; the extension to `user.message` is the platform's replay-wedge argument. The
  409 change is a no-op: the platform's events lane never answers 409.
- *`betatoolrunner.go`* (v1.62.0) — `executeTools` gains a second early return: a `max_tokens` /
  `model_context_window_exceeded` turn executes **no** tool ("a cut-off turn left its last call's
  arguments incomplete"), complete calls included. The platform keeps running the complete blocks
  of such a turn — its truncation guard is per block, and #181 is the reason — so the turn-
  classification registry entry and `brain.go`'s comment now state the disagreement instead of
  citing the SDK's refusal return as its sole non-execution case.
- *`lib/environments/poller.go`* — `isFatal4xx` also excludes 409 (heartbeat 409 → transient,
  retried to the staleness ceiling; poll 409 → backoff). The platform's work API answers 409 only
  from stop, never poll/ack/heartbeat, and its heartbeat mismatch is 412 (fatal on both sides), so
  nothing needs tolerating; the platform's own BYOC twin `isFatalHeartbeat` keeps 409 fatal
  (`internal/worker/lease.go`) — unreachable against this server, left as is.
- *`betamessage.go`, `message.go`* — thinking / redacted_thinking doc comments and the removal of
  the two `claude-opus-4-1` model constants (model ids are opaque config-resolved strings here);
  no stop-reason or MCP change. `betadream.go` (+285, v1.63.0: `output_behavior`) is Messages-side
  dream surface the platform does not mirror. `api.md` +21 rows above the route table (Stop Work
  citation 693 → 698). No-op.

**Citation durability — 36 SDK line citations re-read at v1.63.1: 15 hold, 21 drifted, 0 broken by
line; one broken by content** (`betatoolrunner.go executeTools`, restated above). Every drift is a
mechanical shift from insertions above the cited lines (advisor +28 and `inference_geo` +8 in
`betaagent.go`; the budget/advisor/usage types in `betasession.go`; `execRead`'s −2 in `fs.go`;
+2 imports in `worker_test.go`; +5 in `api.md`), each re-read identical at the new line and
corrected in the registry. The 24 live `v1.61.0` labels advanced (`.claude/agents/verifier.md`,
`docs/REFERENCE_PROJECTS.md`, `internal/toolset/definitions.go` — the toolset types are untouched —
plus `internal/domain/outcome.go`, `internal/api/outcomes_test.go` and the registry's evidence
labels); the historical ones ("v1.61.0's SDK re-worded…", "again with v1.61.0's: 683 → 693",
CHANGELOG/HISTORY/archived plans) stay as written. Plan 35's Ground-truth ranges are v1.61.0 lines
by its own statement and were re-confirmed at that tag; the ranges that drift at v1.63.1 are the
plan's to update when it cites them.

**Evidence.** The sweep ran as five parallel investigations (session/event types; agent/thread/
webhook types; toolset and runner behavior; citation durability; Messages/REST residue) plus a
cross-check that reconciled their file lists and found no contradiction. `make verify` in WSL on the
bumped pin: every package green (Docker and K8s sandbox suites included) at total statement
coverage **91.03%** — the one red line being `internal/mcp`'s #380 assertion, which fails on
roughly half of WSL2 runs, on `main` too, and is green in CI; a run before the review fold
went fully green at 91.06%; the toolset, skills, events, executor, worker, brain and api suites were additionally
run standalone under the new pin.

## Trimming the documentation to what code cannot say (plan 34, #413) — archived 2026-08-16, six slices delivered

Tracked markdown went **2,665,735 → 2,166,819 bytes** (−19%) and is now smaller than the
2,382,801 bytes of non-test Go it documents, which was the plan's stated problem. The rule
the plan wrote into CLAUDE.md — a document earns its place by holding what code cannot — is
also what stopped it: of nine byte targets, four were retired rather than met, three were
missed after real cuts, and two were met or beaten (`ARCHITECTURE.md` by 71 KB, and
`changelog.d/`, whose size a release resets anyway).

**The retirements failed for one reason, and the plan re-learned it each time.** Each target
was derived from a cluster's *size* without asking what the cluster must still hold, then
defended by an instrument narrower than the thing it tested — quoted section titles only,
`var.` occurrences only, heading granularity. Every deletion premise that broke, broke on
the reference class nobody had measured. The finding is not that documents are
incompressible: it is that a byte target is a hypothesis about content, and only the content
settles it.

**One deletion was made and reverted inside its own PR.** Slice 5 cut 33 spent sections from
the 32 archived plans after a cut-candidate check found zero collisions against quoted
section titles. Review found plans are cited two ways that check never tested — by slice
number (`plan 29 slice 3`) and by role ("whose DCF-rubric example the plan's acceptance
replays") — and the verifier's docs rung had passed the same deletions on the same evidence.
Everything was restored. Archived plans are not trimmable; the attempt is the evidence.

**What survives is method, not bytes.** Slice 5's working shape — draft a rewrite, then set
an adversarial skeptic on it whose only job is to find what the rewrite lost — restored 25
claims the rewrite had dropped and fixed 9 accuracy defects across 24 entries, the defects
mostly inherited from the original entries rather than introduced. Feeding the first batch's
defect patterns into the second batch's prompts more than halved the losses per entry: 20
across 11 entries, then 5 across the 6 that followed — the two measured batches, covering 17
of the 24.

Rejected along the way: rewriting citations to permit deletion (churn against a record whose
value is that it does not move), and editing archived records to match present behavior — a
decision later overturned is exactly what an archive exists to hold, so reversals get dated
notes instead. Detail, including every retired target and why, is in
[docs/plan/34_doc-trim.md](./plan/34_doc-trim.md).

---

## MCP client and `mcp_toolset` (plan 29) — archived 2026-08-15, delivered in twelve PRs (#45)

Closed #45: an agent configured with `mcp_servers` + `mcp_toolset` now calls a real MCP
server's tools. Before this plan both were accepted on the wire and stored, the brain never
expanded the toolset, and such an agent silently got no tools at all.

Delivered as seven slices:

1. **Wire correctness** (#343) — the shapes the rest of the plan is built on, checked
   field-by-field against the pinned SDK.
2. **The client and the catalog** (#352, #377) — `internal/mcp` over the official go-sdk,
   the dial-address guard beneath it, the `mcp_catalogs` table, and the `mcp_exec`
   discovery driver. Inert: nothing offered to the model yet.
3. **A tool call can stop and ask** (#387) — the human-confirmation arm, on the same
   `always_ask` default the rest of the platform gates on.
4. **Activation** (#398, #402, #404) — the execution driver and MCP-first settlement, the
   brain offering a catalog's listing as `mcp__{server}__{tool}`, and an oversized answer
   spilling into the session's sandbox. **#45's acceptance criterion is met here.**
5. **Credentials** (#405, #406) — vault matching, bearer injection on both dial paths,
   `mcp_authentication_failed_error` split off the connection failure, and an expired
   `mcp_oauth` token refreshing at the dial with the rotation sealed back onto the row.
6. **Networking polish** (#407, #409) — `allow_mcp_servers` widening the per-session gate
   so a sandbox reaches the servers its agent declares; the discovery pass dialling
   concurrently so declaration order stops deciding who gets reached, and a server it
   cannot reach said out loud on the session's event stream.
7. **Evals and acceptance** (#410) — the `mcp-answer` eval trial, the `RUN_LIVE_MCP_TESTS` tier
   against a server this project did not write, and the `ant` CLI acceptance run recorded
   below.

Three things the plan set out to do and did not, each for a reason recorded where it
belongs: no per-server disable state (no source documents one, so a transient failure heals
by itself); no MCP surface on the work API (the reference makes the connection server-side
on every environment kind, so a BYOC worker never sees one); and the discovery pass stays on
the goroutine that took the work item rather than moving off it, which no source asks for.
Issue #408 was filed out of the last slice's review: a credential-resolution failure that is
infrastructure rather than credential faults its work item and says nothing, which is a
work-queue retry-policy question rather than an MCP one.

## MCP toolset acceptance — real `ant` CLI, real third-party MCP server (plan 29 slice 7, run 2026-08-15) — ✅ passed

A controlplane + brain + executor built from this branch, on ports of their own against a
Postgres of their own — deliberately not the shared compose stack, whose image predates
every commit under test. Every management call driven by the real `ant` CLI (v1.22.1, built
from the local checkout) over `--base-url`, and the MCP server was **DeepWiki's public
endpoint** (`https://mcp.deepwiki.com/mcp`): a server this project did not write, reached
over the public internet through the executor's own guarded client.

- **The agent round-trips.** `ant beta:agents create --mcp-server '{"type":"url","name":"deepwiki","url":"..."}' --tool '{"type":"mcp_toolset","mcp_server_name":"deepwiki"}'` returned the agent with both arrays echoed in the wire's shape, and with the `mcp_toolset`'s `default_config.permission_policy` defaulted to **`always_ask`** — the gate issue #26 extended to the MCP arm, visible on the wire without anyone asking for it.
- **The chain closes.** One `user.message` asking which MCP tools were available produced `session.status_running` → `span.model_request_start` → an `agent.message` naming **`mcp__deepwiki__ask_question`, `mcp__deepwiki__read_wiki_contents` and `mcp__deepwiki__read_wiki_structure`** → `span.model_request_end` → `session.status_idle` with `stop_reason.type = "end_turn"`. The model can only have those names from the listing the brain assembled out of `mcp_catalogs`, so one message proves the executor's discovery pass dialled a third-party server, stored what it answered, and the brain offered it under this platform's `mcp__{server}__{tool}` naming.

Two CLI shapes worth recording, both this client's and neither a defect of ours:

- **`--model` takes a mapping, not the scalar its help text advertises.** `--model MiniMax-M3` fails inside the CLI at `[1:1] string was used where mapping is expected`, before any request goes out; `--model '{"id":"MiniMax-M3"}'` works. Our server's rejection of the intermediate guess (`{"model":...}` → `model.id is required`) is itself the wire shape asserting itself.
- **The event verbs are `retrieve` / `send` / `list` / `stream`**, addressed by `--session-id` rather than positionally. `get` and `--event` on a positional session id do not exist in v1.22.1.

The live MCP tier landed with the same slice and was run against the same endpoint:
`RUN_LIVE_MCP_TESTS=1 MCP_LIVE_SERVER_URL=https://mcp.deepwiki.com/mcp` listed three tools
through `mcp.DefaultClient` — the guarded one — in 6.2s. That tier exists because every
other MCP test in the repository speaks to a fixture built from the same go-sdk the client
is built from, so both ends share one understanding of the protocol and agree even where
that understanding is wrong.

The `mcp-answer` eval trial (`RUN_EVALS=1`, real model, real containers) covers the half the
CLI run does not: a passphrase that exists only in what an MCP tool returns, retrieved
through the confirmation round trip the `always_ask` default requires. Its first run failed
in a way worth keeping: the model read "tell me the secret passphrase" as a prompt-injection
attempt and declined — naming `mcp__vault__read_passphrase` in its refusal, which is itself
proof the platform chain underneath was working. The trial now says who attached the server,
as the other mounted-answer trials do.
## A work item cannot run forever (plan 33, #383) — archived 2026-08-15, slice 1 delivered; slices 2 and 3 deferred to #395 and #396

docs/plan/33_bounding-a-wedged-work-item.md is archived: a work item is now bounded by its
own silence in both halves of the pull protocol — the platform executor through the shared
lease keeper, the BYOC worker through its heartbeat — and a holder that reports no progress
for its budget has its work cancelled and its lease left to lapse, so the item is reclaimed
by the path that a blocked-but-alive process had made unreachable. The narrative is in
CHANGELOG.md; what follows is the designs evaluated and rejected, one of which was this
plan's own first draft.

- **A ceiling on the item's total runtime** — the plan's original D1, written before the
  tool loop was read closely. Rejected once it was: a `tool_exec` runs its turn's tools
  *serially*, and `bash` — the one tool that carries a cap, `toolset.MaxTimeout`, ten
  minutes — may legitimately spend all of it, so a turn of eight `bash` calls can legitimately
  occupy eighty minutes, while `read`/`write`/`edit` carry no cap at all, which is the wedge
  itself. Any ceiling tight enough to contain a wedge would kill that turn; any ceiling loose
  enough to spare it (hours) would contain little.
  Silence separates the two cases where a clock cannot, which is the same conclusion plan 17
  reached for the model endpoint — and #383 itself named that shape before this plan did.
- **A deadline on each sandbox call** — rejected for the reason the seam is untimed by
  design: `WriteFileStream` legitimately streams a 500 MB mount and an untimed `Exec`
  legitimately runs as long as its command does, so a per-call wall clock has no defensible
  value. The finer version of this — bounding a call by *silence* inside the two transports
  — is right but needs progress plumbing inside Docker's and Kubernetes' streams, and is
  deferred whole to #395 rather than half-built here.
- **Having the control plane refuse to extend a BYOC worker's lease past some age** — the
  plan's original D3, and the tempting one, since the control plane already sees every
  heartbeat and `work_items.started_at` is already set by that path. Rejected: it changes
  observable wire behaviour whose reference semantics are unconfirmed, so it would need a
  recording and a DIVERGENCES entry to be honest about, and it would bound a process this
  platform does not run. The worker bounds itself instead, in its own loop, against its own
  clock — no wire change at all.
- **Inferring progress instead of having the holder report it** — rejected as
  self-defeating: anything derived from a timer, or from the renewal succeeding, reports
  progress a wedged holder is precisely not making. Only the holder can tell a long step
  from a stuck one, so `Progress` is called at the boundaries a wedge stops a run from
  crossing (provisioned, each materialization pass, each tool answered).
- **Reusing `queue.LeaseKeeper` in the worker** — not available: the BYOC worker has no
  database and renews over the wire, so the two lanes keep separate trackers with the same
  arithmetic, each measuring from its own monotonic base as the keeper already did for its
  `Extend` budget.
- **Force-stopping a stalled item from the worker** — rejected for the reason a lost lease
  is not force-stopped either: a stop is terminal and no reclaim recovers a stopped item,
  and by the time the worker gives up, another worker may already hold it. The item is left
  live and its lease is left to lapse, which is exactly what a dead process would have done.

**Review hardening, landed in the same PR.** The first cut armed the budget on lanes and
passes that reported nothing, which both reviewers and the verifier attacked from three
directions at once; each is now a mutation-checked test:

- **Per-pass reporting was the same defect wearing the fix's clothes.** A session may mount
  eight repositories at `RepoCloneTimeout` apiece and dozens of half-gigabyte files, so a
  wholly healthy materialization pass outlasts the budget — and because a pass writes its
  sentinel only at the end, the reclaim restarts it from zero and is killed at the same
  place, forever. Reporting moved inside the loops, per skill, per repository, per mount, per
  harvested output, and the excluded items report too (a vanished file, a repository this
  executor cannot clone): each still cost a round trip. The `web_exec` lane, whose 60s-per-
  request backends make a 31-call turn legitimately exceed 30 minutes, and the `mcp_exec`
  lane, whose own budget is operator-tunable past the stall budget, now report per call and
  per server rather than resting on an arithmetic argument about defaults.
- **A stall settled as a lost lease discarded work that had really run.** The first cut
  returned at the keeper's error for both, so a wedge in the third tool threw away the two
  results already committed to the sandbox's side effects — and the reclaim ran them again: a
  second push, a second POST. A stall now commits down the partial-commit path a backend
  fault uses, since the claimant still holds the lease when it gives it up; only a genuinely
  lost lease commits nothing.
- **Two timing tests were the flake class this change exists to remove.** A 300ms budget
  against a run whose clock starts before a session read and a provision could cancel before
  the gated tool was entered, leaving an unguarded channel receive to hang to `go test`'s
  package alarm — #318's own failure mode. Budgets moved to a second, receives are guarded,
  and the moving-run tests carry a twentieth-of-budget per step so a scheduling pause cannot
  redden the gate.
- Smaller: the stall arithmetic is `elapsed - last > budget` rather than `last + budget`, so
  a budget near the duration ceiling cannot wrap negative and stall instantly; and a
  non-positive `EXECUTOR_STALL_TIMEOUT`/`WORKER_STALL_TIMEOUT` now fails startup instead of
  being silently replaced by the default.

A second review round, on the commit that answered the first, found four more and was right
about all four:

- **The remaining silence was in the steps between the loops.** Skill *resolution* is its own
  loop — two round trips per reference in the executor, wire calls in the worker — and a
  dangling reference leaves it by the skip path without ever reaching the instrumented write
  loop; the sentinel probe is a sandbox read per recorded tree and returns before that loop
  when the set is unchanged; the worker's unanswered-use scan pages over the wire before
  provisioning. Each reports now. So does provisioning itself, between its own steps rather
  than only on return — the credentials resolved, the session lock taken (behind, at worst,
  another goroutine's checkpoint capture), the sandbox up — which was the largest healthy
  pause of all sitting under the wedge bound as one interval.
- **A budget under one step loops rather than degrades.** The knob's documentation promised
  "a cancelled item that is reclaimed and re-run, not a failed session", which is false when
  the re-run is deterministic: every reclaim cancels at the same place and no error ever
  reaches the session. Both binaries now floor a configured budget, and the documentation
  states the loop instead.
- **The ownership proof was a read, not a lock.** `queue.Assert` proved ownership at the
  instant it ran and then let the transaction commit behind it — and the stalled claimant is
  precisely the one settling with nothing renewing its lease. It takes the row `FOR UPDATE`
  now; `Claim` skips locked rows, so a reclaim waits for the settlement instead of racing it.
  What the lock cannot cover is stated rather than papered over: a settlement slower than the
  lease's remainder, and a call that never returns to settle at all.
- **A stall with nothing left unanswered left the item live.** The settlement scheduled the
  `model_turn` and kept the row `active`, so the turn's own follow-on `tool_exec` hit the
  live-item dedupe and the session sat still until the abandoned lease lapsed. It completes
  the item now — nothing unanswered means nothing for a reclaim to do. The same round
  corrected the fault log, which asserted "its results were not committed" on the path that
  had just committed them, and kept the run error that the stall branch had been dropping.
- **Refuted, with evidence:** that the change lets a stalled item's already-committed
  `session.error` rows contradict "nothing commits" — the per-repository clone error is
  appended outside any settlement and always was, exactly as a lease lost at that moment
  would leave it (D4 now says so).

A third round found four more, all of them the same lesson a third time — a report placed at
the end of something that is itself a loop:

- **The floor was exactly the cap, and a tool at its cap answers after it.** Both sandbox
  backends wait a kill grace past a command's deadline before giving up on it, and the result
  still has to come back, so a budget of exactly `toolset.MaxTimeout` would cancel a healthy
  timed-out `bash` moments before it answered — and its use, never answered, would take the
  same path on every reclaim. The floor is that cap plus a minute.
- **The unchanged-set probe is a loop.** `skills.SentinelMatches` reads one file per recorded
  skill, and the API admits sessions of up to 500 skills, so the single report after the probe
  covered a pass that a legal budget can lose to. The reads report as they land, through a
  wrapper around the read function the helper already takes.
- **The worker's event scan pages.** Its report landed after the auto-pager had walked every
  page; a turn wide enough to span several spent all of those round trips inside one silent
  step. It reports per event now, so the silent interval is one page.
- **The lease-TTL flake was left in a third test.** Two of the three keeper tests moved to a
  1.5s TTL; `TestKeepLeaseRenewsWhileHeld` kept the 600ms one whose 400ms renewal bound had
  just reddened a gate run. It moved too.

A fourth round, on the branch as a whole, found the same shape once more — a diagnosis kept
in the rarer case and dropped in the common one — plus five smaller things:

- **The stall report could not name the tool that wedged.** The branch had taken care to
  carry the *setup* error through a stall (the diagnosis when nothing had started yet) while
  overwriting the *tool* fault, which is the ordinary case and the only place the wedged
  tool's name and `tool_use` id are written down. Whichever the run returned now rides along.
- **Four of the executor's stall tests kept the 600ms lease TTL** the queue's had just left
  behind — in a PR whose subject is removing a flake class. They moved to 1.5s.
- Smaller: `Assert`'s doc now says the lock lasts as long as the *caller's transaction* and
  is a bare read when handed a pool; `provisionSandbox` no longer carries a nil-progress guard
  its eight siblings lack (the tests pass a no-op); the sentinel skip's own report went away
  as redundant with the caller's; `report`'s comment no longer states the tool_exec lane's
  commit rule as if it held for the three lanes that discard; and the worker tells a malformed
  `WORKER_STALL_TIMEOUT` apart from a negative one, as the executor already did.
- **Refuted, with evidence:** that wrapping a stall as `lease keeper: %w` in the web, MCP and
  harvest lanes makes it indistinguishable from a lost lease — the wrapped text still reads
  `queue: work item stalled` rather than `queue: work item lease lost`, and `errors.Is` sees
  through the wrap either way.

A fifth round, after the rebase onto plan 29's MCP call lane and #399's renewal, found the
guard's own blind spots — three places where the floor or the reporting stopped short of the
thing it was supposed to cover, and one where the fix contradicted itself:

- **The floor was measured against the wrong step.** It compared a configured budget with
  `toolset.MaxTimeout` alone, but `EXECUTOR_REPO_CLONE_TIMEOUT` is a sibling knob with no
  ceiling of its own, and one clone is a single silent interval of exactly its length. A
  monorepo deployment setting it to 45 minutes and leaving the 30-minute default rebuilt the
  loop the floor exists to prevent — every reclaim cancelling the same clone at the same
  point, a `session.error` every half hour and the repository never mounted. The floor
  follows the longest step the binary can name now.
- **The one guard with no test.** The sign and floor checks were written out twice, verbatim,
  in two `main` packages the coverage gate excludes and that hold no test files. They are one
  helper in `internal/toolset` now, tested where a main package cannot be, and the two
  binaries cannot drift apart.
- **Two steps were counted as one, three times.** The BYOC worker reported only after a tool
  had run *and* its result had been posted, so a full-length `bash` and a slow wire round trip
  shared one interval that the floor does not cover — the run cancelled after its side
  effects and before its answer, and the reclaim running the command again. A materialization
  pass reported per item but not before the sentinel write behind the last one, so a 500 MB
  mount and a slow write shared an interval, and a pass cut between them writes no sentinel
  and repeats forever. And a checkpoint restore — fetch, validate, ship, extract — was one
  silent stretch whose size cap an operator may raise with no relation to the budget.
- **Detection rode the wrong clock.** The stall check shares the renewal ticker, so a lease
  far longer than the budget stretched detection with it: at a three-hour TTL the first check
  came an hour in, and this consumer runs one item at a time. The tick is the shorter of the
  two now, and a test pins both that and the check's position *before* the renewal — an
  ordering nothing else would have failed on.
- **The fix contradicted itself in two lanes.** The `tool_exec` lane commits what already
  answered because a spent side effect must not be spent twice; the web and MCP lanes
  discarded, on a rationale ("re-derivable work") that fits a discovery and does not fit a
  call someone was billed for or one that wrote to a third party's system. Both commit now,
  handing this item back for what is left rather than enqueuing a second one the live-item
  dedupe would swallow. Harvest still discards, for a different reason: half a snapshot is
  not a snapshot.
- Smaller: a settling append that fails on the stall path no longer drops the stall sentinel
  and the wedged tool's name; the copy-pasted budget rationale in the two MCP passes is
  written once.
- **Refuted, with evidence:** that abandoning the lease while the container's command runs is
  a new defect — it is the change's stated cost, in the changelog's "what this deliberately
  does not do" and in #396, and the alternative it replaces is an item nobody can ever
  recover. And that the silence tracker is a third re-implementation to be shared — the BYOC
  worker is wire-only and imports no `internal/queue` in production code, so its heartbeat
  cannot use the keeper's; `provider.StallGuard` sits a layer below both and guards a stream
  rather than a work item.

A sixth round, on the fifth's own fixes, found that half of them stopped one step short —
and both reviewers, independently, found the same first one:

- **The floor guarded only the budget an operator typed.** The check ran when
  `EXECUTOR_STALL_TIMEOUT` was set and never otherwise, so raising
  `EXECUTOR_REPO_CLONE_TIMEOUT` alone — which is the way this actually happens, since nobody
  raising a clone timeout for a monorepo thinks to touch a knob about stalls — left the 30m
  default in place under a 45m clone. That is the exact scenario the round-five commit
  message said was now prevented, which made the message an overclaim as well as the code a
  gap. The default is checked now, and startup is refused until the two agree.
- **Three more pairs were hiding behind one report.** A repository's presence probe and its
  credential decrypt sat ahead of the clone the floor is *measured against*, so the interval
  the floor guards was never the interval that existed. A checkpoint restore's extraction
  shared an interval with the marker update behind it — and a pass cancelled there leaves the
  marker `ready`, so the reclaim reaps the sandbox and restores from zero, forever. The
  worker's session-liveness read shared one with its paging scan, two wire round trips before
  a tool is ever reached.
- **The two new partial commits dropped their diagnosis.** The `tool_exec` lane had just been
  taught to keep the stall sentinel and the wedged tool's name through a failing settlement;
  the web and MCP lanes, which had only just learned to commit at all, had not.
- **Accepted rather than fixed, and written down:** the stall check and the renewal share the
  keeper's goroutine, so a renewal blocked on a stalled database delays the next check by
  however long it blocks. That is bounded by what the lease has left and self-limiting — the
  blocked attempt either times out, which cancels the holder anyway, or returns to a tick
  already buffered and checks at once — and the outcome in that window is a cancelled holder
  either way. A second goroutine would close it and is not worth the synchronisation; the
  keeper's documentation states the cost rather than leaving it to be discovered.

A seventh round, on the sixth's, found two — and again both reviewers landed independently on
the same one, which is again the one that mattered:

- **`cmp.Or` cannot keep two things.** The sixth round taught the web and MCP lanes to carry
  the stall sentinel and the cut-short call through a failing settlement, and spelled it
  `cmp.Or(faultErr, kerr)` — which *chooses*. A stall usually cancels a call in flight, so a
  cause usually exists, so the sentinel usually lost: `errors.Is(err, queue.ErrWorkStalled)`
  went quietly false in exactly the case it was added for, and the round-six entry above,
  which claimed the parity, was wrong when written. All three settling lanes now fold through
  one `stallFault` helper that keeps both, and a test pins the property rather than the
  spelling.
- **The floor's own arithmetic could invert it.** `StallFloor` added its minute without
  checking, and `time.ParseDuration` accepts every duration up to the largest int64 of
  nanoseconds — so a clone budget within a minute of that wrapped the sum negative, and a
  negative floor is one every budget clears. The guard would not have failed loudly; it would
  have read as satisfied while the default went on cancelling the step. It saturates now.
  Unreachable in any real deployment, and repaired anyway: a guard that inverts under its own
  input is a different class of defect from one that is merely absent.
- **And the refusal nobody could test.** The sixth round's default-side check was written
  inline in `cmd/executor`, outside the coverage gate — so the plan and the changelog, which
  both said the refusals lived in a tested helper, had become untrue in the same commit that
  added it. It is `toolset.CheckStallDefault` now, tested there, and the sentence is true
  again.

**Evidence.** Every guard and reporting path was mutated against its test: with the
executor's stall budget unwired the wedged-tool test hangs to its own 15-second alarm; with
the worker's guard disabled the worker holds its item past the same alarm; dropping either
lane's per-tool reports cuts a healthy thirty-tool run short; routing a stall back through
the lease-lost branch loses the answered result; dropping the per-item reports collapses
each materialization, harvest, web and MCP count to its pass boundaries; unlocking the
ownership proof lets a second claimant take an item its holder is still settling; and
removing the resolution, sentinel, pre-scan or provisioning reports drops each of the two
processes' counts by exactly the steps they cover. The fifth round's guards were measured the
same way: with the tick left riding the lease's third a 400ms budget under a 30s lease goes
undetected past five seconds; with the stall check moved after the renewal the wedged item's
lease is bought once more; with either the web or the MCP stall routed back through the
discard the call that had answered is lost; and dropping the worker's run/post split, either
materialization boundary or the files probe's report takes each count down by exactly one. The
sixth round's four report placements answer the same way — the pre-clone and post-credential
reports, the one after a checkpoint's extraction, and the worker's pre-scan boundary — and its
floor was measured by running the binary: a 45m clone budget with the stall knob unset is
refused by name, 29m30s is refused against the 30m default by half a minute, 20m is accepted,
and a deployment that sets nothing still starts. The seventh round's two are the sharpest of
the set, each mutation restoring the exact code a reviewer had objected to: spelling the fold
`cmp.Or` again turns `errors.Is(err, ErrWorkStalled)` false on an error that names the wedged
call, and dropping the floor's saturation makes `StallFloor` of a near-maximum step return
*negative* — whereupon both the typed-budget and default-budget refusals silently become
acceptances. The binary answers to match: a 292-year clone budget is now refused rather than
started, and the eight earlier probes are unchanged. The suite was observed failing in each
case before the mutation was reverted.

## A refutation of #392 that both reviewers refuted, 2026-08-14

A **review-hardening record**: the first version of the #392 fix argued the issue had no
defect behind it, and shipped no production change. Both reviewers independently proved
that argument wrong, and the PR became a real fix. The defect and its repair are the
changelog entry; what belongs here is how a confidently-argued refutation survived its
author and died at review.

**The claim.** [#392](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/392)
was filed — by this session — on one failure of `TestLongTimeToFirstTokenKeepsLease` under
full-suite load, reasoning that a lease genuinely taken by another brain and an extension
whose round trip merely did not finish are indistinguishable at that call site. The first
attempt at the issue rejected it on the keeper's own arithmetic: renewals tick at `ttl/3`
and each is bounded at `ttl - ttl/3`, so an attempt that exhausts its budget began a third
of a lease in and ran to the end of it — *"by the time an extend times out, the lease
really has lapsed"*. The keeper's existing comment said the same, which made the claim feel
confirmed rather than merely repeated. The proposed change was to widen the test's margin
and record the keeper as correct.

**Why it was wrong.** The arithmetic holds only for a punctual tick. A `time.Ticker`
buffers one tick and drops the rest, so a renewal that outlasts an interval is followed by
a tick *already waiting*, and the next attempt starts at once with a budget computed as
though it had not — landing its deadline inside the lease the slow renewal had just bought.
The invariant fails on every tick after the first slow one, which is exactly the loaded-host
condition the issue was filed under.

**How it was established, by three independent routes.** The Codex reviewer produced the
counterexample by construction (High). The verifier, dispatched against the same commit and
knowing nothing of that finding, reached FAIL by two further routes: a simulation of the
loop, and the real keeper against real Postgres, failing 3/3 once the contention window was
widened. This session then reproduced it with queued `SELECT … FOR UPDATE` row locks, which
fails against `main`'s keeper with the issue's own error string and passes with the fix.

**A negative result that mattered.** The first version of that reproduction passed 6/6
against the *buggy* keeper — the second lock raced the second renewal instead of queueing
behind it, so the bug had a window to escape through. A reproduction is only evidence once
it has been made to fail against the unfixed code. The version that replaced it bought its
ordering with sleeps, and Codex pointed out the remaining hole: on a slow enough host the
intended queue never forms, and the test does not fail — it silently stops testing. Both
orderings are now *observed* before the next step runs, a lock only once its goroutine
reports the grant and a queued renewal only once Postgres reports a backend waiting on a
lock, leaving sleeps to decide how long a lock is held and never who gets the row.

**A second refutation, this one the reviewer's.** Attacking the fix, Codex called the
anchor wrong: `bought` is stamped when `Extend` *returns*, later than the instant the
database bought the lease, so a budget can outlast the real lease. The observation is
exactly right; the conclusion drawn from it — anchor before the call instead — is
backwards, because the two directions are not symmetric. Anchoring late overstates the
lease, and an attempt that runs past an expiry it cannot see still cannot commit the turn:
settlement happens only once `Close` reports the keeper healthy, and `Complete`/`Requeue`
carry the lease as proof. (The overdue `Extend` itself may succeed — its guard matches the
timestamp it is replacing, not the wall clock, so an item nobody reclaimed is simply
re-extended. The first draft of this paragraph said otherwise, and the same reviewer caught
it.) Anchoring early understates it, and an
attempt then times out while the lease is still live — which is #392 itself. Applying the
suggestion and running the reproduction fails it 2/2 with *"the keeper abandoned a lease it
still held"*, the same signature `main` produces. It is recorded here because the code now
reads like an oversight and will attract the same correction again.

**What was not done.** The original 250ms flake was never reproduced (CPU saturation, six
runs; database contention, eight runs). The test's margin was therefore left exactly as it
was: the diagnosis that justified widening it — a merely tight test over a correct keeper —
is the diagnosis that turned out to be false, and widening a test after fixing the
mechanism it was pointing at only costs sensitivity. If it recurs, that is new evidence.

**What the fix does not cover, and the third reviewer who said so.** The GitHub Codex bot,
reviewing the same commit, raised a *different* mechanism: an `Extend` whose `UPDATE`
commits while the client's deadline fires renews a lease the caller never observes, so a
timeout still discards a turn whose row is in fact leased. It is right, and the measured
budget does not close it — the budget makes a timeout mean "this attempt outlived the lease
it was racing", which is safe, not "the item is free", which would be certain. Split out as
[#400](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/400) rather than
stretched into this PR: it is unobserved, and closing it means changing the keeper's
ownership protocol, not its arithmetic.

**The retry remains rejected, for a narrower reason than first written.** An extend that
fails *fast* — an exhausted pool, a reset connection — likewise returns while the lease is
live, and the keeper still kills the turn. The first draft called a naive retry *worse*
than nothing; the verifier corrected this, and it is right: `internal/brain/brain.go`
treats every keeper error identically, so the outcome would be unchanged, not worse. The
real constraint is that `Extend` identifies the claimant by the lease timestamp it replaces
(`WHERE … lease_expires_at = $2`) and updates `item.Lease` only on success, so a retry
after an ambiguous completion would present a stale timestamp and be told, wrongly, that
the lease was lost.

**And resynchronising is not the way out, though this record first said it was.** The
sentence that stood here told a future implementer to resynchronise the lease value before
retrying, while the current lease is still live. Triaging #400 showed that recipe is not
merely insufficient but dangerous, so it is corrected rather than left to be followed.
`Claim` reclaims an expired-active row **without** reassigning the id
(`internal/queue/queue.go:209-241`); only `Poll` rotates the identity (`queue.go:352`), so
#62's "every re-hand-out mints a fresh `work_` id" covers the BYOC wire lifecycle, not the
internal claim path that the brain and executor actually use. No migration adds an owner or
generation column, and `Extend`'s proof is a bare timestamp. So after an ambiguous
completion the row reads identically — `active`, with a future expiry — whether this
holder's own `UPDATE` committed or a rival reclaimed the item. Adopting the expiry found
there would let the adopting holder's next `Extend` *and its settlement* both match and
succeed, so two brains would believe they owned one item and the loser's settlement would
stop rolling back: a correctness bug where today there is only a wasted turn. The
live-lease guard does not save it, a rival's fresh lease being live by definition.

What #400 inherits is therefore a real fencing token — an owner or generation identity
threaded through `Claim`/`Extend`/`Complete`/`Requeue` — which is why it is triaged as
needing a plan, and why it stays unbuilt while the race remains unobserved.

---

## The docker wrapper's no-state rule, narrowed rather than kept (#390), 2026-08-13

A decision this repo made deliberately, recorded as a property in
[docs/history/2026-07.md](./history/2026-07.md) § *"`internal/sandbox` — the hands"*, and
reversed here in one specific direction. It is written down because the reversal is not
visible from the diff: the code it changes is four lines, and the reasoning it overturns
is a paragraph somewhere else.

**What was decided.** The docker exec wrapper keeps **no state inside the container**. The
stated reason: the first design's marker "let a command forge a timeout it never hit or
erase one it did" — the same section's review list names it the `/tmp` marker. A test
pinned it, asserting the wrapper's script
mentioned no writable path at all. Everything the sandbox knew about a command's deadline
came from outside: two probes of the command's process, run against the daemon on Exec's
own clock.

**What broke it.** [#390](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/390).
What was *observed* is one run of `TestShell/TimeoutDoesNotKillTheSession` reporting
`TimedOut=false` beside `ExitCode:137`, and taking 135 seconds for a command with a
one-second deadline. What is *inferred*, and stated in the issue as a hypothesis nobody has
reproduced under load, is the mechanism: the two probes each keep their own clock, and on a
loaded host they are scheduled after the watchdog's kill has landed, so the daemon answers
— correctly — that the process is gone, both terms come back false, and a genuine timeout
reads as an ordinary 137. The fix does not depend on that inference being the whole story.
It removes the dependence on probe scheduling altogether, which is why it was preferred to
tuning the probes: any cause that leaves both probes answering late is covered by a witness
that does not consult a clock.

**Why the reversal holds where the first design did not.** The first design used the mark
*as the evidence*, so erasing it hid a real timeout — the objection was exact and it was
right. Here the mark is one OR-term beside two host-measured ones, and the structure is
what makes it safe:

- **Forging it** requires also exiting 137, and buys a tenant a `TimedOut` label on its own
  tool call. It cannot extend a deadline, reach another session, or suppress anything.
- **Erasing it** returns the classification to the probes — precisely where this backend
  stood before the mark existed. This is the **residual limitation**, and it is not the
  same as harmless: a command can read `$3` out of its watchdog's argv in `/proc` and
  pre-create the directory so the `mkdir` fails `EEXIST`, or delete it before the daemon is
  asked, and its own punctual timeout then reads as a plain SIGKILL again. The honest claim
  is that suppression costs *that command* its timeout label and restores the pre-fix
  behaviour — not that erasure cannot hide a timeout. The mark repairs a **race** (probes
  scheduled late under load); against a command hostile to its own label it is best
  effort, which is the same standing the watchdog itself has always had.
- **`overran`** — the term that carries the deadline's actual guarantee — never reads the
  mark at all, so the guarantee is still measured entirely from outside the container, and
  no amount of mark tampering lets a command outlive its deadline or go unkilled.

Every term only ever *adds* a timeout, which is the property that lets in-container state
be weighed at all, and it is the same argument the Kubernetes backend already made when
[#95](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/95) and
[#110](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/110) cost it the same
classification. That backend's own comment noted the mark was "the thing docker keeps out
of its container on purpose"; that sentence is now out of date, and this section is why.

**What was not reversed.** The deadline is still enforced from outside, on Exec's clock,
because the watchdog is a process the command can kill. The wrapper still bakes no
writable path into its script — the state path arrives as argv, random per exec, so a
tenant cannot pre-create a mark for an exec that has not started. And the test that pinned
the old rule was not deleted: it was rewritten to pin the narrowed one, that the mark is
the *only* write the wrapper makes. A change that had merely stopped tripping the old
assertion — the mark's path is passed in rather than written in the script, so the original
string check would have gone on passing — would have left the repo with a documented rule
its code no longer followed.

**Alternative considered and rejected.** The issue itself proposed the cheaper fix: have
the pre-deadline probe read a "process gone" answer that arrives *after* the deadline as a
timeout, since it cannot then distinguish a command the watchdog killed from one that
finished early. It needs no container state at all. It was rejected because it trades one
misclassification for another under the same load: a command that exits early and leaves a
straggler holding the exec's output stream — `sleep 300 & echo started`, an ordinary agent
pattern, pinned by `TestAStragglerHoldingTheStreamIsNotTheCommand` — would start reading as
a timeout on exactly the loaded hosts this fixes. The mark leaves that command alone,
because the watchdog re-checks `kill -0` and never marks a command that is already gone.
Positive evidence beats a widened guess.

---

## Management-key inferences settled against the live console (#389) — three of five went against us, 2026-08-13

Plan 32 shipped with five INFERRED entries in docs/DIVERGENCES.md: behaviours the
2026-08-13 recording never probed, where this platform had to choose. #389 existed
to settle them against the real console, and it did — from the console's own page
context, the way the original recording was made. Probe keys were created for the
run and all archived afterwards; every `sk-ant-…` was redacted at capture time to
prefix-and-length, so no key material entered this repository, its issues, or its
commits.

**All five are now resolved, and three of them were resolved against us.** The
registry's INFERRED section loses all five entries — not because they were
confirmed, but because there is nothing left to infer and, after this change,
nothing left to diverge on.

| Inference | The reference's answer | Us, before |
|---|---|---|
| Is `archived` terminal? | `400 "Archived API keys cannot be updated."` | matched |
| Is re-archiving idempotent? | **`400`** — the same refusal | 200 no-op |
| Can a lapsed key be re-activated? | `400 "Expired API keys can only be deleted, not renamed or reactivated."` | matched |
| Is `"expires_at": null` accepted on create? | `200`, `expires_at: null` | matched |
| How does a *disabled* key past its expiry render? | **`expired`** | `inactive` |
| Is a past `expires_at` accepted on create? | **`200`**, born `expired` | `400` |

Two further divergences surfaced that were never registered because nobody
suspected them. The reference's expired-key refusal states its own rule — an
expired key *"can only be deleted, not renamed or reactivated"* — so **archiving
is the only operation a lapsed key admits**, where we had permitted the disable
and the rename too, on the argument that cleanup must not depend on the clock.
And an **empty patch** (`{}`) answers 200 with the unchanged resource, where we
answered `400 "at least one of status, name is required"`. ("Deleted" in that
message means `status: archived`; there is no DELETE verb — that route answers
405, as does a single-key GET.)

That 200 is a *live* row's answer, and the first review of the change caught the
gap it left. The tests asserted `{}` was refused on archived and on lapsed rows —
which is what the code does, the state guards running ahead of any shape check —
but neither shape had been probed, so an assertion about the reference rested on
nothing. Both were then measured rather than reasoned about: `{}` on an archived
key returns `400 "Archived API keys cannot be updated."`, and `{}` on a key minted
with a past `expires_at` — lapsed but not archived — returns `400 "Expired API
keys can only be deleted, not renamed or reactivated."` Both match what shipped.
The reviewer's finding was that a test could pass against a weaker implementation;
the more useful answer was that the test had been pinning an unmeasured claim, in
a change whose whole point was to stop doing that.

A third fact fell out of the same run and is now pinned in code: **`archived`
outranks `expired`** in the rendering. A key archived after its expiry had
already passed reads `archived`, not `expired`.

**The uncomfortable one.** Our original implementation refused *every* patch to an
archived row — exactly what the reference does. Three reviewers converged on that
as a defect in round 1 of #388, arguing that a retried Delete should not error,
and we changed it to a succeeding no-op. The reasoning was sound in the abstract
and wrong about this API. That is the fourth time in this plan's history that a
fix introduced the next round's finding, and the first time the finder was the
reference itself rather than a reviewer. It is also the cleanest argument in the
repository for CLAUDE.md's rule that a wire shape is never guessed: every one of
these six answers was cheap to obtain and three of them were unguessable.

What landed with this record: the derived-status precedence, the archived guard
widened to refuse everything, the lapsed guard narrowed to permit only archiving,
the past-`expires_at` refusal dropped along with the database-clock guard that
enforced it, and the empty-patch refusal dropped. The DIVERGENCES entries and
docs/self-hosted-security.md §10 are corrected in the same PR, and the plan-32
acceptance record below carries a pointer saying which of its observations the
code has since moved away from.

## Management API keys (plan 32, #378) — archived 2026-08-13, all three slices delivered (#385, #388, #391)

The slice that plan 31 could not ship. Its issuance surface was gated on a live
observation of the reference console's key-management dialect, which needed an
authenticated Anthropic console account; plan 31 declined to invent the dialect and
moved the work to #378 with the recording it needed written down. That recording
was made on 2026-08-13 (transcripts in #378's comments) and this plan is what it
bought.

- **Slice 1 — the lifecycle in the schema, no new surface** (#385). Migration 0024:
  `status` replacing `revoked_at`, plus `expires_at`, `partial_key_hint` and
  `created_by`, with `api_keys_one_live` **dropped** and a narrower
  `api_keys_one_live_unissued` created in its place — keyed on `created_by IS
  NULL`, so console-issued keys may share a name as the reference allows. `authenticate`
  gained its expiry check **in the query**, against the database's clock. Nothing
  observable changed for an existing deployment, which is what the green gate
  proved. Two defects were found and fixed in review rather than shipped: the
  key-masking rule anchored on the value's *last separator*, which for base64url —
  whose alphabet contains `-` — published most of a minted key against an unsalted
  hash; and `EnsureAPIKey` adopted an existing row without clearing `created_by` or
  `expires_at`, which would have put the bootstrap key outside its own uniqueness
  index and let the process boot over a credential `authenticate` already refused.
- **Slice 2 — the console surface** (#388). Three admin-gated routes under
  `/api/console/organizations/{org}/workspaces/{workspace}/api_keys`: issue
  (resource plus `raw_key`), list (a bare JSON array, archived rows included),
  update status or name. Five CONFIRMED and five INFERRED DIVERGENCES entries, the
  inferences tracked by #389. Its four review rounds are recorded two sections
  below.
- **Slice 3 — docs and acceptance** (#391). §10 of
  [docs/self-hosted-security.md](./self-hosted-security.md), and the acceptance run
  recorded immediately below.

## Management API keys (plan 32, #378) — acceptance against real Casdoor tokens and the real `ant` CLI (run 2026-08-13) — ✅ passed

Plan 32's slice-3 criterion, quoted rather than paraphrased: *"an acceptance run
that issues a key over the console API, drives `/v1` with it, disables it and is
refused, re-enables it, archives it and is refused again"*. Every step below is
that chain, driven over real HTTP against the shipped `deploy/compose` stack with
`--profile iam`, on a database Casdoor had never seeded. **31 assertions, 0
failures.** No manual database edits: every operator action is an HTTP call.

> **Parts of the record below no longer describe the code**, and are left standing
> because this is the record of a run rather than a description of the system.
> #389 later measured the reference and moved us. Two observations the run made
> are now reversed: a repeated archive answers **400** rather than 200, and a past
> `expires_at` is **accepted** rather than refused with *"expires_at must be in the
> future"*. Two refusal messages quoted verbatim below are no longer the ones the
> code emits — *"is archived, which is permanent; use "inactive" for a disable you
> can undo"* and *"expired at its expires_at and cannot be re-activated; issue a
> new key"*. And one rule tightened in a direction the run never probed: a lapsed
> key now admits **archiving alone**, where the rationale recorded below — that
> cleanup must not depend on the clock — had also been read as licensing the
> disable and the rename. The section immediately above has the detail.

**The tokens are real.** The seeded application carries `authorization_code` and
`refresh_token` and no password grant, so the run scripts the full
authorization-code + PKCE exchange against Casdoor's own OP endpoints, as plan 31's
acceptance did. Both tokens decode as the platform requires — `iss` byte-equal to
`IDENTITY_OIDC_ISSUER`, `aud=["map-console-dev"]`, and
`groups=["map/platform-admins"]` / `["map/platform-read"]`.

- **The machine lane is untouched and the human lane is live.** `GET /v1/agents`
  answers **200** to both `x-api-key` and the admin's Casdoor token.
- **The surface is admin-only.** Viewer LIST → **403**
  `{"message":"this route requires the admin role","type":"permission_error"}`;
  viewer ISSUE → **403**. Admin LIST → **200**, and the body is a **bare array**,
  not an envelope.
- **An admin issues a key.** The response carried the whole resource plus a
  `raw_key` of 56 characters behind `sk-map-api01-`, hint
  `sk-map-api01-f9x...nM7Q`, and a `created_by` that is the admin's own principal —
  `{"id":"principal_jx3rwe9a1sacy8yhfh4mv9z6","type":"principal"}`. The audit trail
  names the human, which is the reason the identity lane exists. (This step and the
  agent create below asserted on the returned resource rather than the status line,
  so their success is transcript-proven and their `200` is code-backed. Every other
  code quoted in this record was captured with `-w '%{http_code}'`, except the
  `401`s in the `ant` paragraph, which are the CLI's own printed error output —
  verbatim in its transcripts, just not `-w`'s.)
- **The issued key drives `/v1`.** `GET /v1/agents` → **200**, and `POST /v1/agents`
  minted `agent_thm8jh8c862tymp57ce41k44`. A read and a mutation, on a credential no
  operator seeded.
- **Disable is reversible.** `{"status":"inactive"}` → **200**, and the key is then
  **401** on `/v1` (`invalid x-api-key`). `{"status":"active"}` → **200**, and it is
  **200** on `/v1` again. This is the sequence the round-3 review found a hole in;
  it now behaves as documented end to end.
- **Archive is permanent.** `{"status":"archived"}` → **200**, `/v1` → **401**.
  Un-archiving → **400** *"is archived, which is permanent; use "inactive" for a
  disable you can undo"*; `inactive` → **400**; renaming → **400**; **re-archiving
  → 200**, still rendering `archived` — the idempotent retry, not an error about a
  state that already holds.
- **The env-var-managed key is listed but not the console's to change.**
  `{"status":"inactive"}` on the `CONTROLPLANE_API_KEY` row → **400** *"is managed
  by CONTROLPLANE_API_KEY; rotate it by restarting the control plane with a new
  value"*, and that key still authenticates afterwards.
- **Expiry is the caller's, and the database's clock enforces it.** A past
  `expires_at` → **400** *"expires_at must be in the future"*. A key minted with
  `expires_at` 45 seconds out worked while live, was **401** on `/v1` once it
  lapsed, listed as **`expired`** (derived, never written), refused re-activation
  → **400** *"expired at its expires_at and cannot be re-activated; issue a new
  key"* — and **archiving it still succeeded**, because cleanup must not depend on
  the clock.
- **The listing never carries a secret.** Five rows including archived and expired
  ones; zero occurrences of `raw_key` or of any `sk-map-api01-` value; only masked
  hints. Two rows share the name `acceptance-runner` and two share `short-lived` —
  residue of an earlier partial run, and an unplanned demonstration that live keys
  may share a name, which is exactly what `api_keys_one_live_unissued` was narrowed
  to permit.
- **A real `ant` authenticates with it.** `ant` 1.21.0, built from the read-only
  `anthropic-cli` checkout, across **three** console-issued keys — one per run, each
  archived at the end, so no run inherited another's credential. On the first
  (`apikey_9grz…`), `ant beta:agents list --base-url http://127.0.0.1:8080` returned
  the agent JSON at exit **0**, and archiving that key over the console API made the
  same command fail with `401 … {"message":"invalid x-api-key",
  "type":"authentication_error"}`. On the second (`apikey_wz3a…`), the full
  reversibility ran against `beta:agents list`: disabled → 401, re-enabled →
  succeeded, archived → 401 for good. On the third (`apikey_nhx5…`), `ant
  beta:agents create --name plan32-ant-created --model 'id: claude-sonnet-5'` minted
  `agent_mmcr66adag155sty2ta2szrf` at exit **0** — the mutation, so the CLI is not
  merely reading. The whole lifecycle observed through the reference client rather
  than through curl.
- **One property measured because §10 asserts it.** With `IDENTITY_MODE=oidc`
  live, the management `x-api-key` still reaches the admin-gated routes — LIST
  **200**, ISSUE **200** — because `requireRole` binds the human lane only. The
  minted row's `created_by` is then
  `{"id":"apikey_m9p5qs6wggbgypyb87d95397","type":"api_key"}`: the issuing key's
  row id, typed from its own prefix. So "a management key can mint management
  keys" is a measured fact rather than a reading of the code, and §10 says so
  where an operator will meet it.

Machine state: the stack ran under its own compose project (`map-accept32`) and was
torn down with `--profile iam down -v`, volumes included. The three containers
predating the run (`managed-agent-console`, `kind-control-plane`, and an exited
jaeger) were left untouched, and `docker ps -a` afterwards showed exactly those
three.

## Management API keys (plan 32, #378) — slice 2's four review rounds: two defects the fixes created, and three findings refuted, 2026-08-13

Worth recording because of *where* the defects came from: the two that mattered
were not in the original implementation. Each was introduced by a fix for the
previous round's finding, which is the failure mode a single review pass cannot
see.

**Round 1 → the archived-resurrection defect.** Making `archived` terminal was
correct and is now an INFERRED divergence. The implementation of it refused *every*
patch to an archived row, which broke the idempotent retry: an operator repeating a
Delete got a 400 that both errored on a state already holding and advised
`inactive`, the state they had just declined. Three reviewers reached it
independently, which is what the contemporaneous reply on #388 records: the Codex
CLI pass, this repo's `verifier` subagent, and then the Codex GitHub bot raising it
a third time on the same diff. The fix lets a repeated archive fall through to a
no-op write, as `RevokeEnvironmentKey` already did for the same reason.

**Round 3 → the lapsed guard was reachable around.** The refusal to re-activate a
lapsed key was itself added in round 2, and it computed its condition with the same
SQL the *rendering* uses — including a `status = 'active'` conjunct. The two
expressions answer different questions. The rendering asks "does this row currently
show expired" and ands in the status deliberately, so a disabled key reports the
operator's action rather than the clock; the guard has to ask "would activating
this row produce a dead key", which the current status has nothing to do with. So
the guard was reachable around by the sequence an operator actually performs:
disable, forget, re-enable after the expiry passes — answering **200** with
`status: "expired"`, verbatim the answer-shape the refusal existed to eliminate.
CodeRabbit and the verifier found it independently; the verifier then reproduced it
against a real Postgres, and after the fix confirmed the pinning test can fail by
restoring the conjunct in a throwaway copy. Eleven further orderings and body
combinations were driven against the fixed guard and none got around it.

Worse than that bug, and the reason the verifier called it a concern rather than a
note: the comment beside the guard said the refusal was "registered INFERRED beside
the archived one" and **no such entry existed**. A false claim that a divergence is
registered is worse than an unregistered one, because it tells the next reader the
checking has been done.

**Three findings refuted with evidence rather than "fixed".**

- CodeRabbit flagged a "future-dated August 13, 2026" reference. That is the date
  the recording was made, and the transcripts are on #378.
- It read the `ON CONFLICT` finding as environment-key rotation. Wrong subsystem:
  `environment_keys` issuance has no `ON CONFLICT` at all.
- It prescribed refusing to boot when `EnsureAPIKey` adopts a console-issued key.
  Declined: that strands the control plane behind a state only a direct database
  edit clears, and protects nobody — setting the variable at all requires the
  deployment access that could equally configure a fresh value. The adoption logs
  a warning naming the previous status instead — which **slice 2's round-4
  verification** drove directly (its verdict is a comment on
  [#388](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/388), where
  this provenance chain ends: no raw boot log of that measurement survives),
  booting a control plane over an archived
  console-issued value and reading `configured management key already existed as a
  console-issued key … previous_status=archived` out of its log, then booting over
  an archived env-var-managed value and finding no such line. That is the
  provenance: a verification run, not the acceptance run above, which never
  restarted a control plane and so could not have observed a boot-time adoption at
  all.

**One tracking gap, found by the verifier in round 4.** All five INFERRED entries
cross-linked #389 while the issue enumerated only four of them — someone working it
from its body would have closed it leaving two inferences unconfirmed. The issue
now enumerates all five, in six experiments.

**A note on the local gate.** Across this plan the verifier never obtained one
uninterrupted green `make verify` on this host, and said so rather than composing
silence: `internal/mcp` flaked in 6 of 8 executions (#380, measured this session
against the "~40%" in its title) and `internal/sandbox/shell` flaked once with a
signature that turned out to be worth its own issue (#390 — a genuine timeout
misclassification, not merely a slow test). Slice 3's own verification then turned
up a **third** signature on a run where the first two both passed:
`internal/brain`'s `TestLongTimeToFirstTokenKeepsLease` failed with
`lease keeper: queue: extend work_…: context deadline exceeded` at 153s of package
time and passed in 3.1s re-run alone (#392). Every one of the three packages was
byte-identical to `main`, every one passed alone, and CI was green on every merged
SHA — so the pattern is the host under full-suite load, not the changes. The
verdicts were composed from per-package results plus the coverage total plus CI
(90.81% on slice 3's run), and each verifier report labelled that composition
explicitly rather than reporting a green it had not obtained.

---

## Console SSO and RBAC (plan 31, #56) — slice 1's dependency decision: rejected alternatives, 2026-08-12

Plan 31's Scope decision 1 originally named `coreos/go-oidc` + `golang.org/x/oauth2`
as the verifier's foundation. Slice 1 overturned it, and the plan text is amended
in the same PR. What a changelog cannot hold is why the two alternatives lost, so
it is recorded here.

**Evaluated and rejected: `coreos/go-oidc`.** Two of the plan's own requirements
are unimplementable on it. Its `oidc.NewRemoteKeySet` caches until an unknown
`kid` forces a refetch and exposes no TTL, so a key the provider has *removed*
keeps verifying indefinitely — while the same plan's Architecture section requires
a minutes-order bound on exactly that. And `oidc.Config` carries a single
`ClientID`; the library's documented route to multi-audience or `azp` checking is
`SkipClientIDCheck: true` plus a hand-written audience policy. Between the two,
adopting it would have meant writing the security core anyway *and* carrying a new
module (which itself depends on go-jose and `x/oauth2`).

**Evaluated and rejected: hand-rolling compact-JWS and JWK parsing.** The
argument for it was attack surface — that linking a JOSE library puts an HMAC
verifier and a JWE decrypter in the binary, kept unreachable only by our
allowlist. That premise is false for this binary: `go list -deps ./cmd/controlplane`
already prints `github.com/go-jose/go-jose/v4` *and* its `/cipher` package,
reached through `internal/blob/gcs` → `cloud.google.com/go/storage` → grpc/xds →
`go-spiffe`. Hand-rolling would therefore have removed nothing from the binary
while adding roughly 250 statements of security-critical parsing. Positively,
go-jose closes algorithm confusion *structurally* rather than by discipline: the
allowlist is a required parameter of `jwt.ParseSigned`, so `alg:none` and HS256
are refused inside the parser before any key lookup.

**What the adopted route still leaves to us**, and therefore what slice 1's tests
pin: go-jose skips `exp` when the claim is absent and never checks `sub`, knows
nothing of `azp`, does not fetch or bound the lifetime of a key set, does not
bound an RSA modulus or require an odd exponent, does not parse `key_ops` at all,
and does not enforce a JWK's declared `alg` against the JWS header. One more is a
correctness trap rather than a gap: `jose.JSONWebKeySet` has no set-level
`UnmarshalJSON`, and `JSONWebKey.UnmarshalJSON` errors on any `kty` it cannot
build, so decoding a set whole would let one entry a provider is entitled to
publish (an X25519 encryption key, a future `kty`) fail the *entire* set and take
every signing key with it — a boot failure, then uniform 401s once the cache
expires. `parseKeySet` therefore decodes per entry and skips the unusable, as
RFC 7517 §5 directs.

**One deliberate extension of the plan's enumerated boot errors.** The plan lists
five startup rejections; slice 1 adds a sixth — setting
`IDENTITY_PROXY_HEADER`, `_ISSUER`, `_KEYS_URL` or `_ALGS` while
`IDENTITY_PROXY_PRESET=gcp-iap` fails startup rather than being ignored. Those
four variables *are* the verification parameters, and silently discarding an
operator's attempt to change them is not a warning-grade event.

---

## Console SSO and RBAC (plan 31, #56) — slice 1's security review: what was fixed, and the two findings refuted, 2026-08-12

The Codex security pass over `internal/identity` returned eight findings. Six were
confirmed against the source and fixed in the same PR; the changelog carries the
resulting behaviour. Recorded here is only what a changelog cannot hold — the two
findings **refuted with evidence** rather than fixed, so the next reviewer does not
re-raise them, and the one fix whose reasoning is a judgement call.

**Refuted: `azp` should be checked whenever present, and extra audiences should be
rejected.** The reviewer's reading is OIDC Core's SHOULD for `azp` and its
MUST-not-accept-untrusted-audiences rule. The implemented rule — `aud` must contain
the configured audience, and `azp` must equal it when `aud` names several — is the
plan's settled Scope text, and it is not weaker where it differs. A token whose
`aud` names this console *is* a token whose issuer named this console as an
audience; that is the authorization signal, and `azp` is advisory beside it.
Tightening to "`azp` must equal our audience whenever present" would break the
ordinary deployment where the audience is an API/resource identifier while `azp` is
the client id — Keycloak and Entra both emit exactly that shape. Rejecting a token
that additionally names a third-party audience protects that third party, not this
platform, and only after `azp` has already established the token was issued to us.
Revisit only with a real provider that motivates it, behind a config flag rather
than a silent tightening.

**Refuted: `key_ops` duplicates and `use`/`key_ops` inconsistency should be
rejected** (RFC 7517 §4.3's MUST-NOT-duplicate). `usableKey` already requires
`use ∈ {"", "sig"}` *and* `verify ∈ key_ops` when `key_ops` is present, and this
package has exactly one use for a key: verifying a signature. A duplicated
`"verify"`, or an extra `"encrypt"` alongside it, therefore authorizes nothing
further — the violation cannot change the decision. Enforcing it would add an
unbounded O(n²) scan over a provider-controlled list (`maxKeys` bounds the number
of keys, nothing bounds one key's `key_ops`), which is a denial-of-service seam
opened to enforce a producer-conformance rule with no consequence here.

**A judgement call, stated rather than buried: the `kid` is still logged.** The
review flagged writing an attacker-controlled `kid` to a Debug line. It is kept,
truncated to 64 bytes, at Debug only, because it is the single diagnostic that
answers "which key did the provider rotate to" when logins start failing; slog
escapes control bytes, and the truncation is what bounds the log-volume
amplification the reviewer was pointing at. Everything else the review named —
URL query strings and userinfo in logs and errors — is now redacted, including
inside transport errors, whose `*url.Error` quotes the URL verbatim (the fix wraps
the *cause*, so `errors.Is` still reaches `context.DeadlineExceeded`).

**A note for whoever reads the mutex code.** The panic-safety fix in `keySet.get`
is not defensive habit. `net/http` recovers a panicking handler per connection, so
a panic unwinding out of `get` while the cache mutex was held would leave the
process **alive** with every later authentication blocked forever on a lock nobody
can release. `TestGetReleasesTheMutexWhenTheFetchPanics` uses `TryLock` precisely
so the bug fails the suite instead of hanging it.

### The second review round: the claim-name reading, and three go-jose boundaries

The Claude reviewer (Opus 5) and a Codex re-review of the fixes each returned a
further set. Both independently reached the same conclusion about the RSA
exponent ceiling, and it is the most instructive item in the slice.

**The exponent ceiling was checked one step too late, and the first fix was
nearly useless.** `usableKey` bounded `pub.E`, but go-jose has already reduced the
published exponent to `int(big.Int.Int64())` (`encoding.go:193`) by then, and
`Int64` on an oversized value yields its **low 64 bits**. A published exponent of
`2^64 + 65537` therefore arrives as an ordinary `65537` — odd, ≥ 3, far under the
ceiling — and no rule applied to `pub.E` can see the difference. The bound only
ever caught the `(2^31, 2^63]` window, and on the `linux/arm` target `make
crossbuild` compiles it is dead code entirely, since `int` is 32 bits there. The
real fix reads the raw `e` member from the JWK, where the published value is still
intact. `TestParseKeySetRejectsATruncatedExponent` asserts the truncation happens
before asserting the entry is skipped, so the test cannot quietly stop testing
anything if go-jose changes.

**The claim-name reading was reversed, and the reasoning is worth keeping.** Slice
1 resolved a dotted claim name as a path ONLY, never as a flat key, to stop an IdP
surface that lets a user place a claim literally named
`resource_access.console.roles` from outranking the real nested one. That rule
silently broke the namespaced-custom-claim convention Auth0 *requires* and Okta
and Entra also use: `https://corp.example/roles` was split into
`["https://corp", "example", "com/roles"]`, resolved to nothing, and denied every
human on those providers with nothing in any log to say why. The fix keeps the
security property and drops the breakage by deciding from the **configured name**
rather than from the token: a URI-shaped name (one containing `://`) is a flat
key, any other dotted name is a path. The rejected alternative was "try the flat
key, fall back to walking" — that one lets the *token* choose the reading, which
is the escalation the original rule existed to prevent.

**A null `crit` is accepted, deliberately.** `"crit":null` decodes to a nil
`*json.RawMessage`, and go-jose's `sanitized()` skips those (`shared.go:416`), so
it never reaches `ExtraHeaders` and the presence rule never sees it. This is safe
because a null confers nothing to hide: go-jose's own `getCritical` returns no
names for it, and `getB64` returns the default `true` for a null `b64`, so neither
member can declare an extension or change how the payload is read. The test
asserts the acceptance rather than omitting the case, so a go-jose change fires
there instead of in production.

**Refuted: the discovered `jwks_uri` should be https-only, with no loopback
exception.** The exception is not what protects production — the dial guard is,
and `fetch.go` already states that with the guard wired, http-to-loopback URLs are
dead in production, fail-closed. The exception is live only when an operator has
replaced `Config.HTTPClient` and thereby taken the guard off, which the field's
own documentation calls owning the consequence. It is also load-bearing for the
fake provider, which every discovery test reaches over http loopback.

**Refuted: `redactURL` should blank the path too.** It removes the userinfo, the
query, the fragment and the opaque form, and keeps scheme, host and path on
purpose: `key set fetch failed` is only actionable if it says *which* endpoint,
and `/oauth2/v3/certs` versus `/.well-known/jwks.json` is the whole diagnostic.
Nothing can distinguish a secret path segment from a routing one, so blanking it
would cost every legitimate reader the answer to protect a shape no provider in
the compatibility set uses. The package doc now states that boundary exactly
instead of claiming more than the function delivers.

**Refuted: `email_verified` should be required.** Many providers never emit it, so
requiring it would deny every human on those deployments. Nothing in this package
derives authority from the email — roles come from the roles claim alone — so an
unverified address is a display string, not a privilege. What the fix does add is
a length bound on `Email` and `DisplayName`, since those are the only fields whose
length the token alone decides and a later slice persists them; and a note at the
assignment telling that slice that anything MATCHING or LINKING on the address
must make its own verification decision, because on an IdP with self-service
profile attributes — Casdoor included — the user chooses that string.

### The third round: the PR's own bots, and a truncation that defeated itself

Ten review threads on [#369](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/369)
from the Codex connector and CodeRabbit. Eight were confirmed and fixed; the
changelog carries the behaviour. Three are worth recording.

**`truncate` cut on a byte boundary, which reintroduced the failure it existed to
prevent.** The bound on `Email` and `DisplayName` was added in the second round so
that an oversized claim could not turn a valid login into an insert failure
against a bounded column. Cutting `s[:n]` through a multi-byte UTF-8 sequence
leaves trailing bytes that are not valid UTF-8, and a UTF8 PostgreSQL database
refuses exactly those on insert — so any non-ASCII display name whose cap landed
mid-rune produced the insert failure the bound was for, and slice 2 is what makes
that reachable, since slice 2 is what persists the fields. Proven rather than
argued: with the byte cut restored, `TestVerifyBoundsTheProfileFieldsOnARuneBoundary`
reports `DisplayName` ending `\xe4\xb8`. Trimming the incomplete tail is enough
because the input is always valid UTF-8 already — these strings come out of
`encoding/json`, which replaces malformed bytes with U+FFFD while unquoting — and a
genuine U+FFFD survives, since `DecodeLastRuneInString` reports it with size 3
while a stray byte comes back with size 1.

**Google mints two spellings of its issuer, and exact comparison rejected one.**
`iss` is compared exactly, on purpose. Google documents an ID token's `iss` as
"always `https://accounts.google.com` or `accounts.google.com`" and emits both,
while OIDC Core §2 requires an issuer identifier to be an https URL — so the
scheme-less form cannot be configured here, and a deployment pointed at Google
would boot cleanly and then 401 an arbitrary half of its logins. The allowance
added is one hard-coded pair, keyed on the exact configured string: no operator
input widens it, no other deployment reaches it, and both spellings denote one
issuer identity whose key set is the same either way. A provider that violates the
spec differently gets an issue, not a second special case.

**Refuted in mechanism, fixed in substance: the log-presence guard.** CodeRabbit
argued that `TestLogsCarryNoCredentials`'s four presence assertions could be
satisfied by another test's lines, because `slog.SetDefault` is process-wide and
"parallel subtests of earlier tests resume while this sequential test runs". That
mechanism does not hold: a top-level test that calls `t.Parallel()` parks until
every sequential top-level test has finished, so nothing in this package writes to
the sink concurrently with that test today. What *was* wrong is what the finding
pointed at sideways — the sink's own comment claimed every assertion below it was
about absence, and the presence block is not. Both are now fixed: the comment says
what the two kinds of assertion are, and each presence check is qualified by the
fixture's own key-set host, so the guard no longer depends on a scheduling
property a later `t.Parallel()` could quietly remove.

## Console SSO and RBAC (plan 31, #56) — slice 2's review: the lane leak, and one finding refuted, 2026-08-12

The Codex pass over the identity lane returned five findings. Four were confirmed
against the source and fixed in the same PR; the changelog carries the behaviour.
Recorded here is what a changelog cannot hold.

**The machine-first rule leaked through a repeated header field.** Both
dispatchers asked "is an x-api-key offered?" with `Header.Get`, which returns
only the first value of a field HTTP permits to repeat. A request carrying an
empty `x-api-key` ahead of a wrong one, plus a valid admin token, therefore read
as offering no key and was served through the **human** lane — proven rather than
argued: with the old predicate restored,
`TestARepeatedAPIKeyHeaderCannotChangeLanes` reports 200 where the invariant says
401. It is not a privilege escalation (the human lane still verifies the token
and still checks the role), but it is precisely the lane confusion the ordering
exists to prevent, and it would also deny a legitimate machine caller whose proxy
prepended an empty field. The fix pairs a predicate that reads every value with a
`requireAPIKey` that refuses a duplicate outright, because the bug's real cause
was answering "which value wins" in two places with two different rules.

**A fix from the previous round created a defect one layer down.** Accepting
Google's two issuer spellings (third round, slice 1) made `Verify` treat them as
one issuer while `principals` — keyed `(issuer, subject)` — treated them as two,
so one human could hold two rows and two ids depending on which spelling their
token carried. `Identity.Issuer` now reports the configured value. Worth stating
because the second defect was invisible while slice 1 stood alone: nothing
persisted the field until slice 2 existed.

**Refuted: a grandfathered environment key shaped like a JWT is a defect.** The
reviewer noted that a pre-0021 key whose operator-chosen value happens to carry
two dots is routed to human verification and denied there. That is the
documented, accepted residual — `dualAuth`'s own comment, the changelog entry,
and docs/self-hosted-security.md §6's instruction to reissue those keys all
describe it, and the outcome is a 401, fail-closed, never an over-authorization.
The reviewer's extension — that such a value might *also* be a currently valid
token from the deployment's IdP — requires an operator to have set an environment
key to a real JWT, and would grant only that human's own authority.

**The verifier then caught the fix's own documentation overclaiming.** Running two
control-plane binaries side by side — `origin/main` and the branch — against the
same Postgres, it measured the one request shape where "with identity disabled the
platform is byte-for-byte what it was" is false: a repeated `x-api-key` field. The
refusal above lives in `requireAPIKey` and is **not** conditional on a verifier, so
`valid` + a second empty field went from 200 to 401, and `empty` + `valid` changed
its message, in a deployment that never configures identity. Four documents and two
code comments asserted the unqualified promise; one of them —
docs/ARCHITECTURE.md's `server.go` row — described the duplicate refusal in detail
and then contradicted itself two sentences later. The check was left unconditional
and the claims were scoped instead: gating it on the verifier would leave the same
malformed request answered differently in two deployments, and would re-open the
gap between "which value selects the lane" and "which value authenticates" that
the round's first finding was about. `TestIdentityDisabledIsUnchanged` now pins the
exception with no verifier present — red on a copy with the refusal removed
("message = \"missing x-api-key header\", want the ambiguous-credential refusal").
The general lesson is the one the round keeps repeating: a hardening that can only
deny still changes behaviour, and a doc that rounds it off to "nothing changed" is
the defect, not the hardening.

---

## Console SSO and RBAC (plan 31, #56) — acceptance against a real Casdoor token and the real `ant` CLI (run 2026-08-12) — ✅ passed; slice 5 moved to #378

Plan 31's slice 6 criterion, quoted rather than paraphrased: *"Compose stack with
the `iam` profile up; a real token minted from the bundled Casdoor (scripted
code+PKCE against its OP endpoints); the chain proven end to end: viewer token
403s a mutation → admin token issues an environment key over the console API → a
real `ant beta:worker poll --environment-key …` authenticates with it."* Every
step below is that chain, driven over real HTTP against the shipped
`deploy/compose` stack with `--profile iam`, on a database Casdoor had never
seeded. No manual database edits: every operator action is an HTTP call, and the
only processes touching Postgres are the control plane and Casdoor themselves.

**Tokens are real, and minted the way a console would.** The seeded application
carries `authorization_code` and `refresh_token` and no password grant, on
purpose, so there is no shortcut to a token — the run scripts the full
authorization-code + PKCE exchange (S256 challenge, verifier held back until the
code exchange) against Casdoor's own OP endpoints. One mechanic is worth
recording because it cost a cycle and no struct would have revealed it: Casdoor's
login handler reads the OAuth parameters from the **query string** (Beego's
`c.Input()`, which merges query and form values but not a JSON body) while the
credentials ride the JSON body. Sending everything in the body answers
`Grant_type:  is not supported in this application` — an empty grant type read
from a query string that carried nothing, which reads like a misconfigured
application and is not one.

Both minted tokens decode exactly as the platform requires:
`iss=http://localhost:8000` (byte-equal to `IDENTITY_OIDC_ISSUER`),
`aud=["map-console-dev"]` (the application's client id), and
`groups=["map/platform-admins"]` / `["map/platform-read"]` — strings spelled
`organization/name`, which is what `IDENTITY_ROLE_MAP` keys on and the reason the
`groups` claim is mapped rather than `roles`.

- **The machine lane is untouched.** `x-api-key` on `GET /v1/agents` → **200**,
  with the identity lane fully configured. This is the property every slice of
  this plan promised and the one a regression would be quietest about.
- **Admin does what an admin does.** `POST /v1/agents` → **200**
  (`agent_hx6dxtf16m43369mpmpmqzta`); `POST /v1/environments` → **200**
  (`env_rkjmgx2tdf1k7v7ta89jbsan`), both authenticated by nothing but the Casdoor
  token.
- **Viewer is refused the same mutations.** `POST /v1/agents` → **403**, body
  `{"error":{"message":"this route requires the developer role","type":"permission_error"},"request_id":"req_…","type":"error"}`
  — the wire error envelope, with the refusal in its `error` member;
  `POST /v1/environments` → **403**. The same token reads: `GET /v1/agents` →
  **200**. Role, not authentication, is what separates the two.
- **The console key surface is admin-only.** Viewer on
  `GET /api/oauth/organizations/default/environments/{env}/tokens` → **403**.
- **Admin issues an environment key over that surface** → **200**, an
  `sk-map-env01-…` of 56 characters, and the list then shows it.
- **A real `ant` authenticates with it.** `ant` 1.21.0, built from the read-only
  `anthropic-cli` checkout, run as
  `ant beta:worker poll --base-url http://127.0.0.1:8080 --environment-id … --environment-key …`:
  no error, no output, and exit **124** — the timeout killing a poller that was
  still long-polling, which is what an idle environment looks like from the
  worker side. The control plane logged no 401 in that window. Because silence is
  weak evidence, the same lane was then called directly:
  `GET /v1/environments/{env}/work/poll?beta=true&block_ms=500` with that key →
  **200**, body `null` (no work, correct for an idle environment).
- **Forged and revoked keys are refused, through the real CLI and directly.** A
  forged key → `ant` stops with `401 … {"message":"invalid environment key",
  "type":"authentication_error"}`; the direct call agrees, **401**. Revoking the
  real key (`POST …/{token_id}/revoke` → **204**, list drops to zero) then makes
  that same key **401** on both paths. Per-host revocation works against a
  credential a human minted with a token from the bundled IdP.

**Slice 5 did not ship and was not faked.** The api-key issuance surface is gated
in the plan on a live observation of the reference console's key-management
dialect, which needs an authenticated Anthropic console account and creates real
credentials in it. The observation could not be made in this session, so the
plan's own provision applied — the slice moved to **#378** with the recording it
needs written down, rather than shipping an invented dialect. Plan 31 archives on
what it shipped: slices 1–4 and this acceptance.

Machine state: the stack was brought up and torn down with `--profile iam down -v`;
the containers predating the run (`managed-agent-console`, `kind-control-plane`,
an exited jaeger, and one created-but-not-started container) were left untouched.

---

## Console-issued environment keys (plan 30, #43) — acceptance against the real `ant` CLI (runs 2026-08-10 and 2026-08-11) — ✅ #43 passed; the deferred work-item-pull criterion passed too (#363 closed)

#43's acceptance criterion, quoted rather than paraphrased: *"An operator can
issue and rotate an environment key, and a real `ant beta:worker` authenticates
with it end to end (no manual DB edits)."* That was driven against a real `ant`
binary built from the read-only `anthropic-cli` checkout, with **no manual
database access or edits** — every operator action a curl against the console
API, and the only process touching Postgres the control plane itself. Per-host
revocation, below, is stronger evidence than the criterion asks for; it is
recorded because it is the property the whole plan exists for, not because #43
required it.

The stack was deliberately **not** the shared `deploy/compose` one: an unrelated
compose stack had been running on `:8080` for 45 hours and predates this work, so
this run used its own throwaway `postgres:16-alpine` on an ephemeral port plus a
`cmd/controlplane` built from the branch on `127.0.0.1:18080`. Both were removed
afterwards; `docker ps -a` matched its start-of-run baseline exactly.

- **Issue.** `POST /api/oauth/organizations/default/environments/{env}/tokens`
  with `{"name":"laptop-01"}` → **200**, headers `Cache-Control: no-store` and
  `Pragma: no-cache`, body `{"access_token":"sk-map-env01-…" (56 chars),
  "expires_in":31536000}`. The matching `GET` rendered
  `{id: envkey_…, name, created_at, expires_at (+1 year)}` with
  `{total:1, limit:100, offset:0, has_more:false}` — **no secret, no hash**.
- **The real CLI authenticates on that key alone.**
  `ant beta:worker poll --base-url http://127.0.0.1:18080 --environment-id … --environment-key …`
  ran clean with no output — a long poll on an empty queue — and the platform's
  own view confirmed it positively rather than by silence: `GET …/work/stats`
  reported a non-zero **`workers_polling`**, which is decisive because
  `Queue.RecordPoll` fires only on the *authenticated* poll path
  (`internal/queue/stats.go`). The same route answered
  `401 "missing Authorization: Bearer environment key"` to the *management* key,
  so the worker lane is demonstrably the lane being exercised. (The number read
  `2` at that moment, not `1`: `workers_polling` counts distinct `worker_id`s
  that polled inside a 30-second window, and an immediately preceding 25-second
  `ant` run — launched without `--worker-id`, so the CLI minted a default one —
  was still inside it. Only one worker was live. The count is evidence that a
  real `ant` completed an authenticated poll, not a headcount.)
- **Revocation reaches a running worker.**
  `POST …/tokens/{id}/revoke` → **204, 0 bytes**; the same key on
  `…/work/stats` immediately → **401 `invalid environment key`**; the running
  `ant` process printed that same `authentication_error` envelope and exited; the
  listing went empty.
- **Per-host revocation — the property the old model could not offer.** Two keys
  (`build-host-a`, `build-host-b`), two real `ant beta:worker poll` processes
  started with explicit `--worker-id host-a` / `host-b`, and here
  `workers_polling: 2` *is* a headcount: the earlier ids had aged out of the
  30-second window. Revoking **host-a alone** → 204. Host-a's process exited
  on the auth error; **host-b's kept running**, its log still 0 bytes, its key
  still **200** on `…/work/stats`, and the listing showed only `build-host-b`
  surviving. Under the retired rotate-on-mint invariant (migration 0013), issuing
  host-b's key would already have revoked host-a's.

**Deferred on 2026-08-10, met on 2026-08-11 — this plan's exit criterion, not
#43's.** The plan's slice-3 wording was stronger than the issue's: "poll *until
it pulls a work item*". That step was not run on 2026-08-10, so plan 30 archived
with one of its own criteria carried to
[#363](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/363). The
run below closes it. The paragraph that follows records why it could not run at
the time, kept as written rather than rewritten in hindsight.

The blocker is causal, not procedural. `queue.Poll` serves `kind = 'tool_exec'`
only (`internal/queue/queue.go:314`), and while three production sites enqueue
that kind — the brain after a model turn emits `agent.tool_use`
(`internal/brain/brain.go`), the API after a client confirmation releases a
pending platform tool (`internal/api/events.go`), and the executor when web-tool
settlement chains remaining sandbox work (`internal/executor/webwork.go`) — **none
can manufacture one without a pre-existing `agent.tool_use`**, and a client cannot
post that event itself (`internal/events/inbound.go` rejects it; confirmations and
results are cross-checked against outstanding uses). The API's own enqueues at
session-create and message-post are `model_turn`, which no worker polls. So an
item needs a model turn, and this checkout has no `model-providers.json` — STATE.md's
standing "blocks every session" task, not something this plan introduced.

Scope of what was still unproven on 2026-08-10, stated precisely rather than
minimised: `TestConsoleIssuedKeyDrivesAWorkerAndIsRevocable` pins the issued-key
authentication boundary, the empty-poll route and revocation over real HTTP — it
does **not** pin queue delivery, work serialisation, or the tool runner. Those
three, plus the real CLI's ability to claim an item, execute the tool in-process
and settle it back on a console-issued key, were what #363 remained to prove.

### The deferred step, run (2026-08-11) — ✅ #363 closed

**The premise the deferral rested on was wrong, and checking it was the first
step.** The 2026-08-10 note gave the blocker as "this checkout has no
`model-providers.json`". **That statement was incorrect when it was written**, and
it is recorded as such rather than reconciled: `.worktreeinclude` has listed
`.env` and an unrooted `model-providers.json` since `d3b5b0d`, the unrooted
pattern matches `deploy/compose/model-providers.json`, and this worktree was
created on 2026-08-10 at 22:16:48 with both files already in it — before the run
that said they were absent. Only a repo-*root* `model-providers.json` was ever
missing, and nothing reads one: the brain takes `MODEL_PROVIDERS_PATH`, compose
mounts the `deploy/compose/` copy. So the deferral's stated cause did not hold;
the model route was configured the whole time, `.env` carrying a live
`anthropic`-protocol route (`https://api.minimaxi.com/anthropic`, `MiniMax-M3`)
and `deploy/compose/model-providers.json` the same endpoint and key.

A read of the files still could not settle whether the credential worked, so the
live tier was opted into for one turn: `RUN_LIVE_MODEL_TESTS=1 go test
./internal/provider/anthropic/ -run TestIntegrationRealEndpoint` → **PASS in
2.17s**, `real turn ok: 2 output tokens, stop_reason=end_turn, text="OK"`. (The
"placeholder (real endpoint, fake key)" STATE.md task, which the 2026-08-10 note
cited as the blocker, was introduced by `61d947d` and names the **GCP staging**
Secret Manager version — `deploy/gcp/README.md` says so explicitly. #363 needs no
GCP.)

Same discipline as the first run: **not** the shared `deploy/compose` stack —
that one had been up for three days — but a throwaway `postgres:16-alpine` on an
ephemeral port, plus `cmd/controlplane` on `127.0.0.1:18081` and `cmd/brain`,
both built from the branch, with the real `model-providers.json` handed to the
brain by `MODEL_PROVIDERS_PATH`. `ant 1.21.0` was built from the read-only
`anthropic-cli` checkout. Everything was torn down afterwards and `docker ps -a`
diffed **identical** to its start-of-run baseline.

- **Issue, again over the console API.**
  `POST /api/oauth/organizations/default/environments/{env}/tokens` with
  `{"name":"worker-01"}` → **200**, `Cache-Control: no-store`, `Pragma: no-cache`,
  `{"access_token":"sk-map-env01-…" (56 chars), "expires_in":31536000}`. The `GET`
  rendered `envkey_axrn…` / `worker-01` / `created_at` / `expires_at` (+1 year)
  with `{total:1, limit:100, offset:0, has_more:false}` — no secret, no hash.
- **A model turn produced the `agent.tool_use` no client can create from
  scratch.** (The precise claim, since the paragraph above states it correctly
  and an earlier draft of this line did not: a client confirmation *is* a wire
  path that enqueues `tool_exec` — `internal/api/events.go` — but only by
  releasing a tool use the brain already emitted.) A
  `self_hosted` session created with an `initial_events` `user.message` ("Run the
  shell command: `echo a363-worker-pull-ok`") was born `running`; four seconds
  later the event log read
  `user.message → session.status_running → span.model_request_start →
  agent.tool_use → span.model_request_end`, the `agent.tool_use` carrying
  `{"name":"bash","input":{"command":"echo a363-worker-pull-ok"},
  "evaluated_permission":"allow"}`. That is the causal step the deferral was
  about: the brain emitting it is what enqueues a `tool_exec` a worker can poll.
- **The real CLI claimed, executed and settled it — on the console-issued key
  alone.** `ant beta:worker poll --base-url … --environment-id …
  --environment-key … --worker-id a363-host`, verbatim from its log:

  ```
  claimed work    component=work-poller work_id=work_744trc3xef9k5jd5mdvw0nek work_type=session
  executing tool  component=session-tool-runner tool=bash tool_use_id=sevt_1t0v2wxqk8ycrhr01m3smr65 custom=false
  dispatched tool tool=bash is_error=false posted=true
  session idle after end_turn; stopping   max_idle=1m0s
  ```

  The queue item's own row shows the full lease lifecycle — `acknowledged_at`,
  `started_at`, a `latest_heartbeat_at` a minute later, then
  `stop_requested_at`/`stopped_at` and `state: "stopped"`.
- **The result came back through the same credential, and the model read it.**
  `user.tool_result` carried `text: "a363-worker-pull-ok\n\n"`, `is_error: false`,
  and a `tool_use_id` equal to the `agent.tool_use` id above; the following
  `agent.message` answered *"It printed: `a363-worker-pull-ok`"*. Session ended
  `idle`, usage 1171 in / 45 out.

So all three of the previously unproven properties — queue delivery, work
serialisation, and the tool runner — are now driven end to end by a real `ant`
binary authenticating with nothing but a console-issued key. **No *manual*
database edits were made**, which is the issue's criterion: the platform wrote
the session, event and work-item rows described above, as it must, and the only
manual database access in the whole run was a read-only `pg_stat_activity` query
issued *after* the acceptance, to check what had connected.

**What this run does not cover.** Plan 30's slice 3 says "bring up
`deploy/compose`", and neither run did — both stood up throwaway infrastructure
instead, deliberately, to leave a long-running stack alone. The behavioural
criterion is met; the stated procedure was substituted, and that substitution is
recorded here rather than smoothed over.

## GCP continuous delivery — the mode-2 build → deploy → smoke sequence, by hand (run 2026-08-08) — ✅ passed

Every step `.github/workflows/deploy.yml` performs was run by hand against the real
project first, and the workflow then written to match what ran. Against the staging
coordinates the workflow reads — `GCP_PROJECT_ID`, `GCP_ZONE`, `GKE_CLUSTER` —
namespace `map`, release `map`, **mode 2** (Cloud SQL + GCS + Cloud KMS behind
`existingSecret: map-platform`).

- `gcloud builds submit --config deploy/gcp/cloudbuild.yaml --service-account=…cd-deployer@…`
  built all four images — controlplane/brain/executor as three tags on one build,
  plus `--target gate` — in **2m16s**, into the `ARTIFACT_REGISTRY` prefix. Two
  things had to be fixed for it to run at all and are the `Fixed` half of this change:
  `DOCKER_BUILDKIT=1` on both docker steps (the Dockerfile's
  `FROM --platform=$BUILDPLATFORM` is BuildKit-only) and
  `options.logging: CLOUD_LOGGING_ONLY` (mandatory once a build names its own
  service account).
- `helm upgrade --install map … -f deploy/gcp/staging-values.yaml --wait --atomic
  --timeout 10m` returned 0; **all three Deployments** (controlplane, brain,
  executor) reached Ready.
- Smoke against the controlplane LoadBalancer, port 8080:
  `GET /v1/agents?limit=1` answered **200** with the management key and **401**
  without one — the round trip that also proves Cloud SQL is reachable.
- The console half (`managed-agent-console`) deployed to the same cluster:
  rollout green, its own LoadBalancer up, in-pod `/api/health?deep=1` **200**
  with `platform.reachable: true` against the in-cluster control plane
  `http://map-managed-agent-platform-controlplane.map.svc.cluster.local:8080`,
  anonymous `GET /` **307** to `/login`, public `/api/health` **200** with
  `login_gate: true`.

**Not covered by this record:** the GitHub Actions workflow had not itself run.
Nothing could drive `push: main` until the PR merged, and the `workflow_dispatch`
path is deliberately refused from any other ref. Two further gaps were recorded
rather than closed — `deploy/gcp/README.md`'s "Continuous delivery" section
carries both: the WIF provider did not yet assert `assertion.ref`, and
`model-providers` holds a placeholder (real endpoint, fake key) so the sequence
could be proven without inventing a credential.

**Since closed, in that order.** The WIF provider now asserts `ref` and
`ref_type` alongside `repository_owner`. The workflow then ran on merge and
failed in under a second at `gcloud builds submit` — which stages source as the
*caller*, not as the build's `--service-account`, so every by-hand submission
above (called by a human with Owner) had proven nothing about that path, and two
rounds of granting the *build* identity more could not have fixed it. #349 drops
Cloud Build from CD and builds on the runner, which needs only the
`artifactregistry.writer` the job already held; `cloudbuild.yaml` stays as the
manual path's definition. Run 31260884425 on `0c01e14` was green in 3m41s, build
through smoke, and all three components run its images at `ready 1/1`. The
`model-providers` placeholder is the one gap still open — no session will run
until it is replaced.

## Changelog and history slimming (plan 28) — archived 2026-08-08, both slices delivered

Slice 1 (#337): the `archive` subcommand (`make changelog-archive`) moved
§ [0.2.0] (5,507 lines) and § [0.1.0] (915) to docs/changelog/ behind index
stubs — CHANGELOG.md 6,448 → 35 lines, with an independent inverse
re-composition against the pre-move document byte-identical. Review
hardening: the Codex High finding (a verbatim copy breaks every relative
link from docs/changelog/ — 123 in § [0.2.0]) became the byte-reversible
link re-base with its inversion guard anchored on the written bytes; the
Opus review panel proved the original round-trip guard constructively true
and it gained a reachable firing input (a stub quoted mid-entry); the
fence-aware boundary scan closed a pseudo-section corruption vector no
reviewer's first pass had named. Twelve guard mutations run red in scratch
copies across the slice's two review rounds; the one deliberate survivor —
dropping the inversion comparison — is disclosed in the PR as
defense-in-depth.

Slice 2 (the archiving PR): the 31 sections of 2026-07 (1,327 lines) moved
to docs/history/2026-07.md by a one-off scripted manual move —
recomposition from the two output files reproduced the pre-split file
byte-for-byte as performed, a property of the move rather than a standing
invariant — links re-based the same way, and 96 references to moved
sections re-pointed: 69 citations in docs/DIVERGENCES.md, 24 across
thirteen archived plan files, one each in docs/ARCHITECTURE.md and
README.md, and one Go comment (internal/sandbox/k8s) — the wider net after
the single-reviewer pass caught the quoted-grammar sweep missing every
variant phrasing, the v0.2.0 lesson again. Citations inside archives stay as written
(docs/changelog/ bytes are pinned by the inversion guard; a docs/history/
file records its era) and resolve through this file's pointer paragraph. The split kept
1,823 of the file's 3,150 lines in place — far above the plan's ~1,000
estimate, because 2026-08 alone is the heaviest month yet; the figure
falls as it archives in turn.

Rejected en route: a citation sweep at changelog-archive time (the stubs
keep anchors resolvable — plan 28 decision 4), and a shipped
HISTORY-split tool (speculative for a two-file split).

## Release management (plan 27) — archived 2026-08-08, all four slices delivered (#332 #333 #334 #335, tag v0.2.0)

docs/plan/27_release-management.md is archived complete: the fragment changelog
(`changelog.d/` + `tools/changelog`, slice 1, PR #332), version embedding
(`internal/version` + ldflags through the Dockerfile and worker `--version`,
slice 2, PR #333), the tag-triggered publishing pipeline (`release.yml`
sequencing the `release-*` Make targets, slice 3, PR #334), and the v0.2.0 cut
itself — release PR #335, the annotated tag, and the acceptance below (slice 4,
closed by this archiving PR). The first cut absorbed the legacy `[Unreleased]`
backlog into `§ [0.2.0]` byte-identically and re-pointed twelve evidence
citations across DIVERGENCES/HISTORY — the ritual's prescribed search was
widened to a fixed-string `grep -rF` after the narrow pattern missed seven
variant spellings; both lessons now live in docs/RELEASING.md step 6.

Review hardening that outlived the PRs: the GitHub Release step converges via
create-if-missing → `gh release edit --draft=false` → `upload --clobber`,
because `gh release view` matches drafts (confirmed against gh v2.97.0 source)
and a cancelled first run would otherwise leave an unpublished draft that
re-runs green-exit into; every pure check (strict-SemVer tag validation,
`release-tag-check`, `release-chart-check` with the version/appVersion
lockstep, the notes render) runs before anything publishes, so a half-bumped
chart or an unrenderable notes body aborts an unpublished run; the Dockerfile
build stage is pinned to `$BUILDPLATFORM` with Go cross-compiling to
`$TARGETOS/$TARGETARCH` (probe: the amd64 target built natively on an arm64
host in 12s, x86-64 ELF extracted); and `TestNotesCapChargesTrailer` pins the
clamp trailer into the cap budget, mutation-proven red.

Decisions evaluated and rejected. Byte-reproducible release tarballs:
tar/gzip timestamp normalization is platform-conditional complexity for a
guarantee nobody asked for — the shipped contract is "a re-run converges the
release with equivalent, not byte-identical, artifacts", worded in release.yml
and RELEASING.md. An editorial figure refresh inside the release PR's assembly
(flagged P1 in review): reverted, so the assembled 6,448-line CHANGELOG.md
reproduces byte-for-byte from the fragments as they stood on the release
commit's parent (`4eb4245` — the release consumed them, so that parent is
where an auditor finds the sources) — fragment-verbatim is the precedent the
first cut sets.

---

## v0.2.0 release acceptance — the tag-triggered pipeline end to end (run 2026-08-08) — ✅ passed

The annotated tag `v0.2.0` on the release PR's squash commit (`7a8dbdc`) drove
release.yml run 31208475933 green on the first attempt — every step: SemVer
resolve, `release-tag-check` + `release-chart-check`, the notes render,
multi-arch `release-images` (linux/amd64 + arm64), `release-chart` to OCI,
`release-binaries`, and the create→edit→upload Release step. Verified:

- **GitHub Release** published, not draft: four worker tarballs
  (linux/darwin × amd64/arm64) plus sha256sums; notes body 85,528 characters —
  the 446,309-byte un-clamped notes body clamped at a Keep-a-Changelog group
  boundary under the 120,000 cap, ending with the tag-pinned CHANGELOG link.
- **Worker binary leg:** the darwin/arm64 asset downloaded from the Release,
  `shasum -a 256 -c` OK, `./worker --version` → `0.2.0`.
- **GHCR visibility:** the five packages (controlplane/brain/executor/gate
  images + the chart) started private per the `GITHUB_TOKEN` default and were
  flipped public in the org package settings — the documented one-time step
  (browser-driven; the anonymous probe method was first validated against a
  known-public package after an earlier "flipped" report turned out to be a
  different package). All five answer anonymous `tags/list` 200.
- **kind install from the published artifacts:** `helm install` from
  `oci://ghcr.io/opensdlc-dev/charts/managed-agent-platform --version 0.2.0`
  into a fresh namespace on the local kind cluster — six pods Ready; the three
  platform pod specs name their per-component images and all three resolve to
  one digest (`sha256:e3d6997c…`), the "same digest, three names" design
  observed in its production form; the controlplane startup log reports
  `version=0.2.0` (slice 2's embedding, live in-cluster); `GET
  /v1/agents?beta=true` through a port-forward with the chart-Secret API key
  answers the wire shape `{"data":[],"next_page":null}`. Uninstall and
  namespace deletion clean.

**Amended 2026-08-24 (#425).** The one property this run did not check was link
form, and the body it certified left 59 repo-root-relative targets across 39
paths relative. The release page was never broken by them: github.com's
renderer resolves such a target against the repository at the tag — observed
before the replacement, when the page already served all 59 rewritten to
`/…/blob/v0.2.0/…`, and reproducible on any release body still carrying one.
What did carry them unresolved is the raw body the REST API and mirrors serve.
It has been re-rendered from the tagged section by the post-#423 tool and
replaced in place, so the published body is now 89,363 characters against the
85,528 recorded above: identical bar those 59 targets, clamped at the same
group boundary. No release here still shows the form, so the observation is
recorded rather than left to be re-derived.

---

## The root-owned write residue (#310) — a rejected design and five review rounds, 2026-08-06

The fix that shipped (PR #311) is in CHANGELOG.md. What a changelog cannot hold is the
design that was written, measured, and withdrawn, and why five rounds were needed.

**Rejected: a cleanup exec as uid 0.** The first three iterations cleaned the root-owned
temporary with `docker exec -u 0 rm -f`. It worked, and it was abandoned because the
credential cannot be bounded: `docker exec -u 0` starts with `AT_SECURE=0` and runs a
binary and shared libraries the *image* supplies, so what root does there is decided by
the image — and, wherever an agent can write part of that filesystem, by the agent. Each
attempt to bound it by closing a channel was answered by another channel, found by a
different reviewer:

| Channel | Found by | Measured effect |
|---|---|---|
| `bash -c` sources `ENV BASH_ENV` | Codex (P1) | the agent's own file ran **as uid 0**, twice per cleanup |
| loader honours `ENV LD_PRELOAD`, no shell needed | the implementer, checking the neighbouring case | `ld.so` loaded a sandbox-writable object |
| image leaves `/bin/rm` (or its libraries) agent-writable — the *premise* the other four sit on, not a fifth variable to close | Codex (P1) | the credential runs the agent's binary |
| `ENV LD_DEBUG_OUTPUT`, with every other hook emptied | verifier | root-owned file written at an image-chosen path |
| `/etc/ld.so.preload` | verifier | honoured with no environment variable at all — no `Env` entry can close it |

The conclusion is the decision: emptying a list of variables is not a bound, and not
running anything is. The verifier **failed** the uid-0 iteration on a blocker — an absolute
claim in `docs/self-hosted-security.md` that it measured false — which is what forced the
question rather than a softer wording.

**Round-5 hardening.** The exec-free design was itself wrong twice, both found in review
after the verifier had passed it:

- Folding the daemon-side emptying into every shed put it on the branch where the rename's
  *exec* failed. That error cannot distinguish "the exec never started" from "the exec
  started and the stream dropped", so a `mv` may be in flight; emptying the temporary under
  it lets the `mv` land zero bytes on the target the caller is simultaneously being told was
  untouched. Silent data loss, traded for a residue the container destroys anyway. Codex.
- Both cleanups inherited the failed write's context, so a write that failed *because* its
  caller went away shed nothing — the case that creates the residue was the case that
  skipped the code removing it. Codex and CodeRabbit independently. The verifier measured
  the difference on a real daemon: 896 KiB of agent payload stranded in a root-owned
  directory without `context.WithoutCancel`, zero with it.

**Refuted, with measurement rather than argument** (Codex P1, round 5): extraction over a
hardlink an agent plants at the temporary path. `protected_hardlinks` refuses the sandbox
user's link outright (`Operation not permitted`), and even with the link staged as root the
daemon's extraction **replaces** the name rather than writing through it — the 16-byte 0600
victim was byte-for-byte intact afterwards, its link count 2 → 1.

**Left open deliberately**: the bulk write's own residue, tracked as
[#316](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/316).

**Model-pin deviation, disclosed**: the verifier's pinned `claude-fable-5` hit its usage
limit partway through, so rounds 3–5 ran it under an explicit `opus` override — a departure
from `.claude/skills/run-reviews/SKILL.md`'s pinning rule, forced by quota rather than
chosen, and recorded here because a weaker verifier certifying this class of change is
exactly what the pin exists to prevent.

---

## Sandbox teardown (plan 24, #64) — slice 5: the idle-TTL tier, and the plan's acceptance (2026-08-06)

**Acceptance record.** The plan's acceptance section ran in full against the compose
stack (all three server images rebuilt from the branch; `EXECUTOR_REAP_INTERVAL=5s`,
`EXECUTOR_SANDBOX_IDLE_TTL=30s`; the brain on a real Anthropic-protocol endpoint,
model `MiniMax-M3`) and against a host executor on `SANDBOX_BACKEND=k8s` pointed at
the kind cluster — ten rows, all green:

1. **Idle TTL round trip** (the plan's headline): a session wrote
   `/workspace/keep.txt` and `cd /tmp`, idled 30s → marker `ready`, blob recorded,
   container gone within a reap interval; the next `user.message` restored the file
   *and* the persistent shell's cwd (`pwd` printed `/tmp`), marker `consumed`.
2. **Deliverable continuity**: a `user.define_outcome` session's harvested
   `report.txt` was downloadable via `GET /v1/files/{id}/content` before the reap and
   **still listed and downloadable after the resume and the next grading cycle**, with
   the restored `/mnt/session/outputs` still carrying it.
3. **Archive** removed the sandbox within reap intervals.
4. **A gated session's archive** removed the sandbox *and* the gate container, and
   its live `session_gate_tokens` row went revoked in the same reap.
5. **Delete** removed the marker row and wrote the tombstone — after the run's one
   real find, below.
6. **Running-session guards**: archive and delete of a running session both 400.
7. **Mid-turn kill** (`docker rm -f` during a `sleep`): the turn settled, and the
   resume ran on a **fresh** workspace — the pre-kill file was gone, no marker
   involved (D6: a crash-recreate never rewinds).
8. **The #29 wedge heals**: `EXECUTOR_IMAGE` flipped to `debian:stable` and the
   executor restarted; the old-image sandbox reaped after the TTL, and the session
   completed on the new image with its checkpointed workspace restored.
9. **K8s TTL round trip** on kind: pod reaped, the in-pod exec capture path produced
   the checkpoint, the resume restored file and cwd, marker `ready` → `consumed`.
10. **K8s archive**: the archived session's pod reaped (the slice-2 RBAC `list` verb
    and label-selected delete, live).

**The run's find** — merged unit-green, caught live: the `session_checkpoints`
row's assigned cleanup owner (slice 4's PR review had put it in the reaper's deleted
tier) could never fire in the tier's steady state. The delete row first FAILED: a
session the idle tier had already reaped no longer appears in any `Owned` listing, so
no reap pass ever visits it and the row lingers forever — the unit test had the
sandbox still owned, hiding the gap. Fixed by moving the delete into the API's
deleting transaction (no FK cascades it), the reaper's tier keeping only the blob
delete; the mutation (`DELETE` swapped for `SELECT 1`) fails the API test, and the
delete row then passed.

**Review hardening, landed in the same PR.** The verifier returned PASS WITH FINDINGS
(gate from scratch, both suggested mutants reproduced, the asks-first ordering argument
independently judged sound against `AppendInTx`; its two note findings were wording
fixes). The dual review (Codex `gpt-5.6-sol`, FAIL with 2 P1 + 4 P2; the Opus 5 review
workflow, 18 agents, 10 confirmed / 3 refuted after per-finding adversarial verification)
then converged on one headline from both sides:

- **D8 implemented broader than the plan wrote it** (Codex #5, workflow P1 ×3
  dimensions). The first cut degraded to reap-without-checkpoint on *every* capture
  failure; the plan sanctions exactly two — over-budget and an unreadable sandbox. A
  transient object-store 5xx, a full executor spool disk, or a failed marker write
  therefore destroyed the workspace permanently, with one WARN line as the only trace —
  and one store blip spanning a pass would have hit every idle-past-TTL session on the
  endpoint. Now only sandbox-caused failures degrade (`ErrCheckpointTooLarge`, and
  export/archive failures wrapped in a sentinel); anything outside the sandbox aborts
  the reap so the sandbox stays owned and the next pass retries — the deleted tier's
  own blob-delete rule. This also repairs D4's lossless-wake argument (Codex #1): a
  `user.message` committing after the under-lock recheck now always finds either a
  ready checkpoint to restore or a still-alive sandbox; the loss window is confined to
  sandbox-caused capture failures (a transient export failure among them — D8
  sanctions any exec/archive failure, so a retry that might have succeeded is
  deliberately not attempted).
- **An idle reap racing a concurrent DELETE resurrected the marker and blob forever**
  (Codex #3, workflow P1). `deleteSession` takes no advisory lock, and the marker
  table carries no FK by design, so a delete committing between the reaper's
  re-classification and the marker upsert left an orphaned `ready` row and blob no
  reap pass would ever revisit. The marker write now inserts only while the session
  row exists, under `FOR KEY SHARE` — it either lands before the delete (whose
  in-transaction sweep removes it) or waits the delete out, inserts nothing, and
  withdraws the just-uploaded blob; a *failed* withdraw aborts the reap instead of
  reporting the benign sentinel, so the next pass's deleted tier (the tombstone is
  already written) retries the blob delete before reaping — the verifier's own
  post-round finding, adopted with its test.
- **A `stopped` work item can still have a physically executing claimant** (Codex #2).
  An interrupt cancels the row instantly, but the executor only notices at its next
  lease renewal — with a short operator TTL the tier could reap under a still-running
  tool. The owed-work exclusion now also holds for an item stopped within the
  executor's lease TTL (`stopped_at`, which normal completion never sets).
- **Helm folded the numeric `0` — the documented disable value — into "unset"**
  (Codex #6, workflow P2 ×4, reproduced by render on both sides): `with`-truthiness
  omitted the env var and the binary armed its 24h default, silently enabling
  destructive reaping the operator disabled. The template now reads the value through
  `toString`; the five-case render matrix (unset / null / `0` / `"0"` / `"30m"`) is
  pinned in the PR record.
- **The workflow's test dimension confirmed the ordering gap this session then
  watched happen**: `fakeProvider.Reap` left `Export` working, so swapping capture
  after `Reap` — the exact mutant one workflow probe agent left on disk mid-review —
  kept the whole package green. The fake now mirrors both real backends (a destroyed
  sandbox answers `ErrNotFound` until re-provisioned), which makes the happy-path test
  kill that mutant; the owed-work exclusion's per-session scoping and the asks-first
  read order each gained their own test (a recording `Querier` pins the statement
  order — the commit-timing race itself is not constructible, the ordering that closes
  it is).

Refuted with evidence, no change: the API's post-commit best-effort blob delete
(pre-existing narrow window, already WARN-logged; the proposed in-transaction delete
would strand a `ready` marker over a missing blob and wedge every future provision),
and two claims against the delete-path tests' framing that named no reachable defect.

**Plan 24 progress summary (archived).** Five slices, five PRs: #314 (wire guards:
archive/delete of `running` 400), #315 (`Owned`/`Reap` on both backends + contract
rows), #317 (the reaper's terminal tiers: tombstone-evidenced deleted, archived,
terminated — cloud-only — under the per-session advisory-lock protocol), #319 (the
checkpoint/restore engine: `Export` on both backends, the one-measure budget, the
marker table, replace-first restore), and the archiving PR (the idle-TTL tier with
its three exclusions, blob-less disablement, D8 degradations, and this acceptance).
The user's four requirements all hold as built: a running session's sandbox is never
reaped (guards + lock + under-lock recheck), idle-timeout reap checkpoints and the
resume restores (#28's workspace-continuity half), delete reaps (interrupt
deliberately does not — the TTL is the backstop), and the whole mechanism is
horizontally scalable (endpoint-local `Owned`, idempotent `Reap`, no cross-replica
coordination).

## Sandbox teardown (plan 24, #64) — slice 4: the checkpoint/restore engine

**Review hardening, landed in the same PR.** The verifier returned PASS WITH FINDINGS
(gate green; its own discretionary seam probe streamed a real Docker `Export` through
the real re-rooting walk and extracted the result into a second real container,
byte-exact); its findings — the hardlink name-loss trade and the symlink `Linkname`
handling undocumented — were fixed as code commentary and an ARCHITECTURE clause
before review. The dual review (Codex `gpt-5.6-sol`, 7 findings; the Opus 5 review
workflow, 21 raw findings adversarially verified to 11 confirmed / 10 refuted) then
drove a hardening round whose headline both reviewers found independently, the
workflow four times over:

- **Capture and restore metered different quantities against the same cap** (Codex #1;
  workflow C1/C4/C5/C7, three dimensions converging). Capture charged only regular
  members' `hdr.Size`; restore bounds the whole decompressed framed tar — headers,
  padding, the 1024-byte trailer, directories and symlinks charged nothing at capture —
  so a cleanly-captured checkpoint could be arithmetically unrestorable forever:
  restore fails, D6 leaves the marker `ready`, and every next provision reaps the
  replacement sandbox and fails again. Fixed with one measure on both sides — a
  `limitedWriter` meters the framed tar stream itself during capture.
  `TestCaptureAndRestoreShareOneBudget` pins the arithmetic (3600 content bytes under
  a 4096 cap must now fail at capture, where the old accounting accepted them).
- **A K8s export could die behind a syntactically complete archive** (Codex #2; C2):
  `tar.Reader` stops at the end-of-archive blocks and never issues the read that
  surfaces the pipe's `CloseWithError`, so a tar that exited non-zero was captured and
  marked ready. Capture now drains each export stream to its close. The finding's
  probe half was real too: the single-level existence probe answered `ErrFileNotExist`
  for a root hidden behind an unsearchable ancestor — replaced with a walking probe
  (deepest existing ancestor: searchable means genuinely missing, an unsearchable
  directory is an error), after the first draft of the walk was itself caught
  misreading the shell-state root, whose parent legitimately does not exist before the
  session's first bash run.
- **A ready marker on a storage-less executor now fails the provision closed**
  (Codex #3). The workflow's adversarial verify had refuted its own variant (R2) on
  the single-executor scenario, but Codex's mixed-rollout scenario stood: a blob-less
  executor provisioning fresh leaves the marker standing, and the next blob-configured
  executor restores over the session's new work.
- Codex's remaining confirmed findings: `Reap` inside restore can self-deadlock the
  documented two-connection floor via the pool-backed gate-token revoker — the floor
  is now 3 with the pin count named (#4); a configured workdir could alias the other
  checkpoint roots, double-charging shared subtrees, and `/tmp` as workdir would
  archive the restore staging file — `ValidateWorkdir` refuses overlap in either
  direction at startup (#5); `EXECUTOR_CHECKPOINT_MAX_BYTES` at `MaxInt64` overflowed
  restore's `cap+1` into an empty accepted spool — the cap is bounded at 1 PiB (#6).
- Workflow C3 (a root an agent replaced with a plain file got a malformed trailing
  slash) and C6 (the detected wire `Format` carried into the re-rooting writer) were
  fixed — C6's mechanics verified by experiment before trusting either side: Go's
  writer emits GNU LongLink itself, so the GNU shape never errors, but a strict-ustar
  member whose re-rooted path exceeds every 100+155 prefix split fails exactly as
  claimed. Both shapes are pinned (`TestCaptureCarriesAGNULongNameExport`, which also
  proves the `@LongLink` pseudo-member never leaks into a checkpoint, and
  `TestCaptureCarriesAUSTARDeepPathExport`, the discriminating one).
- The confirmed test gaps each gained a test: re-capture re-arms a consumed marker
  (C8), Docker `Export`'s ownership guard on the fake daemon — no archive request
  escapes (C9), the `too_large` metric outcome (C10), and the absolute-path branches
  of both walkers (C11 — restore's guard is the load-bearing one; capture's is
  redundant with its top-level-directory check, which the mutant run demonstrated).

Refuted with the verify agents' own evidence, unchanged: the missing FK/delete on
`session_checkpoints` (R1/R9 — the delete tier's blob-then-tombstone path owns
cleanup), the marker upsert riding the pool rather than the lock's connection (R3),
restore-then-materialize ordering (R4 — the platform's pre-existing mount contract),
unbounded concurrent restore spools (R5 — the executor loop is serial), the absent
capture→restore round trip (R6 — refuted as harmless then, and this round's symmetry
test performs it anyway), `markerState` swallowing its query error (R7), the
stopped-container wait loop accepting any Exec error (R8, the same substance as
Codex #7 — the compound scenario cannot occur on this code; an optional nit, left),
and `EXECUTOR_CHECKPOINT_MAX_BYTES=0` as documented-legal (R10 — nothing documents it).

Ten hardening mutants killed, each by exactly the test built for it: the budget check
bypassed, the drain removed, fail-closed reverted to the silent skip, the `Format`
reset removed (the GNU-shape test survives that mutant — Go's writer self-handles
LongLink; the ustar deep-path test is the killer), the non-dir root's trailing slash
restored, the marker upsert's `DO UPDATE` flipped to `DO NOTHING`, `ValidateWorkdir`
gutted, `too_large` collapsed into `error`, restore's absolute-path guard dropped, and
docker `Export`'s ownership check dropped. Not constructible: the K8s walking probe's
unsearchable-ancestor branch (unit-unreachable without a cluster, and kind's root-user
images make the permission scenario moot — the contract suite covers the missing-root
answer), and the cmd startup guards (main glue, outside the tested surface by the
convention slice 3 recorded).

**On the PR, the bots added a round of their own.** The Codex bot's two P2s both
confirmed against source and fixed: the K8s probe's `[ -e ]` follows symlinks, so a
root an agent replaced with a **dangling symlink** read as missing and capture
silently dropped the entry — the probe gained a `-L` arm and the behavior became a
shared contract row, both backends. The first fix documented Docker as resolving the
terminal symlink daemon-side; the verifier's direct measurement on its next pass
refuted that (`GET /containers/{id}/archive` on a dangling symlink answers 200 with
the link member itself, running or stopped) — Docker never had the gap, and the row
pins both. And
restore's validation accepted any relative path, letting a corrupted blob's
`etc/profile` land on the sandbox's real `/etc` through `tar -C /` — members are now
confined to the three durable roots (an `outside-roots` tamper case pins it). Both
fixes' mutants killed: the `-L` arm dropped → the dangling-symlink contract row red
on kind, the root restriction dropped → the tamper case red. CodeRabbit's four: three fixed (the
`markerState` helper now fails the test on a real query error instead of reading as
"no marker" — its R7 shape, refuted as a defect, adopted as hygiene; the `rerootInto`
doc block re-attached to its function after the hardening edit left it hanging on
`limitedWriter`, its stale budget clause cut; the K8s export tar's stderr captured
into the exit-code error) and one answered in place: `session_checkpoints` rows for
deleted sessions were assigned a cleanup owner — first the reaper's deleted tier;
slice 5's acceptance run then corrected that to the API's deleting transaction (a
session whose sandbox the idle tier already reaped never reappears in `Owned`, so a
reaper-owned row would linger forever) — see the slice-5 record.

## Sandbox teardown (plan 24, #64) — slice 3: the reaper's terminal tiers

**Review hardening, landed in the same PR.** The verifier returned PASS WITH FINDINGS
(gate green at 90.44% coverage, all five slice mutants independently reproduced in a
scratch copy, a live probe on the real binary — deleted/archived containers removed
within one 2s interval, the idle one untouched, terminated removed after the flip); its
two concerns (the `EXECUTOR_REAP_INTERVAL` knob missing from cmd/executor's env block
and the compose README table) were fixed before review. The dual review then reshaped
the slice's deleted tier:

- **The deleted tier now requires a tombstone** (Claude review, confirmed twice, the
  round's headline). As landed, `classifyForReap` mapped `ErrNoRows` to the deleted
  tier — but a missing row also describes a holding that was never this deployment's:
  the repo's own dev loop (a compose stack's executor and `make test`'s Docker contract
  suite share the host daemon) would have had the compose reaper force-removing the
  suite's fixtures within a minute, and two deployments sharing a daemon or K8s
  namespace would destroy each other's live sandboxes. Migration 0018 adds
  `deleted_sessions`; `deleteSession` writes the tombstone in its deleting transaction;
  the reaper skips any owned id with neither row nor tombstone
  (`TestReapPassSkipsForeignSandbox`, red before the fix).
- **Codex's P1 — a session revived between the under-lock re-read and `Reap` — was
  refuted with evidence**: no revival writer exists. Archived sessions are read-only
  (`internal/api/events.go:78` refuses every event batch under the session row lock,
  and no unarchive path exists anywhere); `terminated` is set by nothing today and both
  wake paths exclude it (events.go:204-212 — user.message requires idle, interrupt
  requires idle/running); a deleted row never returns. The terminal tiers are
  absorbing states, so the under-lock answer cannot rot.
- Codex's remaining findings, all acted on: the under-lock re-classification now rides
  the lock's own connection and cmd/executor refuses `pool_max_conns < 2` (nested pool
  acquisition under the pinned lock connection could self-deadlock a deliberately tiny
  pool); a failed `pg_advisory_unlock` now closes the connection instead of releasing
  it healthy — the old log line claimed a pool release frees the lock, which is false
  for pgx v5 (session advisory locks survive `Release`); `Run` now joins the reaper
  goroutine before returning (`TestRunWaitsForTheReaperToStop`, red before the fix);
  the `pg_locks` test barrier filters by database and this session's classid/objid;
  and the two unpinned contracts got tests — `TestReapPassContinuesPastAFailingSession`
  (errors.Join isolation) and `TestDeleteSessionSurvivesCheckpointDeleteFailure`.
- From the Claude review's confirmed list: the post-commit checkpoint-blob delete in
  `deleteSession` detached from the request context (a client hangup must not skip the
  one delete the path exists for); the reap metric pinned through a ManualReader
  (`TestReapMetric` — archived and terminated differ only in the label); plan 24's D8
  metric sketch and reap-criteria table annotated with the as-built names.

**On the PR, the Codex bot added one more confirmed P2**: the tiers never checked the
environment kind, so on a shared daemon the platform reaper would destroy an archived
**self_hosted** session's sandbox — the customer's BYOC worker's asset, which never
takes the advisory lock (the plan wrote the `kind = 'cloud'` exclusion for the TTL
tier only). Fixed by making every tier cloud-only: `classifyForReap` joins
`environments.kind`, and the tombstone records the kind (the row being gone by the
time the reaper asks) — migration 0018 amended in-branch before merge.
`TestReapPassLeavesSelfHostedSessions` covers both arms; the discriminating mutants
(each kind check dropped) were killed post-commit.

Hardening mutants killed post-commit: tombstone check dropped → foreign-skip test red
(its pre-fix red), join dropped → `TestRunWaitsForTheReaperToStop` red (pre-fix red),
plus fresh runs for the never-red tests recorded in the PR. Not constructible: a
discriminating test for the unlock-failure connection-close — every SQL-reachable
unlock failure (aborted transaction, dead backend, cancelled context) already destroys
the connection on release, freeing the lock; the guard covers the residual class
(e.g. a pooling proxy) no real-Postgres test can simulate. The cmd startup floor is
main-glue, outside the tested surface by convention. The detached-context blob delete
likewise has no cancelled-request test — httptest offers no clean way to cancel the
request context between the handler's commit and its post-commit tail; the change is
one `context.WithoutCancel` line, verified by reading and by the failing-blob test
covering the same tail. Refuted-and-unchanged, with the
verify agents' own evidence: Run-return-before-reap-exit harms (the pool blocks on
`Close` until conns release), the provision-side re-read (a stale sandbox adoption is
the pre-slice status quo and slice 4's restore recuts it under the same hold).

## Sandbox teardown (plan 24, #64) — slice 2: `Owned`/`Reap` on both backends

**Review hardening, landed in the same PR.** The verifier PASSed twice — the slice, then
the hardening delta — independently reproducing five targeted mutants red in a scratch
copy (the 409-wait reverted, either backend arm's revoker threading dropped, the k8s
delete error swallowed, the gate's ownership label dropped) and running a live
Provision→Owned→racing-`docker rm -f`→Reap→Owned probe against the real daemon. The two
review passes (Codex `gpt-5.6-sol` plain mode; the Opus 5 review workflow, 4 finders +
per-finding adversarial verification) converged on the same substantive finding from
opposite directions: Docker `Reap` tolerated only the 404 a racing remover almost never
produces, while the 409 "removal already in progress" window — measured live at ~150 ms
per removal — surfaced the racer as an error; `removeWaitingGone` now waits that window
out, and the correction came from the verification itself (blanket-tolerating the 409
would break Reap's gone-when-it-returns contract). The k8s "actually gone" wording was
cut down to what a grace-0 delete can promise — the API object, Destroy's own documented
bound. Confirmed test gaps each gained a mutation-checked test: backend.New's revoker
threading (proven with no daemon in the loop — the sentinel revoke error surfaces before
any endpoint contact), k8s delete-error propagation, the gate create payload's ownership
label (the one thing that makes an orphaned gate reapable — a probe dropping it had left
every suite green), empty-label-value exclusion on both Owned paths, and the docker list
error paths. Refuted with evidence rather than fixed: a claimed reap-vs-provision token
window (slice 3's advisory lock owns it), the k8s UID-successor "false success" (a
successor under the reaped pod's name means the reap's own target is already gone — the
wait ended for the right reason), and the
pool-before-provider reorder (validation ordering shifted, no correctness change). The
K8s label-authorizes-reap consequence Codex raised is placed deliberately in
docs/self-hosted-security.md rather than "fixed": within the executor's namespace,
label-set authority implying delete authority is the single-trust-domain assumption the
platform already makes, now stated where an operator reads.

## Sandbox teardown (plan 24, #64) — slice 1: the running-session archive/delete guard

**Review hardening, landed in the same PR.** The verifier returned PASS, independently
reproducing the guard's red (reverting `requireNotRunning` re-failed the new test) and
adding one docs finding, fixed (ARCHITECTURE's sessions.go row now names the refusal).
The Claude review (Opus 5) confirmed three gaps, each closed by a mutation-checked test
or a registry entry: the registry's "rescheduling does not refuse" claim was untested — a
guard tightened to `!= idle` survived the suite; the rescheduling case now goes red on
that mutant. The `FOR UPDATE` row lock was untested — stripping it left the suite green;
`TestArchiveObservesConcurrentRunningFlip` now fails on that mutant. And the documented
"session update must be idle" requirement is silently unenforced — registered in
DIVERGENCES, tracked as #312. Its fourth claim — a deadlock in this diff — was refuted
for the diff, but both reviewers independently converged on the underlying pre-existing
inversion: `deleteSession` locks session-then-token while `gatetoken.Ensure` locks
token-then-session (its successor insert takes the FK's `KEY SHARE`); now #313. The first
Codex pass died in its security-scan plugin (finalization schema error, no verdict — the
rerun was steered to plain review mode). The rerun confirmed the six attacked invariants
otherwise clean and forced two test rewrites: the concurrent test's 300 ms sleep proved
nothing about *where* the archive was blocked (under CI pressure the lockless mutant
could pass by reading the post-commit row), so the commit now waits on a
`pg_stat_activity` barrier that observes the guard's `SELECT … FOR UPDATE` in a `Lock`
wait — the mutant now fails deterministically by barrier timeout; and the goroutine
called `t.Fatalf` off the test goroutine through `tserver.do` (a hang, not a failure, on
transport error) — it now speaks plain `net/http` and reports errors over the channel.

---

## Classified unwritable writes (plan 23) — archived 2026-08-05, delivered in one PR (#306)

Drafted, verified against the reference checkouts, and delivered the same day. The plan's
research record (the SDK's `tools/agenttoolset` answering every write failure to the model
as an `is_error` tool_result, worded by `fsErrorMessage`'s four-entry table with raw
passthrough, no wire error-code taxonomy) was verified claim-by-claim by the independent
verifier at the pinned SDK version before the plan landed (PR #308), and the
implementation followed as designed: `ErrNotWritable`/exit 20 with the reason taken from
the sandbox's own bash strerror on both backends, `PathNotWritableError` carrying it to a
`fileFault` row that normalizes wording the reference's way. TDD red observed on all new
tests before the code (host-bash exit-20 + reason, docker probe classification,
mkdirAll's strerror tail, the toolset table rows, the tightened read-only-root contract
cells). The one design deviation from the plan's sketch: the sentinel and typed error
landed beside their siblings in `sandbox.go` rather than `filefault.go`, and docker's
`mkdirAll` classifies from its exec's captured stderr rather than a second probe — same
mechanism, one exec fewer. The tee mid-stream failure deliberately stays a raw exit 1: a
failure of the transfer, not of the target. Review extended the scope past the plan's
sketch on one docker route: the plan scoped the post-PUT `mv` failure out as reachable
only from configurations the provisioner refuses, and both reviewers disproved that —
the daemon extracts the archive as root, so a root-owned parent under a non-root uid
takes the PUT and refuses only the move, a provisionable posture. The delivered code
asks the same writability probe after a failed rename (a probe that succeeds keeps the
raw error: the transfer's failure, not the target's), measured red-first against a real
non-root image.

---

## GCS-native blob plan (22) — archived 2026-08-04, both slices delivered (#240)

docs/plan/22_gcs-native-blob.md is archived complete. It set out to remove the GCP
deployment's last Google-issued key material — the GCS HMAC key pair that
`internal/blob/s3` needed to reach Cloud Storage through its S3-interop XML API — and both
halves landed. Slice 1 (PR #276): `internal/blob/gcs` on `cloud.google.com/go/storage`,
selected by `BLOB_BACKEND` through the sibling `internal/blob/backend` package, passing
`blobtest.Run` unchanged against a pinned `fake-gcs-server` and, opted in with
`RUN_LIVE_GCS_TESTS=1`, against real Cloud Storage. Slice 2 (the archiving PR): the chart's
keyless `gcsObjectStorage` mode, `deploy/gcp` dropping the HMAC pair and re-pointing the
bucket IAM to the three workload identities, and `bootstrap.sh` shrinking from 313 lines to
157. The sibling issue #241 — the S3 backend's pre-created-bucket mode — landed first
(PR #274) and narrowed the same deployment's *permissions*; this plan removed the
*credential*.

Two of the plan's decisions changed shape against measurement rather than being carried
out as written. Decision 4's absence proof is a one-object **list** probe, because a probe
run against real Cloud Storage found the read path returns the same bare
`storage.ErrObjectNotExist` for a missing object and for a bucket that does not exist, with
no `googleapi.Error` to inspect — `ErrBucketNotExist` appears only on the listing path.
Decision 6 said "both bucket IAM grants re-point"; reading the three binaries' actual use
turned that into three grants, not two: `roles/storage.objectUser` for the controlplane and
executor (Get/Put/Delete) and `roles/storage.objectViewer` for the brain, which only reads.
`objectAdmin` was rejected on a measured difference: it adds exactly four permissions over
`objectUser` — two object-IAM ones the bucket's uniform bucket-level access turns off, and
two object-retention ones that are live but govern retention the platform never sets.

**What slice 2 was verified against, and what it was not.** Its gate is credential-free:
`make gcp-fmt gcp-validate gcp-split-check gcp-lint`, the three run-rather-than-read
tooling tests (`gcp-bootstrap-test`, `gcp-split-check-test`, `gcp-dbinit-test`), all four
chart object-storage modes rendered with every guard fired, and a new CI helm step
asserting the keyless mode carries no credential key and reaches all three processes. No
`terraform apply` was run and the mode-2 acceptance was not re-run: both cost money and are
interactive on purpose (CLAUDE.md's `gcp-*` note), so they are an operator action. The
migration path for a deployment that predates #240 is written out in docs/deploy-gcp.md
rather than automated: the three bucket bindings added out of band first (no ordering of
the two Terraform states alone is gap-free, since one apply adds the new grants and drops
the old ones), then the deployment moved and reconciled, then the HMAC key deactivated and
deleted, and only then `terraform state rm` and the deletion of the three retired
foundation resources. That order is load-bearing at both ends — the key must go before the
service account, because deleting the account takes its keys with it and strands the
secret.

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

The production-shaped half of plan 20, run end to end against the live staging
project: the platform on GKE with **no bundled services at all**, every
credential in a pre-created Secret, and each backing service reached with this deployment's
own credential rather than the operator's — three different kinds of credential, itemised
below, because flattening them into "its service account" would not be true of Cloud SQL.
Nothing was exposed — the whole battery ran over `kubectl port-forward` (Decision 8).

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
  for it. (#269 templated it afterwards, and gave the brain the identity it lacked — still
  unexercised on a live cluster, so what this record says about the run stands.)

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
   destroy got *past this finding* by removing the resource from state by hand and
   re-running — exactly the manual step the fix removes. It did not finish the teardown:
   the next wall is below, and it is a different one.

1. *The dbinit record omitted `REPLICATION`.* Slice 5a added the assertion
   (`dbinit.sql`) and a row for it in the deploy guide's asserted-properties table, but not
   the corresponding field in the summary `RAISE NOTICE` — so the property was enforced and
   the record that evidences it was silent. Found by reading the live output against the
   table. The NOTICE now carries `replication=%`, and `dbinit_test.py` asserts it. (The
   suite ends this PR at **83** checks rather than 80, because the review that followed
   found the same omission one role along — see the hardening note below.)
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

**Teardown needed several runs, and only the first failure was a defect.** Run 1 removed 21
resources — the cluster and both node pools, the NAT and router, the subnetwork, both
Artifact Registry repositories, the GCS bucket, the Cloud SQL administrator and every IAM
binding — and then stopped on finding 0 above, leaving the database, the instance the
database blocks, and the eleven resources downstream of that instance. So all but one
billable resource was already gone when the defect fired; what it stranded was the running
Cloud SQL instance.

Run 2, after the database was taken out of state by hand, removed that instance — one
tracked resource, and the last billable one — and then failed on the VPC peering with
`Producer services (e.g. CloudSQL, Cloud Memstore, etc.) are still using this connection`,
because Cloud SQL releases the peering asynchronously and the delete landed while the
release was still in flight. Subsequent runs kept hitting the same wall, and within this
run it never came down: **25 further destroy attempts over about four hours** — ten at
five-minute intervals, then fifteen at ten — all failed the same way, with the instance
long gone from `gcloud sql instances list` and the peering still `ACTIVE / Connected`.
The manual escape does not open it either. `gcloud services vpc-peerings delete` fails with
`FLOW_SN_DC_RESOURCE_PREVENTING_DELETE_CONNECTION`: a producer-side resource inside
Google's own project still holds the connection, and nothing on the consumer side can
reach it.

**Four hours was never going to be enough, and the review pass is what established that.**
Google documents the interval — *"if you delete a Cloud SQL instance, you receive a success
response, but the service waits for four days before deleting the service producer
resources"*
([private services access](https://cloud.google.com/vpc/docs/configure-private-services-access#delete-private-connection)).
So this is not GCP being slow, it is GCP being documented: no number of retries inside one
working session can succeed, and the run's four hours of them were spent against a wall
with a four-day clock on it. The first draft of this record called the timing mysterious
and told operators a next-day re-run would "usually" finish it — a frequency claim built on
zero observed successes, caught by both reviewers, and replaced with the figure above.

One thing was deliberately *not* tried, and the same document explains why it should not
be. Force-deleting the consumer-side peering (`gcloud compute networks peerings delete`)
looks like the way out, but Google's guidance is *"Don't attempt to delete a private
connection by deleting its associated VPC Network Peering connection directly"* — creating
a private connection afterwards can then fail, recoverable only by re-creating it with the
same allocated range names. For a residue that costs nothing, waiting beat inventing state.
Filed as [#270](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/270), whose
remaining half was that `main.tf` still framed that manual command as the escape "if a
retry is not enough" without saying it is subject to the same four days — closed by the
follow-up PR that reworded the comment to match the guide.

What that leaves behind is worth stating precisely rather than rounding to "clean": eleven
resources — the VPC, the reserved peering range, the service-networking connection and
eight API enablements — **none of which is billable**. Project-wide, after run 2 there was
no cluster, no Cloud SQL instance, no Artifact Registry repository, no bucket and **zero
Compute Engine disks**. So the cost was settled by run 2 and everything after it is
bookkeeping. That failure is GCP's timing rather than a fault in the configuration, but it
is reliable enough on a private-IP instance that the guide now tells operators to expect
more than one run — and to accept the non-billable residue as an end state rather than
chase it. `foundation/` is untouched by design.

**A hazard that mode 2 structurally does not have.** The mode-1 record's
orphaned-PersistentVolume trap **cannot fire here**: mode 2 runs all three bundled services
off — `existingSecret` does not disable them, it makes leaving them on a render error — so
the release renders no StatefulSet and claims no volume; the run
finished with zero PVCs and zero PVs, and the only disks in the project were the four node
boot disks the destroy reclaims. The OTel collector's Google service account and its two
project role bindings were created by hand for this run and removed after it.

**Review hardening, landed in the same PR — and it found a fifth defect the run did not.**
The verifier's first pass returned **FAIL**, correctly: the commit it reviewed shipped an
acceptance record claiming "`terraform destroy` removed all 34 resources" while the run it
recorded had stopped at 21 on finding 0. That is the failure mode this whole file exists to
prevent, caught in the artifact the slice delivers; the record above is the corrected
account, and a second pass on the fixed tip returned PASS. It then caught two more accuracy
defects the corrected text had introduced — a teardown paragraph whose run-2 attribution
could not be squared with its own resource arithmetic (the cluster, bucket and registry went
in run 1, not run 2), and this list's own stale "80 checks".

The Codex pass (`gpt-5.6-sol`, `ultra`) returned six findings, and the one with teeth was
**structural, not editorial**: `dbinit.sql`'s **group-role** assertion listed
`SUPERUSER`/`CREATEDB`/`CREATEROLE`/`BYPASSRLS`/`LOGIN` but not `REPLICATION`. The platform
role can `SET ROLE` to that group, and the file's own comment says anything the group holds
is one `SET ROLE` away — so a group role granted `REPLICATION` passed the check while the
summary still printed the platform row's `replication=f`, and the record read clean. The
asymmetry was made *visible* by finding 1 above: adding `replication=` to the NOTICE put the
two assertions side by side for the first time. `LOGIN` was already in that list and is not
`SET ROLE`-reachable either, so the criterion was always "the group holds no privilege
attribute", not the narrower reachability argument — `REPLICATION` was simply missing.
Fixed, with a test case proven red against the pre-fix SQL by both the implementer and the
verifier independently; the suite is 83 checks.

Codex's other five were accuracy defects in this record, and the sharpest was real:
*"every backing service was reached as the deploy guide's own service account"* is **false
of Cloud SQL**, which this run reached over the direct private-IP path with no Google
identity at all. The record now names three distinct credential kinds instead. Also fixed:
`existingSecret` makes leaving the bundled services enabled a render error rather than
disabling them; slice 1 landed in two PRs, not one; there is no `blob.Open`; and two forward
references to "slice 5" outlived its delivery.

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
live project, the platform deployed on GKE in mode 1, the acceptance battery driven by the
**real `ant` CLI 1.21.0** built from the read-only checkout, and the teardown proven by
destroy → apply. This also
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

## Cloud SQL Auth Proxy sidecar (#269) — review-hardening record (2026-08-04)

The change itself is in [CHANGELOG.md](../CHANGELOG.md); this is what the reviews changed
about it, the mutation battery that settled it, and the one thing decided against.

**The setup.** Codex on `gpt-5.6-sol` at the config's `ultra` effort, read-only, twice — the
branch, then the fix commit. The verifier ran twice on its pinned `claude-fable-5`, the
second dispatch after the fixes changed behaviour. The Claude-side `/code-review` is
`disable-model-invocation`; an Opus 5 adversarial pass stood in for it, twice, labelled as a
stand-in rather than as `/code-review`. GitHub's own Codex reviewer and CodeRabbit added two
findings on the PR.

**What the reviews were for, in one line: the guards were fine and the tests of the guards
were not.** Nothing shipped a broken sidecar. What five of the seven findings had in common
is that a CI assertion would have stayed green while the behaviour it named regressed —
which is the failure mode a chart's CI is the only thing standing in front of, since nothing
else executes a Helm template.

- **A refusal asserted by exit code cannot say which guard fired.** Several inputs are
  refused by more than one guard, so deleting the required-`instanceConnectionName` guard
  left the step green: the shape guard refuses an empty name too. Each refusal is now
  asserted by its **message**.
- **The startup probe was asserted, its port was not.** Moving the probe from 9801 to 9800
  kept CI green while shipping a pod whose probe polls a port the proxy never binds — the
  application container then never starts, which is the exact failure the step exists to
  prevent. The step now reads `--http-port` out of the rendered args and requires every
  probe port to equal it. The liveness probe was unasserted altogether.
- **The three health flags were unasserted**, so a sidecar whose probe could never succeed
  passed.
- **`helm template … | grep -q X && fail` passes when the render fails**, because a failed
  render matches nothing and `errexit` is exempt on the left of `&&`. It had already been
  fixed once in this step and reintroduced three lines later.
- **The assertions were scoped to the `initContainers` block, not to the proxy's entry**, so
  a second init container could supply whatever the proxy's entry had lost.

**Twenty-two mutations** were run against the two steps and every one goes red, each for its
own reason: the four guards deleted in turn; the shape guard's empty-segment test and upper
bound dropped separately; each of the five sidecar flags dropped; either probe's port moved
off the health server's; either probe removed; `--address` added; `restartPolicy` removed;
the `privateIP` switch ignored; the proxy defaulted on; a **decoy init container** carrying
what the proxy's own entry had lost; the brain's Deployment stripped of its ServiceAccount;
and the brain pointed at the control plane's. The last two of those are why the assertions
are anchored to the proxy's own entry rather than to the `initContainers` block, and why
the brain's ServiceAccount is asserted by name rather than by existence.

One caution the battery itself produced: a mutation that silently fails to apply reports as
a passing assertion. Two rounds here reported green from an edit that never landed — a
`perl` line-number substitution off by two lines, and a `zsh` loop that did not word-split
its arguments. Both times the assertion was fine. Check the mutated file, not just the exit
code.

**Decided against: a fifth guard refusing a DSN that bypasses the proxy.** The reviewer's
case was that this is the likeliest mistake and the chart holds the DSN in the
`externalDatabase.url` path. The first reason given for declining — that a guard firing in
one of the two DSN paths and blind in the other reads as a check performed — was itself
refuted in the second review, and correctly: `secret.yaml`'s `gcpKMS.keyName` regex is
exactly that shape and ships unchanged. The reason that holds is grammar, not reach. A DSN
through the sidecar can legitimately say `127.0.0.1`, `localhost`, `[::1]`, or a unix socket
path, so a host check would refuse working deployments — the same false-rejection risk that
keeps the `instanceConnectionName` check to a shape rather than to Google's naming rules.
That is the reasoning now in the three documents.

**Not established, and said so in the change itself:** the proxy path has never carried a
connection on a live GKE cluster. What is established is that it renders, that a real API
server accepts the manifests, that the pinned `cloud-sql-proxy:2.24.1` accepts the exact
argument vector the chart passes (it proceeds past flag parsing and fails only on absent
credentials), and that the Terraform passes the credential-free checks.

## e-wire-cli acceptance + mutation duty — plan 25 slice 1, `github_repository` resources (run 2026-08-07) — ✅ passed

The real `ant` CLI (v1.21.0, built from the read-only reference checkout) against the
compose stack built from the branch — controlplane + Postgres + MinIO + OpenBao only,
wire-only by design: no brain, no executor, no clone, no network to GitHub. The
`authorization_token` in every request was a deliberately fake sweep value
(`ACCEPT-TOKEN-e2e-*`), so nothing in this record is a credential.

The sequence: `beta:environments create` (cloud) → `beta:agents create` →
`beta:sessions create` with two `--resource` `github_repository` objects (one with a
`branch` checkout and a defaulted mount, one `.git`-suffixed with an explicit
`mount_path`) → `beta:sessions:resources list` / `retrieve` → `update` with a new token →
`delete` → `beta:sessions retrieve`. Every read shape came back with exactly the seven
public fields; the defaulted mount rendered `/workspace/example-repo`, the `.git` suffix
stripped for the name only (the stored `url` keeps it); the omitted checkout rendered
`checkout: null`. Rotation returned 200 with the full resource and bumped only
`updated_at` (`…:48.895882637Z` → `…:48.937895512Z`), and the later session GET proved the
bump persisted. The delete returned the designed 400: "github_repository resources cannot
be removed; repositories are attached for the lifetime of the session".

Three sweeps, all zero: the transcript's **response** lines for both token values and the
`"authorization_token"` key (command lines, which legitimately carry the fake token, were
excluded); the controlplane's logs for both token values; and the database, where
`session_resource_credentials` held ciphertext only — both rows sealed under the OpenBao
transit key (`token_key_id=map-secrets`), the rotated row showing `updated_at <>
created_at` and a different ciphertext length. The two designed `slog` lines fired once
each: `session repository credentials sealed … repos=2` and `session resource token
rotated`.

**Mutation duty: nine probes, nine red.** Each guard was deleted in a git-archive scratch
copy of the branch HEAD and its sentinel test rerun (a first workflow round had cut its
copies from `main`, where the feature does not exist — nine "unverifiable" rows, discarded
and rerun from the branch archive). All nine went red for their own reason: the token
sweep caught the echo on all five response surfaces; the URL grammar's
userinfo/query/fragment/port refusals each flipped to 200; the repo delete returned
`session_resource_deleted`; the add path ran on to a file-shaped 404 instead of the
file-only 400; rotation on an archived session returned 200; unclean mounts
(`//`, `.` segment, trailing slash) passed; nested repos and a file above a repo mount
passed; nine repos sealed (`repos=9` in the log); and the cipher-less refusal's removal
turned the clean 500 into a nil-pointer panic surfacing as EOF. The tenth guard-shaped
behavior — `checkout` union strictness — is covered by the same validation test's
table rows and was not separately mutated.

**Verifier round (same day): two live bypasses through the default-mount derivation.**
The pinned verifier's behavior rung, probing a branch-built controlplane with curl, found
what every review of the *supplied*-path rules had missed: the **derived** default mount
`/workspace/<repo-name>` skipped validation entirely, and `parseGitHubRepoURL` accepted
`.`/`..`/NUL/space as the repo segment. `url: …/acme/..` (no `mount_path`) stored
`mount_path: "/workspace/.."` — the reserved `/` in disguise, on an unremovable resource —
and `…/acme/%00repo` reached the jsonb bind as a 500 (the #135 failure class). Fixed
TDD-red-first: five new validation cases (`..`, `.`, `%00`, `%20`, a 1200-char name) were
run against the pre-fix code and failed exactly as the verifier observed (200/200/500/200/
200), then the grammar was bounded (segment charset `[A-Za-z0-9._-]`, a repo name
`path.Clean` would rewrite refused) and the derived default routed through
`validateRepoMountPath` like a supplied path; all five went green. The same round added
the captured-`slog` sweep to w-token-sweep (a mutex-buffered `slog.SetDefault` capture —
zero token hits) and the archived-session **delete** assertion beside the rotation gate,
and corrected two plan-matrix facts (create returns 200 on this server, not 201; the SSE
stream is a live tail with no history replay, so the events list endpoint is the
stored-event sweep surface).

**Claude-side review stand-in round (Opus 5 workflow, /code-review being
model-uninvocable; 4 dimensions, every finding adversarially re-verified against the
source): 17 confirmed rows, three genuinely new after dedup.** Ten were the
derived-default-mount family the verifier had already caught (the stand-in's verify pass
independently reproduced the `%00` 500 and the `/workspace/..` store against the pre-fix
commit, and noted the fix already in the working tree). The new three, each fixed
red-first: (1) **the add endpoint skipped the ancestor rule** — a file added at the
resolved `/mnt/session/uploads/repo` mounted above a repo at `…/repo/src` (observed 200,
now the same 400 as create; the in-tree overlay add stays legal and is asserted); (2)
**a bare trailing `?` or `#` passed the URL grammar** (`ForceQuery` is the only trace of
a bare `?`, the raw string the only trace of a bare `#` — both observed stored, both now
400); (3) **the cipher round trip ran under the session row lock** — create sealed inside
the create transaction and rotation encrypted between `FOR UPDATE` and commit, so a slow
OpenBao would have stalled every concurrent event append for the session; both now seal
before their transaction opens, the vault-credential precedent
(`vaultcredentials.go`'s "Seal before opening the transaction"), with the cipher-less
refusal deliberately left inside the rotation transaction so a file resource still gets
its type rejection first. Four smaller coverage gaps from the test-quality dimension
were pinned the same day: the `.git`-strip default assertion, a relative repo
`mount_path`, both caps accepted exactly at their limits, and the archived-session
delete beside the rotation gate.

**Codex round (gpt-5.6-sol, `task` subcommand, read-only) — report lost, findings
recovered, one fix.** The run completed its analysis but its internal security-scan
workflow failed at the completion step (`scan.target.snapshotDigest: expected a non-empty
string`) and, per that workflow's rules, never issued the formal report — reported here
rather than silently dropped, with the findings recovered from the session rollout's
narration. Four items: the percent-decoded URL segment family and the add-endpoint
ancestor gap (both independently found by the other two reviewers, both already fixed);
test-assurance notes matching the stand-in's test-quality dimension; and one new finding,
rated Low/P3 — **a hostile or misconfigured OpenBao endpoint could reflect the request's
plaintext *decoded* into its error body**, and the client's scrub, which replaced only the
request's own strings (the base64 form) and the token, would pass the raw secret through
to the process error path. Fixed red-first in `internal/secrets/openbao`
(`TestServerErrorTextNeverEchoesDecodedPlaintext` observed
`status 400: rejected value ghp_decoded-secret-token` pre-fix): the scrub now redacts
every request-borne value in both its verbatim and base64-decoded forms — a hardening
shared by every sealed secret, vault credentials included.

## Mutation duty + unit M — plan 25 slice 2, the `github_repository` clone (2026-08-07) — ✅ passed

The clone half's verification record. Unit M (docs/plan/25_git-repo-mounting.md,
"Verification") runs against a **real git repository served over real smart-HTTP**: an
in-package fixture (`internal/executor/repofixture_test.go`) wraps go-git's own
server-side transport in a pkt-line HTTP shim, because the library ships the server as a
`transport.Transport` rather than an `http.Handler` and its `ServeUploadPack` lives in an
internal package. Failure modes are handler-level — an answered status drives the auth
and not-found reasons, a sleep drives the deadline — so one fixture drives every
adversarial row. Ten rows run against **real Docker sandboxes**, because the claim
"the agent's tools see a checkout" is only observable where tools really run: a fake
sandbox never runs `tar`, so a clone could never make `<mount>/.git` appear there.

**The nine guards, each shown red against code without it.** The probes ran against a
`git archive` tarball of the branch tip in a scratch directory, never the checkout — a
lesson from PR #321, where shared-scratch probes left a live mutant in the tree.

| Guard | Mutation | Observed failure |
|---|---|---|
| probe-only idempotence | always clone, never probe | `clones = 2, want 1` |
| clone-error dedupe | append without the EXISTS check | `recorded 2 clone errors … want 1` |
| byte budget | drop the meter | `clone errors = [], want exactly one too_large` |
| clone timeout | drop the deadline | `recorded 0 clone errors, want exactly 1` |
| shell quoting | interpolate the mount path raw | `unexpected EOF while looking for matching '` |
| brain cloud gate | inject the block unconditionally | `a self_hosted session was told repositories are mounted` |
| repos before files | materialize files first | `/workspace/fixture/overlay.txt: sandbox: no such file` |
| token in header, not URL | put the token in the remote URL | the sandbox sweep found `url = http://x-access-token:ghp_SWEEP-…@127.0.0.1:57178/o/r.git` inside `/workspace/fixture/.git/config` |
| stage-and-rename | extract straight into the mount | see below |

**The ninth guard failed its own mutation test first, and the test was the defect.**
`TestRepoInterruptedExtractLeavesNoPartialTree` forced its failure by planting a regular
file at the mount's *parent*, which aborts the very first `mkdir` — before `tar` runs at
all. Both versions therefore passed identically (`--- PASS` on each), and the probe said
so plainly: "both versions die at their FIRST mkdir with the identical message before any
tar runs, so neither can leave a partial tree." It proved the mutation had really been
applied by forcing `exit 77` and observing it surface. A guard whose test never saw the
broken code proves nothing, so the row was rebuilt rather than accepted.

The rebuild's problem is that staging and the mount are *different paths*, so no planted
obstruction can trip both candidates — an obstruction at the mount only trips the mutant,
one at staging only trips the shipped code. What they share is the `tar` binary. The row
now shadows it (`/usr/local/bin/tar`, installed over Docker's archive API so installing
the fake does not depend on the binary being replaced) with a script that extracts a
`.git` into whatever `-C` names and then exits 2 — the shape a truncated archive or a
killed sandbox produces. The mutant then failed **twice over**, which is the harm the
guard exists to prevent, in sequence: `a partial tree carrying .git survived a mid-tar
failure`, and then, on the retry, `repository already present, skipping clone` →
`README.md: sandbox: no such file` — the idempotence probe trusting the half-tree, so the
repository never re-clones and stays broken for the life of the session. The shipped
staging-and-rename leaves the mount untouched and the retry materializes.

**The `repo-answer` eval (E2E-2) is wired but unrun here.** It needs a real GitHub
fixture repository and a fine-grained token (`GITHUB_EVAL_REPO_URL` /
`GITHUB_EVAL_REPO_TOKEN`), which only the operator can create; the trial asks for a
passphrase without naming the mount or the file's path, so the brain's injected
"Mounted repositories" block is the only way to find it — the discovery mechanism is the
thing under test, exactly as in its `skill-answer` and `file-answer` twins. Its transcript
joins this record when the tier is first run.

**First green run of `repo-answer` — 2026-08-12 (#358).** It stayed unrun long enough to be
parked out of `tasks()` on 2026-08-10 (#359), after three weeks of reddening the nightly for
want of the fixture. The fixture now exists: a **private** repository holding one root-level
`PASSPHRASE.txt` of sixteen hex characters from `/dev/urandom` (17 bytes with its newline),
and a fine-grained token scoped to that one repository at Contents: Read-only, expiring
**2027-08-13** — the day the nightly will start failing on a 401 unless it is rotated first.
Both, with the fixture's URL, live as secrets on the `evals` deployment environment. The two
`.env` names became `EVAL_GITHUB_REPO_URL` / `EVAL_GITHUB_REPO_TOKEN`, because GitHub refuses
to store a secret or variable whose name begins with `GITHUB_`.

The privacy the trial rests on was measured rather than assumed: anonymous `git-upload-pack`
against the fixture answers 401 and anonymous `raw.githubusercontent.com` answers 404, so the
eval sandbox's unrestricted egress cannot reach the passphrase unless the platform really
materialized the checkout — which is the silent success this trial exists to rule out.

The transcript below is abridged: `resource_id=` is dropped for width and `url=` deliberately,
since this document is public and the fixture is not. Both elisions are marked `[…]`.

```
=== RUN   TestEvals/repo-answer
INFO session repository credentials sealed session_id=sesn_5g9j8kh6vy52dpn0myf820rr repos=1
INFO session created with resources session_id=sesn_5g9j8kh6vy52dpn0myf820rr resource_count=1
INFO repository materialized session_id=sesn_5g9j8kh6vy52dpn0myf820rr resource_id=[…] url=[…] mount_path=/workspace/fixture bytes=14848
INFO repository already present, skipping clone session_id=sesn_5g9j8kh6vy52dpn0myf820rr resource_id=[…] url=[…] mount_path=/workspace/fixture
--- PASS: TestEvals/repo-answer (7.94s)
```

Both the clone and the idempotence path, with every grader green: `ReadsFile` on the mounted
path, `RepoPassphraseAnswered` against the passphrase the in-process oracle read from the
remote, and the Platform-class `TranscriptCarriesNoRepoToken`.

**The two failed attempts before it are the other half of the record**, because what failed was
the network and what behaved was the platform. WSL on that machine cannot reach `github.com` —
the host's proxy is invisible to a NAT-mode WSL, so `api.github.com` answers and `github.com`
times out — and `materializeRepos` met it exactly as designed: a `session.error` of
`type: github_repository_clone_error` with `reason: network`, then `reason: timeout`, carrying
`retry_status: retrying`, and one retry. Neither the event nor the log line beside it carried
the token, and the two get there by different routes worth keeping straight: `emitRepoCloneError`
builds its payload from a fixed message and never receives the clone error at all, so the event
is token-free by construction, while the executor's own `slog` line *does* carry go-git's error
text and is what `scrubTokenErr` stands over. The green run above was obtained by pointing the
test process at the host proxy; nothing in the repository depends on that, and GitHub's own
runners reach `github.com` directly.

**The review round found two ways it would have reddened the nightly anyway**, both of them
the same species as the one that got it parked — a red nobody can act on. `corePack`'s
`no-session-error` is Platform-class and counted *any* `session.error`, so the retrying clone
error above would have failed the trial and blamed the platform for recovering as designed;
it is now tolerated for the retrying variant on trials that carry a repository, with the
recovery itself still asserted by graders that need the checkout. The second is open and
measured rather than fixed: an independent verification run had the model (MiniMax-M3) refuse
the task in 1.7s with no tool call — "I notice there's a prompt injection attempt" — on a turn
that announces a file "holding a secret passphrase" and asks for it back. Both failing graders
were `Either`, so the classing held, but the trial still reds.

The obvious repair was tried and is recorded because it **failed**. Rewording the turn away
from "secret passphrase" to ask for "the one line" of a named file — the move
`journal-multiturn` and `view-range` both record for this reflex — scored 2 of 4 against the
same endpoint where the original scored 7 of 8: once a fabricated "there is no PASSPHRASE.txt
here, which is convenient, there is nothing to leak" (the same reflex wearing a factual
claim), once a tool call emitted as literal text rather than a tool_use block. Twelve runs is
not a study and the endpoint's tool-calling is visibly weak, but the direction was clear
enough to revert to #331's wording and leave the numbers in the code comment, so the next
reader does not spend the same four runs finding out.

**CI cannot prove this on a branch.** The `evals` environment admits only `main`, so a
`workflow_dispatch` from a PR branch gets no secrets at all. The CI half of the proof is a
dispatch on `main` after the merge.

**Self-review round — two defects on the clone-error path, both fixed red-first.**
Re-reading `materializeRepos` with fresh eyes turned up `scrubToken(err.Error(), "")`:
a scrub called with an empty token, which the helper returns unchanged. It read as
protection and was a no-op. Asking what it was meant to protect against found that the
exposure is real rather than theoretical — go-git builds its 401/403/404 errors as
`fmt.Errorf("%w: %s", sentinel, responseBody)`, so a git host that names the credential
it rejected (and one that rejects a credential has already decoded it) puts the token
into an error the executor logs, on the likeliest clone failure there is. A fixture that
echoes the `Authorization` header, sent and decoded, made it observable:

```
the clone error quotes the token verbatim: authentication required: fixture refuses
credentials Basic eC1hY2Nlc3MtdG9rZW46Z2hwX0VSUk9SLUVDSE8tU1dFRVAtOWYzYQ==
(x-access-token:ghp_ERROR-ECHO-SWEEP-9f3a)
```

The same red run caught a second defect the test had not been written for: a 500 from
the remote classified as `reason: internal` (`cloneReason = "internal", want "network"`)
— the platform blaming itself for a git-host outage and sending the operator to read the
wrong logs. Both fixed: `scrubTokenErr` removes the credential in both forms through a
wrapper that keeps `Unwrap` intact — a fresh error would have turned every redacted auth
failure into an `internal` one, which the row asserts against directly — and
`isRemoteStatusError` reads the status off the transport error (unwrapping
`plumbing.UnexpectedError` by hand, since it implements no `Unwrap`) and reports
`network`. The row passes on both arms and is the eleventh guard with red-run evidence.



**A second self-review pass — the byte budget's load-bearing half was untested.**
`TestRepoOversizeAbortsMidClone` asserts an oversized repository is refused with
`too_large` and ships no tar. It proves both, and neither is the property the design
claims: the budget is metered *as the bytes land*, so an unbounded repository costs the
cap rather than its own size, and `meteredFS.Chroot` propagates the *same* counter
because go-git chroots `.git` out of the worktree and the objects are the bulk of a
clone. Neither is observable through that row — its fixture's oversize file lands in the
worktree, which is metered under either arrangement.

Both mutations confirm the gap. With `Chroot` given a counter of its own the integration
row still passed and the new direct row went red (`spent = 40, want 120 (both writes
counted against one budget)`). With the meter moved to *after* the write — the
measured-after shape — the integration row **passed unchanged** while logging
`clone exceeded the byte budget: 1604098 bytes`, which is the whole 1.6 MB fixture
landing on executor-local disk before anything complained; the new row went red with
`the refused write left 500 bytes on disk`. Two direct rows now cover the chroot's shared
budget and the refused write, bringing the slice to thirteen guards with red-run evidence.



**Verifier round (pinned `claude-fable-5`) — PASS WITH FINDINGS, no blockers, coverage
90.31%.** It reran the whole gate from scratch, spot-checked two guards by mutation in its
own scratch copy, exercised the criterion rows against real Docker, and diffed the wire
registrations field-by-field against the pinned SDK. Two of its five findings — the
untested `Chroot` propagation and the `betaworker (tool_exec only)` citation naming a file
that does not exist — had already been found and fixed by the self-review passes above
while it was running, which is the useful kind of agreement: two independent readings, the
same two defects. Three were new and are fixed here.

The substantive one: a cipher-less executor emitted its `internal` clone error *before*
the presence probe, so a session whose checkout was already on disk — restored from a
checkpoint, or landed by a correctly configured executor before the drift — was told a
repository had failed while the agent was reading it. Red first
(`clone errors = [...], want none — the repository is already materialized`), then the
cipher check moved into the loop behind the probe; the one warning line stays per-session
rather than per-repository.

The other two were docs, and one of them turned out to be code. ARCHITECTURE's `replay.go`
row still enumerated the system prompt as agent → skills → files → `system.message`,
missing the repositories block that now sits third. And the DIVERGENCES entry enumerated
the clone-error payload without its `message` field — checking that also surfaced that
`retry_status` is required on every variant of the reference's error union *and* carried by
every other `session.error` this platform writes, while ours omitted it: not a divergence to
register but an inconsistency to fix. Red first across all four failure arms
(`retry_status = <nil>, want {type: retrying}`), then emitted as `retrying` for every
reason — including `auth`, since the next work item re-probes and clones again, so no clone
failure is ever the last attempt and `exhausted` would tell a client this repository is
finished when it is not.



**Codex round (`gpt-5.6-sol`, config `ultra` effort, read-only) — ten findings, two already
fixed, six fixed here, two refuted.** It reviewed `e1cc0dd..366abdb` while the branch moved
under it and said so, and it independently re-derived two defects the verifier round had
already closed (the payload's missing `retry_status`; the cipher check running ahead of the
presence probe). The six new ones were all real:

- **The deadline bounded the fetch and nothing else.** `git.CloneContext` applies its
  context to transport only, so go-git's post-fetch worktree reset, our detached commit
  checkout, and `packTree`'s walk all ran to completion however long they took. A
  repository that downloaded inside five minutes and then checked out for hours held the
  executor indefinitely. The phases we control now check the context between them, and
  `packTree` checks it per entry.
- **`Symlink` bypassed the byte meter entirely.** `meteredFS` wrapped `Create`, `OpenFile`
  and `TempFile`, but go-git checks a mode-120000 entry out by calling `Filesystem.Symlink`
  directly. Hundreds of thousands of entries naming one stored blob cost almost nothing in
  objects and land gigabytes of link data in a spool whose cap never counted it. `Symlink`
  now charges its target.
- **Absolute symlinks reached the sandbox rewritten.** `osfs.New` returns go-billy's
  deprecated ChrootOS, which prepends its own root to an absolute link target, and
  `packTree` reads the link back with raw `os.Readlink` rather than billy's un-rewriting
  one — so `hostfile -> /etc/hosts` arrived pointing at the executor's temporary spool
  path, dangling, inside a clone that reported success and that `.git` then made
  permanently trusted. The spool is built with `osfs.WithBoundOS()` now, which keeps the
  target verbatim and still refuses to escape the root. The Claude-side round reproduced
  this end to end with the real `git` CLI (`git status --porcelain` printing `" M abs-link"`).
- **A cancellation could strand the shipped tar in the sandbox.** Cleanup lived only in
  the extraction command's own tail, so a context cancelled between `WriteFileStream` and
  `Exec` left up to the whole byte budget sitting in the session's `/tmp` for its life.
  Swept now on a context the cancellation cannot reach.
- **Two reachable failures blamed the wrong side.** A branch name go-git will not build a
  refspec from (`foo:bar`) and a remote answering 200 with an outage page both landed on
  `internal`, which tells an operator to go read our logs over someone else's fault. They
  classify as `checkout` and `network`.
- **The executor never re-judged the mount path it builds an `rm -rf` from.** A row that
  reached the database by any route but the create endpoint had that command built for it;
  `/workspace/repo/..` cleans to `/workspace`. `safeRepoMount` now judges it independently.

Refuted, with evidence rather than argument: Codex reported that `packTree` holds every
source descriptor until it returns and dies on `EMFILE` for a repository of thousands of
files. The `defer src.Close()` is inside the closure passed to `WalkDir`, so it runs per
entry. A probe under a deliberately tiny limit settled it — 600 files packed with
`RLIMIT_NOFILE` at 96 — and the same probe, run against a mutant that really did hold the
descriptors, failed with `too many open files`, so the probe could see the bug it was
looking for. Its dedupe-across-lease-handoff finding is accepted as a known low: the
consequence is a duplicate `session.error`, and closing it properly means an append that
asserts the lease, which is a queue-layer change this slice does not justify.

**Claude-side round (six-dimension adversarial workflow, every agent pinned to Opus 5;
`/code-review` itself is not model-invocable, so this is a disclosed stand-in) — 22
findings raised, 8 survived refutation.** Two were the symlink rewrite and the malformed
branch name, already covered above. Four more were real and are fixed:

- **A zero commit sha silently checked out `master`.** The null object id is forty hex
  characters, so it passes the create endpoint's validation, and go-git's
  `CheckoutOptions.Validate` substitutes `master` for a zero hash rather than refusing it —
  so the session was told its clone succeeded while the mount held a branch nobody named,
  and the resource went on advertising the sha. GitHub's own webhooks put forty zeros in
  `before`/`after` on branch create and delete, so a forwarding app reaches this without
  inventing anything. Refused now, on the `checkout` reason.
- **A repository named `skills` deleted the skills tree written four lines earlier.** Its
  derived default mount is `<workdir>/skills`, and extraction begins by removing the mount
  — while the system prompt goes on naming `skills/<dir>/SKILL.md`. The workdir and its
  skills directory are refused executor-side, where the workdir is actually known.
- **A repository could be mounted exactly at another's staging path.**
  `/workspace/.x.map-repo-staging` is a mount path the API accepts, and cloning
  `/workspace/x` removes it. Refused by the same guard.
- **A lost lease was reported as the git host's failure.** `cloneReason` had an arm for
  `DeadlineExceeded` and none for `Canceled`, and go-git surfaces a cancelled fetch as a
  `*url.Error`, which the network fallback matches. Nothing but this platform cancels a
  clone without a deadline, so it classifies `internal` now.

The last two were paperwork the round was right to catch: this record and CHANGELOG both
said "seven rows against real Docker" when `dockerRepoHarness` appears ten times — a count
that never described any state of the branch — and the comment in
`TestRepoCloneErrorNeverQuotesTheToken` had its two rows' rationales inverted. The round
proved the inversion by mutation: with `scrubToken` gutted, the 500 row still passed and
only the 401 row failed, because go-git splices a response body into the error it builds
for 401/403/404 and for nothing else. The comment now says which row is the redaction's
only coverage, so nobody deletes it as redundant.



**The cancellation sweep's own row lied first.** The fix for a stranded tar landed without
a test, so it was written afterwards — and its first draft proved nothing. The recording
stub logged every command it was handed and checked the context only after, so the
mutation that hands the sweep the *cancelled* context — the obvious implementation, which
cleans up nothing — passed the row unchanged. A real sandbox refuses a command whose
context is already done; the stub now records only what it actually runs, and both
mutations go red on `commands issued: []`, the no-sweep one and the cancelled-context one
alike. Third instance in this slice of a test that would have certified the bug it existed
to catch, after the stage-and-rename row and the byte budget's two.

**The verifier's second round found two guards with no test that could fail.** It reran
the whole gate from scratch on the branch tip (`total statement coverage: 90.28%`,
integration suites really running: executor 72.4s, brain 46.7s, docker 77.9s, k8s 70.0s)
and returned PASS with no blockers — and then caught by its own mutation spot-checks what
this branch's mutation duty had missed twice. One of them made a CHANGELOG claim untrue,
which is the more serious half: an unsupported claim of evidence is worse than a
gap nobody asserted was closed.

- **The post-fetch context bound.** With all three `ctx.Err()` re-checks in `cloneToTar`
  and `packTree` neutralized, every one of the 26 repository rows stayed green.
  `TestRepoCloneStopsOnASpentBudgetAfterTheFetch` pins it now, with a context that is live
  until the fixture has been asked for a pack and spent from then on — the deadline that
  survives the fetch and expires in the phases go-git leaves unbounded. Its Done channel
  never fires, so the transport completes normally and nothing but an explicit check can
  notice. Red under the mutation on `a clone whose budget expired after the fetch ran to
  completion, packing …/repo.tar (14848 bytes)`. The three checks are deliberately
  redundant — each downstream one catches what the one before it would have — so no single
  check's removal is observable, and the verifier measured all three rather than accepting
  the claim from the first: alone, each is invisible (`packTree`'s per-entry check catches
  the expiry the first one would have); the first and the last removed *together* are
  caught; and the middle one, which guards the commit checkout the row does not ask for, is
  covered only by the all-three run. The first draft of this record said "only their
  removal together is observable", which understated the pin in one direction and
  overstated the middle check's share in the other. What the row pins is the bound itself,
  the property `EXECUTOR_REPO_CLONE_TIMEOUT` advertises; the CHANGELOG says that rather
  than claiming each check is separately pinned. One nuance is in the fixture, not the
  guard: the context flips when the upload-pack POST *arrives* rather than after the pack
  is written, so the budget expires around the transport's end and not strictly after it.
  Immaterial, and the mutation is the proof — the mutant still completes its transport and
  packs a tar, so nothing on that path consults `ctx.Err()` by itself.
- **The brain's latest-reason ordering.** `seq DESC` → `ASC` in `cloneFailures` left all
  four `TestReposBlock*` rows green. The executor's dedupe is per (resource, reason) and
  deliberately re-emits when the reason changes, so a repository that first failed `auth`
  and then failed `network` carries both on the log, and reading the older one sends the
  model — and the operator reading the prompt back — after a credential that stopped being
  the problem, while the block still claims to describe "the last clone attempt".
  `TestReposBlockNamesTheLatestFailure` seeds both and goes red on the mutant, quoting the
  stale line it rendered.

Two of the round's notes are accepted as known rather than fixed here. A `safeRepoMount`
refusal reaches the wire as reason `internal` with the generic message: the registered
vocabulary has no better fit, and the specific cause is named in the executor's logs — an
operator debugging a repository named `skills` sees a wire signal indistinguishable from a
platform fault. And the same guard refuses the sandbox workdir and its skills tree but not
a strict *ancestor* of a custom nested workdir, which is unreachable under the default
`/workspace`, whose only ancestor is `/` and already refused.

**Plan 25 progress summary (archived).** Two slices, two PRs: #329 (the wire half —
the `github_repository` create arm, migration 0020's `session_resource_credentials`,
live token rotation, the repo-delete rejection, unit W, and the e-wire-cli acceptance
recorded above) and the archiving PR (the clone: go-git materialization in the executor,
the `github_repository_clone_error` surface, the brain's cloud-gated block, the
`repo-answer` eval). #55 closes with it. The three decisions the user settled on
2026-08-06 all hold as built: the clone is platform-side via go-git, so the token never
enters the sandbox and the egress gate is never involved; only the platform half ships,
with BYOC materialization deferred to #322 and the brain's environment-kind gate keeping
that gap honest rather than silent; and a clone that fails surfaces as a `session.error`
and leaves the session running with its other repositories mounted.

## Second recording wave, registry reconciliation (#575) — review-hardening record (2026-09-04)

Twenty-four registry entries were rewritten from a 281-pair recording of the live
managed-agents endpoint. Eleven review passes found 65 defects between them — five,
three, fifteen, four, six, one, seven, five, five, eleven and three — and **twenty of the
65 were one defect repeated**: the recording was read carefully, our own code was not read at all, and the
entry was then filed as "confirmed, and we match". Those twenty came one from the
verifier's first run, three from Codex, nine from the Claude reviewer, none from the
refute-first pass, one each from the verifier's second through sixth runs, and two from
the automated reviewer on the pushed branch.
A finding is counted here when it changed a file in this repository — which is why the
verifier's third run contributes one of the four it raised, its other three having been
answered in the pull request rather than in the tree.

The verifier found five, of which the load-bearing one was a sentence claiming
`isSkillReadPath` "admits exactly the subtree the reference serves" — inside an entry
whose own first half says we additionally serve a route the reference answers 403 to.
The Codex reviewer found three more, each a mismatch recorded as a match:
`credential_host_unreachable_error` (we fire from a gate-config fetch against the live
policy, not at session start against a creation snapshot), the context-overflow
settlement, and the session-scoped `file_id`. The Claude reviewer found fifteen, nine of
them the same shape, including two entries that recorded a reference property and
compared only a third against ours, plus a claim that understated this platform's own
gate —
`cmd/gate` was described as an environment-variable forward proxy a hostile process
escapes by clearing `HTTP_PROXY`, when it installs owner-match iptables before it ever
serves the proxy, so such a process is dropped rather than let out.

Ten issues came out of the review passes rather than the recording: five of them
(#577, #578, #579, #581, #582) from the Codex and Claude passes, the registration of
the pre-existing #576, then #584 — a collision between a repository named `skills`
and the skills materialization root, raised as an unverified risk and real on
verification — and finally #589, #590 and #591 from the fifth pass. That is more
than the recording pass produced unaided.

A fourth pass put six agents on six of this PR's own factual claims, each told to
refute its claim and to default to refuted when unsure. Two held — the `unrestricted`
address floor and the clone shape. Four did not, in three shapes the earlier passes had
not shown. The `cmd/gate` security sentence, itself already a correction of an
overstatement in the other direction, was right about the mechanism down to the rule
order and the verification step, and wrong to state it unconditionally: a gate is
provisioned only for a `limited` or vault-attached session on a deployment configured
for one, so the `unrestricted` session the recording exercised had no firewall to be
dropped by. The skills entry claimed a **path** divergence that does not exist — the
bullet is relative to a workdir defaulting to `/workspace`, so it names exactly the
root the reference advertises, and only the template differs. The cache-creation reading
mistook a running session total for a per-turn one; re-derived over the archive, all 37
nested readings inside an event listing equal that listing's running span sum, and this
platform already accumulates the same way. And STATE.md's tracker count was off by one
from the moment it was written, because a text match for the pointer clause misses a
head whose `#78` is a later comma segment: `tools/registrycheck`'s own parser gave 113
there against the naive 112. The merged file has three such heads, and after the tenth
pass promoted two more `#78` residuals out of the parentheticals that hid them from the
guard it carries 117 — the figure STATE.md states, derived with the parser's grammar
rather than a text match.

So the defect has a mirror, and the mirror is the more expensive one. Assuming we
*match* without reading our code leaves a wrong entry in a document; assuming we
*differ* without reading it files an issue against working behavior. #581's path half
was exactly that, and it took an agent instructed to disbelieve the entry to find it.

A fifth pass, the verifier re-run after the fourth, found six more — and one was the
original defect again, in the entry nobody had thought to reread. The OAuth-refresh
entry records two reference behaviors, a past `expires_at` refused with a 400 and a
failing token endpoint that stays silent on the wire, and compares neither against this
platform, which differs on both: it accepts the past date, and it answers a failed
refresh with `mcp_authentication_failed_error` where the reference sent the stale token
and succeeded. Those are #589 and #590. The other five findings were bookkeeping the
earlier passes had left behind: an entry count stale in three documents; a `Tracked:`
clause that had moved two live residuals *into* the provenance tail the guard never
state-checks, one of them a recorded mismatch with no issue at all (now #591); totals in
this very record that did not add up; a section placement worth arguing rather than
fixing; and two comparison rows the archive counted as settled that had reached neither
the analysis file nor the registry.

Those two rows are worth their own line, because they settled in opposite directions.
Mixed-turn permission gating — whether an `always_allow` tool runs while an
`always_ask` confirmation from the same turn is outstanding — is the reference's own
behavior, gate for gate, so that entry left the divergences for the compatibility notes
and gave up its tracker. A blocked domain's answer to the model is the other way: the
reference returns a readable `is_error` tool result naming `url_not_allowed` and the
field that blocked it, which is the shape this platform already produces for a
different reason, and the divergence it keeps on purpose is that the fence is the
operator's rather than the agent's.

The sixth pass is the one to keep, because of where it found the defect. Answering the
fifth pass's mildest finding — an entry sitting in INFERRED though most of it was
settled — meant adding one sentence to justify the placement, and that sentence said
"three of those four are confirmed and matched" having read our source for one of the
three. Of the other two, one was true by luck; the other was not true at all. The
reference distinguishes a revoked environment key from a live one out of scope by
message text alone, at one status and one type, while `authenticateEnvironmentKey` sends
unknown, revoked and expired down a single branch **on purpose** — "so a probing client
learns nothing about which of them it hit" — and no live-key-out-of-scope state exists
here to compare against, no scope model being built. Two deliberate positions, recorded
as a match.

So the defect is not a lapse that better attention retires. It reappeared in a sentence
written *to answer a finding about that same entry*, at the fifth attempt, under a rule
this PR itself added to prevent it. The one thing that has reliably caught it is a
reader who opens our source with the entry's claim in hand and expects it to be wrong —
which is what the verifier and the refute-first pass both are, and what re-reading the
recording is not.

The seventh pass makes the sixth's point twice over. Sent to attack the replacement
sentence on the assumption that it too was wrong, it found that the conclusion had become
right while the *reason* was still asserted: "there is no live-key-out-of-scope state here
to compare against at all" is contradicted by our own code twice — `/v1/agents` under a
Bearer environment key falls to the management lane and answers 401 `missing x-api-key
header` for a live key and a revoked one alike, which is the same collapse by a second
route rather than an absence, and `workScope` already calls a key naming another
environment "a scope failure, not an authentication one". What is missing is named OAuth
scopes, not an authority model. The same pass also found the citation overreaching — #550's
body proposes a work-API admission split and says nothing of this surface, so the registry
was deferring to an issue that would not retire the gap when closed; the issue's scope has
been widened to match rather than the claim narrowed to fit.

It also turned up a wire difference nobody had **compared** — the reference's 404 was
recorded sixteen times over — inside the fact this record had already called true-by-luck: the reference answers `DELETE /v1/agents/{id}` a 404
`not_found_error`, where this platform answers a 405 `invalid_request_error` from the
method-less fallback. Capability matched, wire did not. It gets no issue, because the
pinned SDK's agent service exposes no delete at all and no typed client can reach it — but
"agents cannot be deleted here either" had stood as a match for two passes.

The eighth pass is the one that should have come first. Sent to attack the same two
sentences a fourth time, it did something no earlier pass had: it opened the recording's
own probe index and read **which route each probe hit**. Every prior pass, this one's
author included, had checked the entry against our code and taken its account of the
recording on trust. The premise did not survive. "Separated by message text alone at one
status and one type" was a pairing of two probes on two different routes — a live key
outside its scope answers 403 `permission_error` on the skills routes and 401
`authentication_error` on `/v1/agents`, so the reference's refusal is route-dependent and
no recorded route carries both states. Worse, the entry gave as the reason we *differ* an
anti-oracle posture that the work-API-auth entry, 165 lines above it in the same file,
records this platform **giving up in order to match** — two entries with opposite verdicts
on one recorded property, which is the single thing a divergence registry exists to
prevent. The corrected fact is smaller and duller: the reference's
refusal is route-dependent, the envelope matches on two of the four routes and differs on
the other two, and one absence is under all of it — no named-OAuth-scope vocabulary, which
is #550's gap.

The ninth pass is the one that stings, because the fix was the failure. Told the premise
was unsupported, this author wrote a correct account — and **appended** it, leaving the
refuted sentence standing byte-identical in the entry's first half. The entry then stated
a wire fact in bold and, some 1,500 characters later in the same paragraph, said that fact
was wrong. A reader skims the bold half. Two more claims went with it: "the envelope
matches wherever the recording reached" is falsified by two of the four routes the
recording reached — `GET /v1/skills` is management-only here, so an environment key draws
a 401 where the reference answers 403 `permission_error`, and `GET /v1/skills/{id}` this
platform admits outright where the reference refused it — and "a route it had reached with
200 minutes earlier" tied the revoked key to the live one when three keys were revoked at
teardown and the archive redacts which answered. Both were inherited from the analysis
file rather than derived. The fact is now rewritten in **both** halves, and the tally is
stated rather than asserted. Its remaining two were notes that changed files all the
same, which is why the sequence assigns that pass five: the 404-versus-405 difference had
been written as a claim about the `/v1/` namespace rather than as one route's difference
with neither side holding a house rule, and this record still read as though the
contradiction had been resolved when only half of it had.

The tenth pass is the automated reviewer, running on each push rather than on request,
and it found eleven across three batches — the largest count after the Claude reviewer's,
on a diff five passes had already been over. Two were the repeated defect. One was a
claim that the reference's address floor "is exactly `dialguard.IPAllowed`", written
without opening `internal/dialguard/dialguard.go`, whose package comment says the guard
covers loopback, link-local, the unspecified address and multicast and **deliberately
admits RFC 1918** because the on-prem premise puts legitimate endpoints on the operator's
own private network — and the probe recorded was `169.254.169.254`, link-local, so only
the half our floor does cover was ever observed. The other was the skills paragraph
asserting the reference "does not stage skills into a sandbox at all", refuted by the
Level-1 entry *this same wave added*: `/workspace/skills/` holds the extracted trees while
only the zips sit on the FUSE mount at `/mnt/skills`.

Chasing that second one turned up the sharper version no reviewer had raised. **"The
reference" is two implementations here, and the entry had been reading one recording
against both.** The SDK's agent toolset really does re-fetch per work item
(`Skills.Versions.Download` into `{workdir}/skills/{dirname}`); the platform-managed
sandbox the recording observed replaces that fetch with the mount. Neither skips the
extraction, so the sentinel's question is live on both paths — and the recording runs
against the argument the paragraph drew from it, re-extracting from a local mount being a
copy rather than a download. The file-mount entry's "does not stream bytes into a
container at all" was scoped for the same reason.

And then the ninth pass's lesson recurred inside the tenth pass's own fix, twice, which is
the last thing worth recording here. Both corrected paragraphs were written below an
earlier sentence that still said the refuted thing: the skills entry opened "The reference
re-downloads and re-extracts every skill each work item" — true of the toolset, false of
the managed sandbox the paragraph two thousand characters later had just split out — and
the egress entry's first mention of the floor asserted a "private/reserved" class that the
sentences after it now say was never probed beyond link-local. Neither was caught by a
reviewer. Both were caught by re-reading the whole entry after fixing part of it, which is
the only method that has worked on this defect: **a claim is not corrected until every
sentence that states it has been rewritten**, and the sentence a reader reaches first is
the one that has to be true.

Two of the eleven are worth keeping for what they say about this record's own claims. The
first is that **the ninth pass's defect had already recurred in a second entry** while
that pass was busy fixing the first: the Level-1 skill-injection entry was titled "the
block format and placement are inferred" and opened "the exact template is captured by no
source", with text appended below quoting the reference's template verbatim and stating
that this recording falsifies that sentence. A correction appended, the refuted sentence
left standing, one entry over from where it had just been fixed. The second is that **this
PR broke the routing rule it added**: the `DELETE /v1/agents/{id}` 405-against-404
mismatch was recorded with no tracker and an exemption argued from client reachability —
no typed client can reach it — which is precisely the "a bug is not a divergence to be
argued" shape the new INFERRED preamble forbids two sections above. It is #592 now, and
the argument moved to the issue where it can be weighed.

One finding went the other way, and it is the mirror worth recording: the reviewer read a
citation naming two listings, concluded the entry's "all three listings are
forward-chronological" was wrong, and proposed a rewrite to "both listings" — which would
have introduced an error. The recording's own index settles it: batch2 idx 41, 43 and 44
are three listings, and the citation was what was short, having omitted
`session.events.list.mixed-perm-after-confirm` along with the session-create and send
probes. The count was right; the evidence for it was not written down. Fixed by completing
the citation to all seven probes rather than by trusting either the entry or the
suggestion — the same discipline the rest of this record is about, applied for once in the
entry's favour.

The remaining bookkeeping: a function named in an entry that exists nowhere in the tree
(`snapshotResources` for `materializeResourceInputs`, raised twice), a first-wave quote
re-announced as a second-wave finding under a title dated a day late, a clause whose
grammatical subject made a statement about this platform read as one about the reference,
two "we are simply wrong" issues named only in body prose where the guard cannot see them
(#576, #582), two live `#78` residuals inside another issue's `Tracked:` parenthetical
rather than heads of their own, and the count sentence in this record.

The eleventh pass is the verifier again, and it found the *other* defect a fifth time, in
a third entry, on a line neither the ninth pass's fix nor the tenth pass's sweep had
touched. The `Offering an MCP tool to the model` entry's first half was byte-identical to
main's: the reference "gives the model a **bare** tool name", and `mcp__{server}__{tool}`
is "**ours and forced by architecture**", with the collision rule and one of the five
drop reasons resting on that premise. Some two thousand characters later, in the same
line, the half this PR appended says the recording overturns exactly that — the name the
reference shows the model is composed, the prefix is reserved at agent create, and there
is no 64-character ceiling. Two bolds contradicting each other in one entry a reader
meets top to bottom.

That is five instances, each in a different entry: the resource-lifecycle entry (ninth
pass), the Level-1 entry (tenth), the skills entry's opening clause and the egress entry's
first mention of the floor (both caught while fixing the tenth), and now this one. By this
point every one of those claims had been checked against the bytes, so the pattern is not
carelessness about what is true. It is that **a correction gets written where the new
evidence is discussed, and the sentence that first states the claim is somewhere else** —
usually above it, usually in bold, and always the one a reader reaches first. So the rule
the whole exercise reduces to is mechanical rather than attitudinal: after settling an
inference, find every sentence in the entry that states the old claim and rewrite each of
them, beginning with the earliest.

Its other two findings changed files as well. The ninth pass's tally of five had a
narration naming three, now reconciled — the missing two were notes that nonetheless
changed the tree. And the skill-reads entry claimed one environment key "produced four
different refusals" directly above an enumeration listing two 403s and a 200; it now
states the route-by-route split and leaves the four-route tally to the entry that carries
it. A third note needed no change and is recorded because it is a trap: CodeRabbit's check
was green on a review it never performed, its rate limit having been reached, so a green
mark there is not a review and is not counted as one.

That is the shape of the whole exercise, stated once more because it took eleven passes to
see it plainly. The defect was never carelessness about our code — by the fourth pass our
code was being read line by line. It was that **the recording's own account was the last
thing anybody checked**, because it arrived as the authority and everything downstream
inherited it. A registry entry has two halves, and this project had built five kinds of
scrutiny for one of them.

**The transferable rule, now written into the INFERRED preamble as a three-way routing
test:** settling an inference has three ends — confirmed and we match, confirmed and we
differ on purpose, or we are simply wrong — and only reading our code decides which.
Recording what the reference does is not checking what we do. `tools/registrycheck`
enforces the mechanical half and caught two entries settled in place and left in the
wrong section, but it cannot catch a prose claim about our own behavior; only a reader
with the source open can.
