---
status: archived
issue: "#77"
---

# Session outcomes: `user.define_outcome`, the grader loop, and `outcome_evaluations` (plan 21)

> **Archived 2026-08-03 — completed.** All five slices delivered (PRs #255, #257, #258,
> #260, and the slice-5 acceptance PR); the progress summary and the acceptance-run
> record are docs/HISTORY.md § "Outcomes plan (21)".

The outcomes surface is the reference's "give the agent a goal and a rubric, and the
platform grades the work" loop: a client sends one `user.define_outcome` event, the agent
starts working immediately, and after each work cycle a platform-provisioned **grader**
(a separate model context) scores the deliverables against the rubric — feeding failures
back to the agent for revision up to `max_iterations` times — while the session resource
mirrors the state in `outcome_evaluations[]` and the log carries a
`span.outcome_evaluation_start/_ongoing/_end` trio per cycle. This plan implements that
surface end-to-end, closes the three v1 placeholders that reserved its seams
(the `user.define_outcome` rejection in `internal/events/inbound.go`, the hard-coded
`outcome_evaluations: []` in `internal/api/sessions.go`, the missing `outc_` prefix in
`internal/domain/id.go`), absorbs #161 (`initial_events` on session create — half its
union *is* `user.define_outcome`), and opens plan 08's reserved session-outputs seam so
the reference doc example's final step (retrieve deliverables by `scope_id`) works.

Tracking issue: [#77](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/77).
Absorbed: [#161](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/161).
Acceptance replays the reference's own example —
<https://platform.claude.com/docs/en/managed-agents/define-outcomes> — against this
platform with the **latest** Anthropic SDK (see Acceptance; versions re-checked on
execution day).

## Ground truth (pinned 2026-08-02)

### Wire surface — anthropic-sdk-go v1.61.0 (latest release, 2026-07-24)

The outcome surface landed in SDK v1.41.0 (2026-05-06, "add support for Managed Agents
multiagents and outcomes, webhooks, vault validation") and **no changelog entry since
touches it** — it is byte-identical between the v1.59.0 pin this plan was authored against and
v1.61.0. Slice 1 bumps to v1.61.0 so acceptance runs on the latest release (the user-facing goal),
with the two in-between releases' non-outcome surface swept and recorded.

- **Inbound params** (`betasessionevent.go:6406-6416`,
  `BetaManagedAgentsUserDefineOutcomeEventParams`): `type: "user.define_outcome"`,
  `description` (required — "What the agent should produce. This is the task
  specification."), `rubric` (required union), `max_iterations` (optional int —
  "Eval→revision cycles before giving up. Default 3, max 20.").
- **Rubric union** — `{type: "text", content}` (`betasessionevent.go:5649-5669`;
  "Plain text or markdown — the grader treats it as freeform text. Maximum 262144
  characters.") or `{type: "file", file_id}` (`betasessionevent.go:1995-2015`).
- **Server echo** (`betasessionevent.go:6292-6320`,
  `BetaManagedAgentsUserDefineOutcomeEvent`, all fields required): `id`, `description`
  ("Copied from the input event"), `max_iterations`, `outcome_id` ("Server-generated
  `outc_` ID … Referenced by `span.outcome_evaluation_*` events and the session's
  `outcome_evaluations` list"), `processed_at`, `rubric`, `type`.
- **Span trio** (all required fields):
  - `span.outcome_evaluation_start` (`betasessionevent.go:4875-4910`): `id`, `iteration`
    ("0-indexed revision cycle. 0 is the first evaluation; 1 is the re-evaluation after
    the first revision"), `outcome_id`, `processed_at`, `type`.
  - `span.outcome_evaluation_ongoing` (`betasessionevent.go:4835-4873`): same fields;
    "Periodic heartbeat … Distinguishes 'evaluation is actively running' from
    'evaluation is stuck'".
  - `span.outcome_evaluation_end` (`betasessionevent.go:4776-4833`): adds `explanation`,
    `outcome_evaluation_start_id`, `result`, `usage` (`BetaManagedAgentsSpanModelUsage`
    — "Token usage for a **single model request**", with nullable `speed`). Result
    semantics, verbatim from the type: "'satisfied': criteria met, session goes idle.
    'needs_revision': criteria not met, another revision cycle follows.
    'max_iterations_reached': evaluation budget exhausted with criteria still unmet —
    one final acknowledgment turn follows before the session goes idle, but no further
    evaluation runs. 'failed': grader determined the rubric does not apply to the
    deliverables. 'interrupted': user sent an interrupt while evaluation was in
    progress."
- **Session resource** (`betasession.go:977-1021` + `1035-1037`):
  `outcome_evaluations` is a required array, "One entry per define_outcome event sent to
  the session", each entry `{type: "outcome_evaluation", outcome_id, description,
  explanation, iteration, result, completed_at}` with `result` — "`pending` before the
  agent begins work; `running` while producing or revising; `evaluating` while the
  grader scores; `satisfied`/`max_iterations_reached`/`failed`/`interrupted` are
  terminal" — and `explanation` "Grader's verdict text from the **most recent**
  evaluation" (one entry mutated in place per cycle, not one entry per cycle).
- **Session create** (`betasession.go:2082-2084`): `initial_events` — "processed in
  order. Supports `user.message` and `user.define_outcome` events. Maximum 50 events."
- **No new endpoints, no new stop_reason.** The surface rides the existing session +
  events endpoints (api.md; sessions → response types lists
  `BetaManagedAgentsOutcomeEvaluationResource`). The `session.status_idle` stop_reason
  union stays `end_turn | requires_action | retries_exhausted`
  (`betasessionevent.go:4218-4221`) — outcome terminality lives in the evaluation's
  `result`, and a satisfied/failed/exhausted session idles with plain `end_turn`.
- **Adjacent surface deliberately out of scope here**: the
  `session.outcome_evaluation_ended` webhook event (`betawebhook.go:1105-1120` — this
  platform has no webhook delivery at all), deployments' own `initial_events`
  define_outcome variant (`betadeployment.go:830-849` — deployments are #51), and
  session threads' echo of the event (threads are #53).

### Reference behavior — public docs (fetched 2026-08-02)

From <https://platform.claude.com/docs/en/managed-agents/define-outcomes> unless noted:

1. "The agent begins work immediately. No additional user message event is required."
2. The event "is processed on receipt and echoed back with `processed_at` already
   populated" — one of exactly three such exceptions, alongside
   `user.custom_tool_result` / `user.tool_result` (events-and-streaming page; this
   platform already records a deliberate divergence for the other two —
   docs/DIVERGENCES.md "processed_at on inbound tool results").
3. "Only one outcome is supported at a time, but you may chain outcomes in sequence. To
   do this, send a new `user.define_outcome` event after the terminal
   `span.outcome_evaluation_end` event of the previous outcome."
4. "The harness automatically provisions a *grader* to evaluate the artifact against a
   rubric. The grader uses a **separate context window** to avoid being influenced by
   the main agent's implementation choices." The grader "returns an explanation
   summarizing which criteria passed or failed … That feedback is handed back to the
   agent for the next iteration." Heartbeats: "The grader's internal reasoning is
   opaque: you see that it's working, not what it's thinking."
5. Status reads two ways: "listen on the event stream for
   `span.outcome_evaluation_end`, or poll `GET /v1/sessions/{session_id}` and read
   `outcome_evaluations[].result`."
6. `interrupted` is "emitted when the session is interrupted while an outcome is active,
   **even if evaluation hadn't started yet**. If no `outcome_evaluation_start` fired
   before the interrupt, `outcome_evaluation_start_id` is an empty string." An interrupt
   "pauses work on the current outcome and marks the end result `interrupted`, allowing
   you to kick off a new outcome."
7. `initial_events` (sessions page): a non-empty list "starts the agent loop in the same
   call: the session is created directly in the `running` status"; 400 on "More than one
   `user.define_outcome` event", "A `user.define_outcome` event without a `rubric`",
   "More than 100 file-sourced `document` content blocks across the whole list"; 413 on
   a body over 32 MB.
8. Deliverables: "The agent writes output files to `/mnt/session/outputs/` inside the
   sandbox. Once the session is idle, fetch them through the Files API scoped to the
   session" (`GET /v1/files?scope_id={session_id}`, then
   `GET /v1/files/{file_id}/content`). Plan 08 built the registry and the `scope`/
   `downloadable` columns as an explicit seam: "session-generated outputs are future
   work; the column and `scope` fields are the seam they'll land in."
9. Mid-outcome `user.message` is allowed "to direct the agent's work as it progresses,
   but it isn't required"; after a terminal evaluation "the session can be continued as
   a conversational session, or a new outcome can be started."
10. The `ant` CLI is pure pass-through (no outcome subcommand, no typed construction, no
    outcome rendering; `--initial-event` and `--event` carry arbitrary YAML/JSON), and
    its delta whitelist stays `agent.message`/`agent.thinking` — outcome span events are
    never preview-streamed, only delivered as committed frames.

### What no source pins (each becomes an INFERRED divergence entry, recording tracked by #78)

The grader's model, prompt, and inputs; the exact evaluation trigger; the revision
feedback's injection shape; heartbeat cadence; event ordering among the echo,
`session.status_running`, and the first `span.outcome_evaluation_start`;
`outcome_evaluations[].completed_at`'s value before a terminal result; the error shape
for a second `user.define_outcome` while one is active; out-of-range `max_iterations`
handling; the `iteration` value on an `interrupted` end event that had no start; whether
`usage.speed` rides the wire as `null`; `max_iterations` boundary semantics (see
Decision 6).

## Design decisions

1. **Storage: an `outcome_evaluations` jsonb column on the sessions row**, exactly the
   `resources`/`usage` verbatim-jsonb precedent — one migration
   (`ALTER TABLE sessions ADD COLUMN outcome_evaluations jsonb NOT NULL DEFAULT '[]'`),
   stored in the wire shape, mutated **only inside the same append transaction as the
   event that changes it**, under the session row lock (the `AddUsage` read-modify-write
   in `internal/events/log.go` is the template — a new `AppendOptions` member). The
   event log stays the source of truth; the column is a projection that can never drift
   from it because they commit together. Rendering is then plan 08's `resourcesJSON`
   pattern: extend `sessionColumns`/`sessionRow`/`scanSession`, replace the hard-coded
   `[]` in `renderSession`, and every session-returning endpoint (get/list/create/
   update/archive) picks it up at once. Queue surface: **grading itself never becomes
   queue work** — it is a brain-side model call (Decision 5) — but slice 4 adds one
   internal work kind, `outputs_harvest` (the `0015_web_exec_kind.sql` CHECK-rewrite
   pattern; internal-only, never served to BYOC `Poll`), because reaching a sandbox is
   executor work under the architecture's brain/hands split (Decision 8).
2. **One active outcome, enforced at send.** A `user.define_outcome` while another
   outcome is non-terminal → 400 `invalid_request_error` (message ours; exact reference
   shape unrecorded → INFERRED). Enforced in the send transaction alongside
   `ValidateToolResults` (it needs the DB, so it lives in the API transaction, not in
   `NormalizeInbound`). The same transaction validates a `file` rubric's `file_id`
   against the files registry — existence within the org scope (v1's single-tenant
   authorization boundary, the same policy every management-key file reference gets),
   with a rubric-file byte cap mirroring the text rubric's bound (256 KiB, ours) —
   and the slice's tests cover missing and oversized rubric files (a foreign-scope
   case is unconstructible under v1's single-tenant registry — the whole registry is
   the org scope; the test lands when multi-tenancy does).
   The accepted rubric's bytes are **snapshotted at acceptance** to an outcome-owned
   blob key (`outcomes/{outcome_id}/rubric`, written in the send transaction's wake):
   replay and the grader read the snapshot, never the source file, so deleting the
   source file mid-outcome cannot break replay — a delete-then-replay test pins it.
   Field validation in `normalizeDefineOutcome` (`internal/events/inbound.go`): strict
   `allowKeys(type, description, rubric, max_iterations)`; `description` required and
   non-empty (the SDK pins only requiredness — rejecting the present-but-empty string
   is ours, INFERRED); rubric union exact (`text` content ≤ 262144 chars, `file`
   requires `file_id`); `max_iterations` integer 1–20, default 3 (out-of-range → 400,
   our choice, INFERRED). The normalizer mints
   `outcome_id: domain.NewID(domain.PrefixOutcome)` into the payload so the echo is the
   usual payload+envelope merge.
3. **`processed_at`: extend the existing recorded divergence, not the docs' words.**
   The docs put `user.define_outcome` in the processed-on-receipt trio; this platform
   already deliberately echoes the other two members with `processed_at: null` and
   stamps at turn settlement, because receipt-stamping would break `pendingInput`
   chaining (docs/DIVERGENCES.md, the "processed_at on inbound tool results" entry, and
   `user.define_outcome` must join `pendingInputTypes` for exactly the same reason — a
   define_outcome landing mid-turn must chain the next turn, not idle past it). The
   registry entry already names all three events; slice 2 re-verifies its recorded
   rationale against the new wake trigger and leaves the entry standing — no rewrite,
   same-PR confirmation only. (Today the entry's "ours echoes all three" phrasing
   overclaims — define_outcome is rejected, not echoed; slice 2's landing is what makes
   the sentence true, which the confirmation pass states in its record.)
4. **Trigger and replay.** In `sendSessionEvents`' state-machine switch, a
   `user.define_outcome` on an idle session behaves like `user.message`: flip
   `status_running` + enqueue a turn ("begins work immediately"). In replay
   (`internal/brain/replay.go`), the event renders as a user-role message built
   deterministically from its payload — the outcome description plus the rubric text —
   so any fresh brain reconstructs the same conversation; grader feedback for revision
   cycles is likewise injected at replay time as a user-role message derived from each
   `needs_revision` `span.outcome_evaluation_end` on the log (deterministic ⇒ no extra
   persisted event, crash-safe by construction). Both renderings are ours → INFERRED.
   A `file` rubric's content is read from the **blob store** — a new read-only
   `blob.Store` dependency wired into `cmd/brain` in slice 3 (precedented: controlplane
   and executor already carry it; the brain reads Postgres and blobs to assemble
   requests, and still never touches a sandbox — the brain/hands split is about tool
   execution, and stays intact).
5. **The grader is one model call at turn settlement, in a separate context — and it
   never reads the sandbox.** Hook point: the brain's `settle` path — the one place a
   finished turn decides between chaining and idling (`internal/brain/brain.go`). When a
   turn would settle idle with `end_turn` and the session has a non-terminal outcome,
   the brain instead: emits `span.outcome_evaluation_start` (flipping the stored entry
   to `evaluating`), runs **one** provider request (the SDK's `usage` doc — "a single
   model request" — corroborates single-call grading) in a fresh context; emits
   heartbeat `span.outcome_evaluation_ongoing` every 30s (cadence ours, INFERRED) while
   the call runs; then emits `span.outcome_evaluation_end` with the verdict,
   explanation, and the call's usage. **Lock discipline mirrors a model turn's**: the
   start commits in its own short append transaction, the grader call runs with **no
   session lock held** — the work item's lease keeps ownership, exactly as `runTurn`
   holds no lock while streaming a model response — heartbeats are short appends, and
   only the verdict/state commit takes the settle-shaped locked transaction; a grading
   pass that serialized event sends (or the documented `user.interrupt`) behind a
   minutes-long lock would be a defect, and the slice's tests assert an interrupt lands
   mid-grading. Grader inputs, in two stages so the brain/hands
   split survives: **slice 3** — grader instructions (ours), the rubric content (text
   inline; file rubric via the Decision-4 blob read), the outcome description, and a
   read-only rendering of the session transcript (which already carries every tool call
   and result the agent produced); **slice 4, cloud environments** — additionally the
   harvested deliverables (Decision 8): the files-registry listing for the session
   (names/sizes, a Postgres read) plus small text deliverables' bytes from the blob
   store under a named budget (default 64 KiB total; bounds ours). All grader inputs
   come from Postgres and the blob store — sandbox access stays executor-side, behind
   the harvest work item. (From slice 4 on, an `end_turn` settlement with an active
   outcome first routes through that harvest item — Decision 8 — and the grading pass
   runs as the chained brain item that follows; in slice 3, and always on `self_hosted`,
   grading runs directly at settlement.) Exact reference grader inputs are unobservable
   → INFERRED.
   Span events and the OTel span come from one instrumentation point — clone the
   `events.StartModelRequest` same-source pattern in `internal/events/span.go` (design
   principle 3). The grader rides the session's own resolved model config (model choice
   INFERRED — the reference names no grader model).
6. **Verdict handling.** `satisfied`/`failed` → entry terminal (+`completed_at`), session
   idles `end_turn`. `needs_revision` → entry back to `running`, `iteration+1`, and the
   brain chains a revision turn (the `chained` mechanics `settle` already has) whose
   prompt carries the grader's explanation (Decision 4). Budget: up to `max_iterations`
   evaluation cycles total (iterations `0 … max_iterations-1`); a would-be
   `needs_revision` on the final cycle is reported as `max_iterations_reached`
   (boundary reading INFERRED), after which **one final acknowledgment turn** runs — an
   agent turn told the budget is exhausted (prompt ours, INFERRED) — and the session
   idles with no further evaluation. `user.interrupt` with an active outcome emits an
   `end` event with `result: "interrupted"` (in the interrupt's own append transaction;
   `outcome_evaluation_start_id: ""` when no start fired, `iteration` = the entry's
   current value) and the entry goes terminal — extending the existing interrupt path in
   `internal/api/events.go`. `completed_at` is `null` until terminal (INFERRED).
   Evaluation triggers **only** on an `end_turn` settlement: a turn that faults to
   `retries_exhausted`, or idles `requires_action` on a blocking tool ask, leaves the
   entry non-terminal (`running`) and the outcome resumes with the session's next wake;
   a session terminated mid-outcome leaves the entry frozen as-is (both ours, INFERRED,
   recording-flagged). **Grading-failure recovery**: a provider error or timeout during
   the grader call retries under the brain's existing bounded turn-retry machinery; on
   exhaustion — and on a brain crash mid-grading (the item lease reclaims) — the cycle's
   committed `span.outcome_evaluation_start` is left dangling (the recorded
   crash-window-residue divergence pattern), the entry reverts to `running`, and the
   outcome re-evaluates at the session's next settlement. `iteration` increments only on
   a committed `needs_revision` end event, so a re-run repeats the same cycle number,
   and an entry can never stick in `evaluating` — that state is held only while a live
   leased item is actually grading. Tests cover provider-fault, crash-replay, and
   interrupt-mid-grading paths.
7. **`initial_events` lands whole (#161).** `createSession` accepts the key: an ordered
   list restricted to **exactly** `user.message` and `user.define_outcome` (the docs
   name only these two; any other event type → 400), max 50, running each element
   through its `NormalizeInbound` per-type normalizer behind that two-type allowlist,
   plus the create-specific 400s (more than one define_outcome; define_outcome without
   rubric — which strict rubric validation already yields; more than 100 file-sourced
   document blocks across the list) and the 32 MB / 413 body bound; a non-empty list
   creates the session directly in `running` with a turn enqueued, events appended in
   order in the create transaction. The #161 divergence entry retires in the same PR.
8. **Deliverables harvest is executor work through the queue, and it runs *before*
   grading.** Mechanism: whenever the brain settles a turn on a session with an active
   outcome (cloud environments), the settlement transaction enqueues an internal
   `outputs_harvest` work item (Decision 1); the cloud executor claims it, walks regular
   files under `/mnt/session/outputs/` in the session sandbox, uploads content to the
   blob store, and upserts files-registry rows with `scope_type: "session"`, `scope_id`,
   `downloadable: true` — opening exactly the seam plan 08 reserved ("session-generated
   outputs are future work; the column and `scope` fields are the seam they'll land
   in"), and making `GET /v1/files?scope_id={session_id}` + content download work as the
   doc example's step 4 expects. Harvest completion enqueues the brain's grading pass in
   the same transaction (the existing chaining pattern), so the grader always sees the
   cycle's current deliverables and a crash between the two loses nothing. **Each
   harvest is a snapshot** of `/mnt/session/outputs/`: the migration adds what the
   `0008_files.sql` schema deliberately lacks — the sandbox-relative path stored as
   `filename` plus a unique index on `(scope_id, filename)` — re-harvest replaces
   content per path, **deletes registry rows whose path no longer exists** (a rename is
   naturally delete+add; blob bytes removed best-effort, the plan-20 delete-convergence
   posture), and the listing and grader input therefore always mirror the **eligible
   snapshot** of the directory as of the last harvest — no stale rows (semantics ours,
   INFERRED; deletion and rename tests included). **Publication is atomic**: content
   uploads stage to the blob store first, then one registry transaction commits the
   whole snapshot (upserts + stale-row deletions) and enqueues the grading pass — a
   crash before that transaction leaves the previous snapshot fully intact (staged
   blobs orphaned by a crash are reaped best-effort), and crash tests sit at the
   upload, registry-commit, and grading-enqueue boundaries. Caps named here so the
   slice cannot silently invent them: 50 MiB per file, 200 files / 500 MiB per session
   (bounds ours), applied **deterministically** — paths in lexicographic order until a
   cap is hit; an ineligible file (oversize, or past the aggregate cap) is excluded
   from the snapshot exactly like an absent path, its prior row deleted, the exclusion
   logged — so clients and the grader always see the eligible snapshot and nothing
   half-in (growth-past-cap and aggregate-cap-selection tests included).
   Harvest is scoped to sessions with outcomes in this plan (the deliverables surface is
   documented on the outcomes page); generalizing to every session is a follow-up issue
   slice 5 files — #263, designed in docs/plan/38_idle-outputs-harvest.md. `self_hosted` environments get no harvest — the platform cannot reach
   a BYOC sandbox, and the reference worker has no file lane at all (plan 08's finding)
   — recorded as a deliberate divergence in the same PR.
9. **Not conversation-gated.** Outcomes work on both environment kinds — on
   `self_hosted` with the documented degradation that grading is transcript-only, no
   harvest and no deliverable bytes (Decisions 5 and 8) — and coexist with mid-outcome
   `user.message` traffic (it chains into the current work cycle; the grader still runs
   at settlement). After a terminal outcome the session is an ordinary conversational
   session again, and a new `user.define_outcome` is accepted.

## Out of scope

The `session.outcome_evaluation_ended` webhook (no webhook infrastructure exists —
recorded as a divergence with a tracking issue slice 5 files, not built), deployments
and their `initial_events` (#51), session threads (#53), the session `stats` fields (the
shared "stats / outcome_evaluations / deployment_id" divergence entry splits; stats
stays deferred), any grader-quality evals beyond the acceptance run (a rubric-grading
eval belongs to the `evals/` suite later, under `RUN_EVALS` — tracked by an issue slice
5 files).

## Slices (each lands as its own PR, TDD-first, `make verify` green)

Lifecycle per CLAUDE.md: slice 1's PR — the first that starts development — flips this
plan to `in-progress` and takes over STATE.md's Active work/Tasks. **Every slice lands
its docs/DIVERGENCES.md entries in the same PR that introduces the behavior** (the
registry's same-PR rule; plan 15 states it verbatim as a warning against batching) —
slice 5 adds no registry entries of its own.

1. **SDK bump v1.59.0 → v1.61.0** (the plan 05/11 ritual, deliberately folded in rather
   than a standalone bump plan — the bump exists for this plan's acceptance goal, and
   the ritual is preserved whole): bump, full `git diff` sweep of the two releases with
   a verification record in docs/HISTORY.md, including plan 11's "second trap" — every
   pinned-version label and every SDK `file:line` citation in the registry and docs
   re-read at the new version. Surface facts the sweep must respect: `tool_change`
   (the `tool_addition`/`tool_removal` variants) is genuinely new at v1.61.0 → handle or
   record (#160 pattern); `model_context_window_exceeded` already exists at the v1.59.0
   pin on the beta surface this platform mirrors (betamessage.go, 3 occurrences) and the
   brain's turn-classification divergence entry already handles it by name — the
   in-between releases add only its non-beta appearance plus one beta doc-comment line,
   so it needs **no** new record; the two betasessionevent.go hunks between the pins are
   comment-only, re-wording the event-list params — the `created_at[gt/gte/lt/lte]`
   filters and `order` are now documented as compared against / ordered by
   `processed_at`, a list surface this platform mirrors, so the sweep checks our list
   semantics against the re-worded contract and notes the result in the record.
   → verify: `make verify`; record complete; citation re-read done.
2. **Domain + acceptance + storage + rendering** (closes the #77 placeholders and #161):
   `PrefixOutcome` in `internal/domain/id.go` (+`knownPrefixes`); domain outcome types
   (`internal/domain/outcome.go`, stdlib-only); span event constants in
   `internal/domain/event.go`; `normalizeDefineOutcome` replacing the inbound.go
   rejection (Decision 2's validation); the send-transaction single-active check and
   file-rubric existence check; the `0016` migration + `AppendOptions` outcome mutation
   + `sessionColumns`/`renderSession` wiring (entry born `pending` at receipt); the
   idle-session wake trigger + `pendingInputTypes` membership; `initial_events` on
   create (Decision 7). Flip the pinned rejection tests into acceptance/echo-shape
   tests; wire tests diff every rendered shape field-by-field against SDK v1.61.0.
   Same-PR registry work: retire the define_outcome-rejection entry and #161's
   initial_events entry; narrow the shared stats/outcome_evaluations entry to
   stats/deployment_id; add the INFERRED entries this slice's behavior creates
   (single-active 400 shape, out-of-range max_iterations 400); confirm the processed_at
   entry per Decision 3.
   → verify: `make verify`; `user.define_outcome` send → echo carries `outc_` id;
   session GET shows a `pending` entry; initial_events session is born `running`.
3. **Brain grader loop** (transcript-stage grading): replay rendering +
   revision-feedback injection + the brain's new read-only `blob.Store` dependency for
   file rubrics (Decision 4); the settle-site evaluation (Decision 5, slice-3 inputs)
   with the span trio's same-source instrumentation and heartbeats; verdict handling,
   iteration budget, acknowledgment turn, interrupt → `interrupted`, non-`end_turn`
   settlement rules (Decision 6); stored-entry state flips
   (`pending→running→evaluating→…`) riding the existing append transactions.
   Deterministic tests script the provider (`internal/modeltest` fake) through
   satisfied / needs_revision→satisfied / max_iterations / failed / interrupted paths
   and assert the full event sequences and projection states. Same-PR registry work:
   the INFERRED entries for grader model/prompt/inputs, replay renderings, heartbeat
   cadence, budget boundary, acknowledgment turn, `completed_at: null`, non-`end_turn`
   settlement, and event ordering around the cycle.
   → verify: `make verify`; scripted end-to-end state machines green.
4. **Deliverables harvest** (Decision 8): the `outputs_harvest` internal work kind +
   migration (path-as-filename + unique `(scope_id, filename)` index); settlement-time
   enqueue, executor harvest, chained grading pass; grader inputs upgraded with the
   registry listing + deliverable bytes (Decision 5, slice-4 stage); caps + idempotency
   tests against the sandbox contract suites. Same-PR registry work: the INFERRED
   entries for harvest path/idempotency/caps and the deliberate self_hosted
   no-harvest/transcript-only-grading divergence.
   → verify: `make verify`; a file written to `/mnt/session/outputs/` in a docker
   sandbox appears in `GET /v1/files?scope_id=…` and downloads byte-identically; the
   grader's request demonstrably carries the listing.
5. **Full-chain acceptance** (below) + docs settlement: CHANGELOG narrative, HISTORY
   acceptance record, README status line, plan archived with its progress summary moved
   to HISTORY, #77 and #161 closed, follow-up issues filed (webhook divergence tracking,
   grader-quality eval under `RUN_EVALS`, generalizing harvest beyond outcome sessions).
   No new registry entries — slices 2–4 already carried their own.
   → verify: verifier PASS including its wire-compat and docs-consistency rungs.

## Acceptance — the doc example, end to end, on the latest SDK

Replay <https://platform.claude.com/docs/en/managed-agents/define-outcomes> — the DCF
Model Rubric example — against the local compose stack
(controlplane+brain+executor+Postgres+MinIO), with the `.env` model endpoint, under the
live-tier consent contract (as built, its own `RUN_LIVE_ACCEPTANCE_TESTS=1` tier; never
in `make test`'s default path). **SDK versions are re-checked on execution day** — as of 2026-08-02 the latest
releases are Go v1.61.0 (2026-07-24), Python 0.120.2 (2026-07-28), TypeScript 0.115.0
(2026-07-24); outcome wire types are identical across them (all landed 2026-05-06).

Three clients, in order of authority:

1. **Go SDK at the bumped latest pin (v1.61.0)** — a live integration test replaying the
   example's Go tab step-for-step: upload `rubric.md` via `client.Beta.Files.Upload` →
   create the session ("Financial analysis on Costco") → send `user.define_outcome`
   (both rubric variants exercised: `file` referencing the upload, and `text` with the
   rubric inline; `max_iterations: 5`) → watch the stream for the
   `span.outcome_evaluation_start/_ongoing/_end` trio → poll
   `client.Beta.Sessions.Get` reading `outcome_evaluations[].result` through
   `pending/running/evaluating` to a terminal value → list deliverables
   `client.Beta.Files.List` with `ScopeID` **and**
   `Betas: []anthropic.AnthropicBeta{anthropic.AnthropicBetaManagedAgents2026_04_01}`
   (the doc example passes the managed-agents beta on the scope-filtered files call;
   our server ignores the header, but the replay is byte-faithful) and download. Every
   response decodes through the SDK's typed structs with zero `ExtraFields` surprises
   and all `api:"required"` fields present.
2. **The real `ant` CLI** (built from the read-only checkout; it embeds SDK v1.61.0) —
   the example's CLI tab: `ant beta:sessions create --initial-event` (the
   define_outcome-in-initial_events variant the page's Note describes),
   `beta:sessions:events send`, `beta:sessions retrieve --transform
   'outcome_evaluations'`, `beta:files list --scope-id … --beta
   managed-agents-2026-04-01`, `beta:files download`.
3. **curl** — the raw-wire tab, captured as the acceptance record's transcript
   (docs/HISTORY.md) with the `anthropic-beta: managed-agents-2026-04-01` header
   asserted present on every request the example sends it on, giving the byte-level
   evidence the typed clients abstract away. **Transcripts are redacted before they
   are committed**: `x-api-key`/`Authorization` values and any live endpoint identity
   replaced with placeholders, no sensitive file contents, and `.env` /
   `model-providers.json` stay gitignored as ever — the record proves shapes and
   ordering, never credentials.

The recorded run (transcripts, event orderings, session projections) lands in
docs/HISTORY.md as this plan's acceptance record — and doubles as the platform-side
baseline for the #78 reference-recording comparison when a real managed-agents endpoint
is available. A scripted-model rehearsal of the same harness (deterministic, no live
key) guards the mechanics in CI so the live run is the only paid step.
