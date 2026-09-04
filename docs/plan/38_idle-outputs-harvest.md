---
status: archived
issue: "#263"
---

# Session outputs harvested at idle — files without an outcome (plan 38)

Today a session's `/mnt/session/outputs/` files only ever reach the Files API when the
session has an active outcome: `settleEndTurn`'s grading branch is the only call site of
`queue.OutputsHarvest` in the repo (`internal/brain/grader.go:948-951`), so a plain
session — no `user.define_outcome` ever sent — idles with `end_turn` and nothing is ever
harvested; `GET /v1/files?scope_id=...` stays `{"data":[]}` forever (confirmed against a
local compose stack driving the `create-slides` workshop, issue #263's 2026-09-03
comment). The reference docs describe no such requirement:

> "Files the agent writes to `/mnt/session/outputs/` appear in the list shortly after the
> agent finishes writing them, sometimes a few seconds after the session goes idle."
> — platform.claude.com/docs/en/managed-agents/files, "Listing and downloading session
> files"

The owner has decided: harvest when a turn ends and the session goes idle, with no
outcome required, reusing the existing harvest path (`internal/executor/harvest.go`'s
walk, caps, and snapshot-replace logic) rather than building a second one. This plan
designs the trigger, the settlement-time fence that replaces the outcome-evaluating
check, and the coordinated fix to a gate that would otherwise make the whole feature
silently inert.

## 1. Scope and goal

**In scope**: a second reason to enqueue `queue.OutputsHarvest` — a cloud session folding
to true session-level idle with no outcome active, whether or not one was ever defined —
fired from the five *brain* settlement paths that can fold a session there *outside* an
outcome cycle: `settleEndTurn`'s existing non-grading branch
(`internal/brain/grader.go:964-1003`), `brain.go`'s retries-exhausted `settle`
(`internal/brain/brain.go:913-1012`), `commitTurn`'s confirmation-gate settlement
(`internal/brain/brain.go:830-856`), `delegate.go`'s `commitDelegatedTurn`
(`internal/brain/delegate.go:722-950`), and `cutExhaustedRun`'s delegation-bound refusal
(`internal/brain/delegate.go:1073-1191`) — one shared helper, one shared gate, called from
all five (§3 decision 4). Also in scope: everything `internal/executor/harvest.go`'s
`settleHarvest` needs to tell an idle-triggered harvest apart from today's
grading-triggered one at settlement time, without a new queue kind or a schema migration.

**Deliberately not in scope, though they fold a session idle too**: the two settlements
that *end* an outcome cycle — `settleVerdict`'s satisfied/failed arm
(`internal/brain/grader.go:357-358`) and `settleGraderError`'s
(`internal/brain/grader.go:432-433`). Each of them ends a cycle that *opened* with a
harvest of the same tree, and the only thing that ran in between is one grader model call
with no tools and no sandbox: the registry snapshot is already the current one at that
point, so a second harvest would re-provision a sandbox to publish byte-identical content
under fresh `file_` ids, churning every id a client had just been handed. A
`needs_revision` verdict loses nothing either — it returns the session to `running`, and
the next agent cycle's own ending harvests through site 1.

**Also out of scope, and a deliberate scope boundary — the two idle folds the *API* makes,
tracked by #586.** A `user.interrupt` (`internal/api/events.go`) and the archive of a
session's last running thread (`internal/api/threads.go`) both fold a session to idle
without any brain settlement running, so the five-site trigger above does not cover them:
a session interrupted or archived into idle harvests nothing under this plan. They are a
separate subsystem (the request-handling API, not the brain's settlement loop), and the
interrupt case needs its own publish-vs-discard decision — an interrupt is precisely the
signal that already discards a grading harvest mid-flight (decision 2's fence), so
whether it should *publish* deliverables at a plain idle is a genuine question, not an
oversight. #586 owns both; this plan's claim is scoped to **brain settlement folds**, not
to "every idle fold."

**Out of scope**: throttling or coalescing repeated idle cycles beyond the existing per-session
dedup key; any change to `collectOutputs`, the listing script, the caps, path validation,
or blob staging (untouched — the executor-side work is identical regardless of why it was
scheduled); a `self_hosted` file lane (still absent, §3.5); real-endpoint recording of the
reference's exact timing, ordering, or re-harvest semantics (tracked under #78, same as
today's outcome-harvest entry).

## 2. Ground truth (pinned 2026-09-03)

**What the docs pin.** platform.claude.com/docs/en/managed-agents/files, "Listing and
downloading session files": deliverables appear "shortly after the agent finishes writing
them, sometimes a few seconds after the session goes idle" — no outcome requirement
(quoted in issue #263's 2026-09-03 comment; this settles the trigger question the
2026-08-04 triage comment left open). "Shortly"/"a few seconds" is best-effort language,
not a promised latency bound.

**What anthropic-sdk-go pins.** `BetaFileMetadata` (`anthropic-sdk-go/betafile.go:163-203`):
`CreatedAt`, `Filename`, `Downloadable` (`api:"required"` on the first two) — the same
shape the existing outcome-triggered harvest already renders through
(`internal/executor/harvest.go:287-289`'s `INSERT INTO files`); nothing about this design
changes that row shape.

**What no source pins — each becomes an INFERRED registry entry (§5's slice 1 records
these):**
- Whether every idle counts, or only a tool-less `end_turn` idle. This design reads the
  docs' "after the session goes idle" language literally, for every *brain settlement*
  fold: whatever stop reason it carries — decision 4 traces the five that produce one
  outside an outcome cycle (`settleEndTurn`'s non-grading branch, `settle`'s
  retries-exhausted branch, `commitTurn`'s confirmation-gate settlement,
  `commitDelegatedTurn`'s idle/gated branches, and `cutExhaustedRun`'s delegation-bound
  refusal). Two classes of idle fold are deliberately *not* covered, both named in §1:
  the two that end an outcome cycle (`settleVerdict`, `settleGraderError`), because that
  cycle already harvested the same tree with nothing sandbox-touching run since; and the
  two the API makes (`user.interrupt`, last-running-thread archive), which are a separate
  subsystem tracked by #586. So the honest scope is "every brain settlement fold," not
  "every session-level idle fold, whatever produced it."
- Whether a session that idles repeatedly (no outcome, several turns) re-harvests on every
  idle, and with what semantics. This design reuses `settleHarvest`'s existing
  delete-all/insert-all-per-snapshot replace (harvest.go:266-293) unchanged — the same
  semantics a multi-cycle grading session already gets.
- Whether a multi-agent (child-thread) session harvests only at true session-level
  quiescence, or on any thread's own idle. This design gates on session-level quiescence
  (§3 decision 6), mirroring plan 21 decision 15's existing rule for the grading trigger.
- Whether `self_hosted` sessions harvest under this trigger. No — same reasoning as the
  existing grading exclusion (`docs/DIVERGENCES.md:109`): the reference worker has no file
  lane, and the platform cannot reach a BYOC sandbox regardless of why a harvest was
  scheduled.
- Whether a session that idles through `brain.go`'s retries-exhausted `settle`, its
  confirmation-gate settlement in `commitTurn` (a permission ask with no delegation call),
  `delegate.go`'s `commitDelegatedTurn` (a coordinator park, a capped delegation chain,
  a child that ends without a report, or a delegated turn's own permission ask), or
  `cutExhaustedRun`'s delegation-bound refusal harvests.
  Yes: every one of them performs its own `events.TransitionThread(...Status:
  domain.SessionIdle...)` and folds the session-level status column to idle exactly like
  `settleEndTurn`'s non-grading branch does (`settle`, `internal/brain/brain.go:977-978`;
  the confirmation gate, `internal/brain/brain.go:850-853`; `commitDelegatedTurn`'s
  switch, `internal/brain/delegate.go:856-873`, reconciled through `sessionStatusNow`,
  delegate.go:875-881; `cutExhaustedRun`'s refusal, `internal/brain/delegate.go:1158-1160`,
  reconciled at delegate.go:1166-1173) — the docs' "after the session goes idle" language
  never distinguishes stop reasons, and this design now reads it that literally for every
  brain settlement path that can fold one outside an outcome cycle, not only
  `settleEndTurn`'s. `envKind` is already
  loaded once per turn in `claimLiveSession` (`internal/brain/brain.go:480-525, 198`) and
  decision 4 threads it the short remaining distance to the sites that lack it. A session whose deliverables were written by an earlier turn and
  which later idles through one of these paths — a failed retry, a parked coordinator, a
  child that stops without reporting, a delegated turn stopped on a permission ask — now
  harvests them under this plan, closing the narrower variant of the symptom issue #263
  reports that the single-trigger design left open.

## 3. Design decisions

1. **Reuse the existing `queue.OutputsHarvest` kind; carry the enqueue reason on the item
   via the existing `metadata` jsonb column, not a new `queue.Kind`.**
   `Item` already carries one kind-specific fact this way: `Chain int`
   (`internal/queue/queue.go:118`, "meaningless... on every kind but model_turn") is read
   back from `work_items.metadata->'settlement_chain'` in `Claim`'s SELECT
   (queue.go:287-288), the same jsonb column `RequeueSettlement` writes with `jsonb_set`
   (queue.go:564-566). `work_items.metadata` is already
   `jsonb NOT NULL DEFAULT '{}'` (`internal/store/migrations/0001_init.sql:126`) — no
   migration. A new `queue.Kind` was considered and rejected: it would need a new
   `work_items_kind_check` migration (the pattern each of `0015_web_exec_kind.sql`,
   `0017_outputs_harvest.sql`, `0023_mcp_catalogs.sql` already followed) plus a new
   dispatch entry in the executor's kind rotation, for work that is byte-identical at the
   exec/stage layer and differs only in one settlement-time branch — more schema and
   surface than the distinction needs.
   One cost of reusing the kind, recorded rather than closed: during a *rolling deploy* an
   old executor still running alongside a new one can claim a new idle-flavored
   `outputs_harvest` item and drain it unread — its `sessionForRun` predates decision 5's
   idle carve-out, so it rejects the now-idle session and completes the item without
   walking. It is self-healing (the session's next idle enqueues another, which a new
   executor serves) and cannot strand a grading cycle (an old executor's grading claims
   still find `running`), so it is a best-effort miss for one deploy window, not a
   correctness loss. A new `queue.Kind` would have avoided it (an old executor would not
   recognize the kind) but at the schema and dispatch cost above; the trade is deliberate.
   Tracked #587 (post-v1).

2. **The item's flag (`ChainGrading bool`) discriminates the two flavors, but `settleHarvest`
   checks the session's *live* `outcome_evaluations` state first, and consults the flag
   only when nothing is currently evaluating.**
   Why a flag is unavoidable: once an outcome is no longer `evaluating`,
   `events.ActiveOutcome` (`internal/events/outcomes.go:227`) returns `ok=false` for two
   different histories that need opposite settlement outcomes — a grading harvest whose
   outcome was flipped to the terminal `interrupted` result by a concurrent
   `user.interrupt` (must discard, today's exact behavior) and a plain idle harvest that
   never had an outcome at all (must publish). `outcome_evaluations` alone cannot tell
   these apart, so a per-item origin marker is required.
   Why live-state-first, not flag-only: a race is otherwise possible. An idle-tagged item
   (`ChainGrading=false`) can still be unclaimed when a fresh `user.define_outcome`
   reaches its own grading cycle on the same session; `settleEndTurn`'s grading branch's
   own enqueue (`b.queue.Enqueue(..., queue.OutputsHarvest)`, grader.go:949) shares the
   *same* `(session_id, thread_id, kind)` dedup key (queue.go:215-217's
   `ON CONFLICT ... DO NOTHING`) as the still-live idle-tagged item, so that enqueue call
   is silently swallowed — its `created` return value is never checked (grader.go:949-951)
   — and the fresh outcome would have no live item left to ever chain its grading turn,
   stranding it in `evaluating` permanently. Checking `outcome_evaluations` fresh, under
   the row lock, before consulting the flag closes this: if the outcome is `evaluating` by
   the time the item settles, it publishes and chains the grading turn regardless of which
   flavor enqueued it — the stale idle-tagged item transparently becomes that cycle's
   harvest. If it settles first (still not evaluating), it takes the idle-publish path and
   completes, freeing the dedup slot for the grading branch's own enqueue to succeed
   normally. This reconciliation is safe because the enqueue that flips the outcome to
   `evaluating` and `settleEndTurn`'s `opts.Then` (where the grading branch's own enqueue
   runs) commit in the same transaction (`AppendInTx`, grader.go:1005) — no claimant can
   observe one without the other.
   `settleHarvest`'s branch:
   - `ActiveOutcome` is `ok` and `Evaluating`, and this pass walked a sandbox (`replace`),
     regardless of `item.ChainGrading` → publish, chain `queue.ModelTurn` (today's exact
     behavior, byte-identical).
   - `Evaluating`, walked nothing (`replace == false`), and the item is *idle*-tagged
     (`item.ChainGrading == false`) → **requeue as a grading harvest** (`RequeueAsGrading`,
     flipping `chain_grading` to true), rather than chain the grader against a stale or empty
     snapshot; the requeued item's next claim provisions a sandbox, walks a current tree,
     publishes it, and chains the grader then (added in review — before the idle trigger a
     grading harvest always provisioned, so this arm could not arise). The `!ChainGrading`
     guard is load-bearing: a *grading*-tagged item reaching here with `replace == false` is
     the blob-less deploy, not the reclaim race —
   - `Evaluating`, `replace == false`, `item.ChainGrading == true` (a storage-less deploy,
     whose `processHarvest` short-circuits to `settleHarvest(nil, false)`) → chain the grading
     turn transcript-only, publishing nothing (plan 21's `TestHarvestWithoutBlobStoreStillChainsGrading`,
     unchanged).
   - Not evaluating, `item.ChainGrading == true` → discard: `queue.Complete` + commit, no
     publish (today's exact `TestHarvestDiscardsWhenCycleSettled` behavior, unchanged).
   - Not evaluating, `item.ChainGrading == false` → publish, do **not** chain a
     `ModelTurn` (new).
   No-blob-store interaction, traced: `processHarvest`'s `e.blobs == nil` branch
   (harvest.go:98-107) calls `settleHarvest(ctx, item, nil, false)` unconditionally —
   `files` is always `nil` and `replace` is always `false`, regardless of which flavor
   enqueued the item. With `replace=false` the delete-all/insert-all block never runs, so
   an idle-flavored item degrades to the same no-op-on-`files` outcome as today's
   grading-flavored one: the fence still runs (nothing is evaluating for a plain idle
   session, so the not-evaluating/`ChainGrading==false` arm fires), it completes the item
   without publishing anything (there is nothing staged to publish) and without chaining a
   turn — safe, but pinned by no test today the way the grading flavor is
   (`TestHarvestWithoutBlobStoreStillChainsGrading`); §5 adds one.
   Rolling-deploy safety, both directions: an old-code item enqueued before this ships
   carries no `chain_grading` key at all, and the new `Claim` SELECT's
   `COALESCE(..., false)` (§5) reads that as `ChainGrading=false`; a new-code *idle* item
   claimed by an old executor still running mid-rollout is drained unread — that executor's
   `sessionForRun` predates decision 5's idle carve-out and rejects the now-idle session —
   a best-effort miss that self-heals on the session's next idle and strands no grading
   cycle (decision 1, #587), not a correctness loss. Either
   direction is safe for the same reason the race above is: the live-state check runs
   first, so a genuinely-`evaluating` outcome still takes the publish-and-chain branch
   regardless of the flag — an old grading item that lands on a new reader mid-rollout
   chains exactly as before; the flag only matters once nothing is evaluating, and a
   plain idle session was never evaluating to begin with.
   One behavior *does* change in the window, benignly: an old-code grading item enqueued
   before this ships (no `chain_grading` key → read as `false`) that is then reclaimed by a
   new executor *after* a `user.interrupt` has settled its cycle. Old code discarded it
   (not evaluating → discard, unconditionally); new code reads the missing key as an idle
   harvest and *publishes* the snapshot instead. Benign because the interrupt already
   settled the outcome to a terminal result, so nothing is grading it and the published
   rows are simply the deliverables as they stood — the same rows a fresh idle harvest
   would have produced on the session's next fold. No grading turn is chained either way
   (nothing is evaluating), so the outcome cannot be stranded. The window is one deploy
   long and needs the interrupt to land in it; the outcome is a harmless extra publish, not
   a correctness loss.

3. **No new pre-check for a concurrent interrupt landing on the idle-flavored branch;
   `queue.Complete`'s existing lease-match check is a sufficient fence, by the same
   argument that already justifies today's outcome-evaluating re-check.**
   Traced, not assumed: `settleHarvest`'s `SELECT ... FROM sessions WHERE id = $1
   FOR UPDATE` (harvest.go:244-246) and the interrupt handler's own session-row lock
   (`internal/api/events.go:80`, `FOR UPDATE OF s`, acquired before the loop that builds
   its `cancels` closures) fully serialize the two transactions on the same row — a
   concurrent interrupt is either already committed (visible to `settleHarvest`'s reads)
   or blocked until `settleHarvest` commits or rolls back, never interleaved mid-transaction.
   `queue.CancelSession` (queue.go:623-633, called from events.go:438) only runs inside
   `api/events.go`'s `settling` branch (events.go:371: `status == Running || len(abandoned)
   > 0`) — an interrupt on a session that is *already* idle with nothing outstanding never
   calls it at all, so a freshly-idled, harvest-pending session is untouched by an
   interrupt that finds nothing to stop. The one case where an idle-tagged item *can* be
   cancelled — a fresh message restarts the session (status back to `running`, no outcome)
   and that new turn is itself interrupted, `settling=true` this time — is exactly the case
   `queue.Complete`'s existing `state='active' AND lease_expires_at=$lease` check
   (queue.go:673-684) already handles: `CancelSession` sets every live item's state to
   `stopped` unconditionally, so `Complete` on a cancelled item fails with `ErrLeaseLost`
   (mirroring `TestHarvestLostLeaseAbortsPublish`'s existing coverage of this exact
   mechanism), and a `stopped` item was never eligible for `Claim` to begin with if the
   cancellation landed before the executor claimed it. No new `Assert` call, no re-read of
   session status, is needed beyond what `Complete` already does.

4. **The trigger fires from five brain settlement paths — `settleEndTurn`'s existing
   non-grading branch, `settle`'s retries-exhausted branch, `commitTurn`'s
   confirmation-gate settlement, `commitDelegatedTurn`, and `cutExhaustedRun`'s
   delegation-bound refusal — sharing one small helper and gated on each site's own
   already-computed session-level fold result, never a `PreviewTransition` call.**

   `enqueueIdleHarvest`, one new method next to `settleEndTurn`
   (`internal/brain/grader.go`, immediately after it), is the one place the gate lives:
   ```go
   func (b *Brain) enqueueIdleHarvest(ctx context.Context, tx pgx.Tx, item *queue.Item,
       sid domain.ID, moved *domain.SessionStatus, envKind string) error {
       if moved == nil || *moved != domain.SessionIdle || envKind != string(domain.EnvCloud) {
           return nil
       }
       _, err := b.queue.EnqueueOutputsHarvest(ctx, tx, item.EnvironmentID, sid, false)
       return err
   }
   ```
   All five call sites already compute a `moved`-shaped value as part of the fold they
   were going to do anyway; none of them needs a `PreviewTransition` (the grading branch
   needs its own, grader.go:895-897, because it must decide *before* performing a
   different set of writes — none of these five has that ordering constraint, so the real
   value is free). These five are the settlements the *brain* makes; the two idle folds
   the API makes — `user.interrupt` and last-running-thread archive — are a separate
   subsystem, out of scope here and tracked by #586 (§1).

   **Site 1 — `settleEndTurn`'s non-grading branch** (`internal/brain/grader.go:964-1003`,
   unchanged in shape from the single-trigger design). It already performs the real
   `events.TransitionThread(...)` for every ending that isn't grading and gets back
   `moved *domain.SessionStatus` — non-nil only when the session-level fold actually
   changed (`internal/events/status.go:134-142`), falling back to `wokeTo` when the idle
   transition itself reports no change (grader.go:987-991). This already accounts for
   `events.WakeOnThreadEnded` (grader.go:974): a woken coordinator's ending folds the
   session to `running`, not `idle`, so `woke` and `moved == SessionIdle` are structurally
   exclusive — no separate guard against firing on a woken ending is needed, and a
   regression test pins the invariant (§5). `envKind` is already read once at the top of
   `settleEndTurn` (grader.go:844-851) and reused by the grading branch's own
   `envKind == string(domain.EnvCloud)` check (grader.go:948) — no new query. Insertion, in
   the `!woke` arm of `opts.Then` (grader.go:993-1002), after `queue.Complete(item)`:
   ```go
   opts.Then = func(ctx context.Context, tx pgx.Tx) error {
       if err := b.queue.Complete(ctx, tx, item); err != nil {
           return err
       }
       if err := b.enqueueIdleHarvest(ctx, tx, item, sid, moved, envKind); err != nil {
           return err
       }
       if !woke {
           return nil
       }
       _, err := b.queue.EnqueueThread(ctx, tx, item.EnvironmentID, sid, "", queue.ModelTurn)
       return err
   }
   ```
   The grading branch's own call site changes exactly as the single-trigger design already
   specified: `b.queue.Enqueue(ctx, tx, item.EnvironmentID, sid, queue.OutputsHarvest)`
   (grader.go:949) becomes `b.queue.EnqueueOutputsHarvest(ctx, tx, item.EnvironmentID, sid,
   true)` — same kind, same dedup key, now explicit about intent. `EnqueueOutputsHarvest`
   is one small additive method on `Queue` (mirrors `Enqueue`, stamping `metadata:
   {"chain_grading": bool}`) — not a signature change to the widely-used `EnqueueThread`
   (queue.go:200-232, over a dozen existing call sites), which stays untouched.

   **Site 2 — `settle`'s retries-exhausted branch** (`internal/brain/brain.go:913-1012`,
   the `else` arm entered once nothing chained, lines 945-1000). `settle` already computes
   its own `moved` the identical way (brain.go:977-987:
   `events.TransitionThread(ctx, tx, sid, events.ThreadTransition{ThreadID: item.ThreadID,
   Status: domain.SessionIdle, Stop: idleStop})`, falling back to `wokeTo` on the same
   no-net-change rule). `envKind` is not currently in `settle`'s scope. Traced: `settle` is
   called only from `commitFailure` (brain.go:1077), called only from `failTurn`
   (brain.go:1056), called only from `runTurn`, at eight call sites
   (brain.go:231, 241, 278, 367, 372, 379, 431, 436) — every one of them already after
   `claimLiveSession` returns `envKind` at brain.go:198. Threaded as a plain parameter the
   short distance it's missing, reusing the value `runTurn` already holds rather than
   re-querying it (the same reasoning that keeps site 1 query-free): `envKind string` added
   to `failTurn`'s, `commitFailure`'s, and `settle`'s signatures; each of the eight
   `runTurn` call sites passes the `envKind` local it already has; `commitFailure`'s one
   call to `settle` (brain.go:1087) passes its own parameter through. Insertion, in
   `settle`'s `opts.Then` (brain.go:990-999), after `queue.Complete(item)`, identical shape
   to site 1:
   ```go
   opts.Then = func(ctx context.Context, tx pgx.Tx) error {
       if err := b.queue.Complete(ctx, tx, item); err != nil {
           return err
       }
       if err := b.enqueueIdleHarvest(ctx, tx, item, sid, moved, envKind); err != nil {
           return err
       }
       if !wakeParent {
           return nil
       }
       _, err := b.queue.EnqueueThread(ctx, tx, item.EnvironmentID, sid, "", queue.ModelTurn)
       return err
   }
   ```

   **Site 3 — `commitDelegatedTurn`** (`internal/brain/delegate.go:722-950`). Unlike the
   other two, its `moved`-analog is not one `TransitionThread` return: the function can
   move several threads in one commit (a spawn, a wake, this thread's own idle), so it
   already reconciles the net with its own before/after read
   (delegate.go:875-881: `after, err := sessionStatusNow(ctx, tx, sid); ...; if after !=
   before { opts.SetStatus = &after }`) — the exact `moved` contract (non-nil only when the
   column actually changed), just computed by comparison instead of returned by
   `TransitionThread` directly. That reconciliation already covers *both* switch cases the
   fold lives in (delegate.go:856-873) without needing to distinguish which one fired:
   `case gated:` (a permission ask on a turn that also carries a delegation call,
   delegate.go:857-864, `Stop: domain.StopRequiresAction`) and `case idle:` (a park, a
   capped delegation chain, or a child that ended without reporting, delegate.go:865-872,
   `Stop: domain.StopEndTurn`). The docs' "after the session goes idle" language never
   distinguishes stop reasons, so this gate doesn't either — a chained turn or a fold that
   leaves a still-running sibling both leave `opts.SetStatus` nil (the reconciliation's own
   contract), so the shared gate excludes them with no extra check: the identical mechanism
   decision 6 already relies on for a child idling under a still-running coordinator now
   also covers a coordinator parking under still-running children
   (`TestCoordinatorSpawnsThreeAgentsAndParksInOneCommit`, delegate_test.go:227-317, keeps
   asserting the session stays `running` — unaffected, and the negative case §5's new test
   mirrors). `envKind` is not currently in `commitDelegatedTurn`'s scope either: its one
   caller, `commitTurn` (brain.go:769, the delegation branch at brain.go:827-828), doesn't
   have it; its own caller, `settleTurn` (brain.go:760, called from `runTurn` at
   brain.go:464, already after `envKind` is loaded), doesn't either. Threaded the same way
   as site 2: `envKind string` added to `settleTurn`'s, `commitTurn`'s, and
   `commitDelegatedTurn`'s signatures; `runTurn`'s one `settleTurn` call (brain.go:464) and
   `settleTurn`'s one `commitTurn` call (brain.go:761) each pass it through;
   `commitTurn`'s one `commitDelegatedTurn` call (brain.go:827-828) passes it on.
   Insertion, in `commitDelegatedTurn`'s single `Then` closure (delegate.go:882-947), right
   after the `delegation_turns` counter update (delegate.go:909-914) and before the
   `chain`/`else` branch:
   ```go
   opts.Then = func(ctx context.Context, tx pgx.Tx) error {
       if _, err := tx.Exec(ctx,
           `UPDATE sessions SET delegation_turns = delegation_turns + 1 WHERE id = $1`,
           sid.String()); err != nil {
           return err
       }
       if err := b.enqueueIdleHarvest(ctx, tx, item, sid, opts.SetStatus, envKind); err != nil {
           return err
       }
       if chain {
           ...
   ```

   **Site 4 — `commitTurn`'s confirmation-gate settlement** (`internal/brain/brain.go:830-856`,
   the branch for a turn with a permission ask and *no* delegation call). It already
   performs the real fold — `events.TransitionThread(ctx, tx, sid,
   events.ThreadTransition{ThreadID: item.ThreadID, Status: domain.SessionIdle, Stop:
   &domain.StopReason{Type: domain.StopRequiresAction, EventIDs: askIDs}})` at
   brain.go:850-852 — inside the closure `commitUnderLock` runs, and assigns `moved` to
   `opts.SetStatus` (brain.go:853) before `AppendInTx` calls `opts.Then`, so the `Then`
   closure it sets first (brain.go:846-848) reads the reconciled value through the same
   `opts` variable, exactly as site 3 does. Structurally it is `commitDelegatedTurn`'s
   `gated` case without the delegation call — the same `requires_action` idle — and
   excluding one while including the other would make the gate distinguish stop reasons
   after all. `envKind` reaches it for free through the threading site 3 already needs
   (`settleTurn` → `commitTurn`). Insertion, in that `Then`, after `queue.Complete(item)`:
   ```go
   opts.Then = func(ctx context.Context, tx pgx.Tx) error {
       if err := b.queue.Complete(ctx, tx, item); err != nil {
           return err
       }
       return b.enqueueIdleHarvest(ctx, tx, item, sid, opts.SetStatus, envKind)
   }
   ```
   A session that harvests at a permission ask and again when the resumed turn ends gets
   the second snapshot through the same replace path every re-harvest uses (decision 6);
   the two live-item structural exclusion holds because the ask-time item has settled long
   before the resumed turn can idle (the resume needs the human first).

   **Site 5 — `cutExhaustedRun`'s delegation-bound refusal**
   (`internal/brain/delegate.go:1073-1191`). This one refuses a turn's *claim* rather than
   settling a turn (#447): when a session has spent its delegation budget, it declines to
   start the turn, folding the thread to idle `end_turn` and, single-agent, the session
   with it. It is a settlement fold in every way that matters here — it performs the real
   `events.TransitionThread(...Status: domain.SessionIdle, Stop: StopEndTurn)`
   (delegate.go:1158-1160) and reconciles `opts.SetStatus` from its own before/after read
   (delegate.go:1166-1173), the identical `moved` contract site 3 uses — and its `Then`
   (delegate.go:1174-1189) already completes the item and wakes a parent, so the harvest
   enqueue slots in beside them exactly as elsewhere. It was missed in the first cut of
   this design (it is a *claim* refusal, not one of the turn-settlement functions), and a
   session whose exhausted run wrote deliverables would have left them unharvested; the
   review caught it. `envKind` is already in scope at its one call site in `runTurn`
   (brain.go:257, after `claimLiveSession`), so it is threaded as one more parameter, no
   new query. Insertion, in that `Then`, after `queue.Complete(item)` and before the
   parent wake:
   ```go
   opts.Then = func(ctx context.Context, tx pgx.Tx) error {
       if err := b.queue.Complete(ctx, tx, item); err != nil {
           return err
       }
       if err := b.enqueueIdleHarvest(ctx, tx, item, sid, opts.SetStatus, envKind); err != nil {
           return err
       }
       if wokeParent {
           ...
   ```

5. **The `sessionForRun` gate every executor kind shares must be widened by one
   kind-scoped carve-out, or the feature is silently inert.**
   `processHarvest` calls `sessionForRun` first (harvest.go:93), and `sessionForRun`
   (`internal/executor/executor.go:904-932`) hard-requires
   `status == domain.SessionRunning` (executor.go:927): any other status completes the item
   and returns `live=false` before `collectOutputs`/`settleHarvest` ever run. This is
   correct for every existing caller — `tool_exec`, `web_exec`, `mcp_exec`, and today's
   sole harvest caller, which keeps the session at `running` for its whole grading span
   (grader.go's own comment, "the session stays running"). But an idle-triggered harvest
   is claimed *because* the session just went idle: unmodified, `sessionForRun` would
   reject every one of them before ever reading the sandbox. This gap was found by tracing
   `processHarvest`'s first call, not by any existing test or doc. Fix, scoped to the one
   kind that needs it:
   ```go
   live := status == string(domain.SessionRunning) ||
       (item.Kind == queue.OutputsHarvest && status == string(domain.SessionIdle))
   if !live || archivedAt != nil {
       // unchanged: Complete + commit
   }
   ```
   `archivedAt != nil` still bars an archived session for either flavor. This adds a
   disjunct — every existing caller's behavior (including today's grading-triggered
   `OutputsHarvest` claims, which always find `status == running`) is unchanged — and one
   more carve-out for the same kind (added in review): an idle attach-only harvest
   (`OutputsHarvest && !ChainGrading`) returns right after the liveness check, skipping the
   environment-config, resolved-agent and resources decodes below, because it reads none of
   them — it attaches to a live sandbox by session id alone (decision 8). Without the skip a
   session whose `resolved_agent` (or config, or resources) will not decode would reclaim-loop
   its idle harvest forever, since `runTurn` already idled that session for the same
   corruption and thereby scheduled the harvest — the sessionForRun twin of the
   `outcome_evaluations` tolerance decision 8 adds in `settleHarvest`. A grading harvest still
   decodes: it provisions a fresh sandbox and needs the full run state.
   Confirmed for all five trigger sites (decision 4): `settleEndTurn`'s fold
   (grader.go:982, `Status: domain.SessionIdle`), `settle`'s (brain.go:978, same), the
   confirmation gate's (brain.go:851, same), both of `commitDelegatedTurn`'s switch
   cases (delegate.go:859 and 867, same), and `cutExhaustedRun`'s (delegate.go:1159, same)
   write the identical `domain.SessionIdle` value to `sessions.status` regardless of stop
   reason — this gate reads only that column, so no further change to it is needed to cover
   any of them.

6. **Re-harvest, child threads, `self_hosted`: no new mechanism — reuse what the grading
   path already has.**
   Re-harvest: `settleHarvest`'s delete-all/insert-all replace (harvest.go:266-293, keyed
   on `files_scope_filename_idx`) is called identically for either flavor when a walk ran
   — a second idle harvest on the same session replaces the whole snapshot exactly as a
   second grading cycle already does (`TestReHarvestReplacesSnapshotPerPath`).
   **One residual exposure this replace carries, recorded rather than closed:** when a walk
   *did* run (grading provision, or an idle harvest that reused a live sandbox) but the
   outputs directory is genuinely empty, `replace=true` with an empty `files` deletes every
   previously-published row. A session that wrote deliverables and then deleted them all
   before idling would see its Files-API rows vanish — the snapshot faithfully reflects an
   empty tree, but a reader may not expect an idle to *remove* deliverables an earlier one
   published. Decision 8's no-provision rule removes the worse case (an idle harvest with
   no live sandbox never walks, so `replace=false` and nothing is deleted); the
   empty-live-tree case keeps today's whole-snapshot semantics deliberately, since a
   partial replace cannot tell a deleted file from one that overran a cap. Tracked with the
   rest of the harvest's unrecorded semantics under #78. Two live `OutputsHarvest`
   items for one session stay structurally impossible: both flavors share the one
   `(session_id, thread_id=NULL, kind)` dedup key. Child threads: decision 4's
   `moved == SessionIdle` gate already excludes a child idling under a still-busy sibling
   (no new check — the same session-level-quiescence rule plan 21 decision 15 established
   for grading, `internal/brain/thread_test.go:57`
   `TestOutcomeGradesAtSessionQuiescenceNotOnAChildsEndTurn`). `self_hosted`: excluded at
   the enqueue site by the identical `envKind == string(domain.EnvCloud)` guard the grading
   branch already uses — no change to `Queue.Claim`, which has never had an
   environment-kind predicate for this kind (the cloud-only gating has always lived at the
   enqueue call site, not in `Claim`).

7. **Crash replay: no new handling — the existing transactional guarantee extends by one
   statement.**
   The enqueue happens inside `opts.Then`, called by the same `AppendInTx` transaction
   `settleEndTurn` already commits atomically (grader.go:1005). A brain crash before commit
   means nothing happened — a fresh claim of the reclaimed `model_turn` item re-runs
   `settleEndTurn` from scratch, idempotently recomputing the same decision. A crash after
   commit leaves the harvest item durably queued, independent of which brain replica
   crashed — the platform's standing "cattle not pets" guarantee, unchanged by this
   design. An executor crash mid-harvest (either flavor) is covered by the existing
   lease-expiry reclaim path (`TestHarvestReadFaultLeavesPreviousSnapshotAndItemForReclaim`)
   — `collectOutputs` and `discardStaged` are untouched by this design.

8. **An idle harvest never provisions a sandbox — it reuses a live one, or no-ops. (Added
   in review.)**
   The first cut of this design routed every harvest through `collectOutputs` →
   `provisionSandbox`, which is the clean-create path (`internal/executor/executor.go`'s
   `Provision`). For a grading harvest that is correct and unchanged: the grader needs a
   current snapshot, so a session whose sandbox was reaped gets a fresh one. But the idle
   trigger fires on *every* cloud idle, and a plain text-only `end_turn` session never ran
   a tool — provisioning one just to walk an empty `/mnt/session/outputs` would make
   ordinary sessions, where the feature is never exercised, each pay a container/pod
   create. That is a behavior change for the common case, and not acceptable.
   `processHarvest` therefore branches on the item's flavor (`ChainGrading`): a grading
   harvest provisions as before; an idle one calls `provider.Attach` (Provision's
   read-only half — it creates nothing, `ErrNotFound` when the endpoint holds no live
   sandbox). A live sandbox is the normal case for a session that actually wrote
   deliverables — it just ran tools and the reaper's idle-TTL is minutes away — so the
   walk and publish proceed. With none, the harvest settles a no-op: `settleHarvest` with
   `replace=false`, publishing nothing and deleting nothing (which also closes decision 6's
   worse empty-walk case). Only `ErrNotFound` means "no sandbox" and settles that no-op; any
   *other* `Attach` error is propagated, not swallowed (corrected in review): it is a
   transient fault — a Docker inspect hiccup, a K8s API timeout — so the lease-recovery
   reclaim retries it, exactly as a grading harvest's provision error is retried. Taking it
   for no-sandbox would silently drop a session's only harvest on a one-off fault. The
   reclaim race (decision 2) publishes a current snapshot while the fresh outcome's own agent
   turn's sandbox is still live when the reclaimed idle item runs; if that sandbox is gone the
   attach-only item walked nothing, so rather than grade a stale snapshot the settlement
   requeues it as a grading harvest, which provisions and walks on its next claim (decision
   2). One residual this leaves, recorded not closed: a newer write racing an in-flight
   harvest — a turn writes v2 and re-idles while a v1 harvest is mid-collection — has its v2
   enqueue swallowed by the dedup key, so v1 publishes and v2 surfaces only on the session's
   next idle; a session that idles exactly once more and never again could miss v2. Tracked
   with the rest of the harvest's unrecorded semantics under #78, beside decision 6's
   empty-live-tree exposure.

   The same "an idle harvest must not wedge" reasoning covers one more path found in
   review: a session whose `outcome_evaluations` will not decode. `runTurn` already idles
   such a session with a visible failure ("session outcome state is corrupt"), which now
   schedules an idle harvest; that harvest reads the same undecodable column in
   `settleHarvest`, and treating the decode failure as a fault would reclaim-loop it
   forever. An idle-tagged harvest never reads the outcome (its publish-or-no-op arm
   ignores it), so it treats an undecodable value as no active outcome and settles. A
   grading-tagged harvest still faults — it cannot grade without the outcome, and a grading
   item is only ever enqueued after `settleEndTurn` decoded the same column successfully,
   so a corrupt one there is a should-not-happen left as today's error.

## 4. Out of scope

- A separate `queue.Kind` and its migration (decision 1) — rejected as unnecessary schema
  growth for a distinction the existing `metadata` column already carries.
- Any change to `queue.ErrLeaseLost` handling — the new branches surface a lost lease
  exactly as every existing `settleHarvest` branch does today; no new asymmetric
  error-handling policy is introduced.
- A real-sandbox (`executor_integration_test.go`) test for the idle trigger:
  `collectOutputs`/`processHarvest` don't branch on why a harvest was scheduled, so the
  existing `TestHarvestRealSandbox` coverage already exercises the shared sandbox walk;
  only `settleHarvest`'s new branch is new surface, and it's covered by the fakes-based
  suite (§5).

## 5. Slices

One slice; the whole change is six small, coordinated production edits (`queue.go`,
`grader.go`, `brain.go`, `delegate.go`, `harvest.go`, `executor.go`) with no schema
migration, so splitting it across PRs would only separate pieces that don't work
independently — a `queue.go`-only PR adds an unused method, and a `grader.go`-only PR
enqueues items `sessionForRun` would silently drain.

**Slice 1 — idle-triggered harvest, TDD-first.**

- `internal/queue/queue.go`: add `Item.ChainGrading bool` (doc comment parallel to
  `Chain`'s — "meaningless for every kind but outputs_harvest"); extend `Claim`'s SELECT
  with `COALESCE((w.metadata->>'chain_grading')::bool, false)`; add
  `EnqueueOutputsHarvest(ctx, db, envID, sessionID, chainGrading bool) (bool, error)`,
  stamping `metadata: jsonb_build_object('chain_grading', $bool)` through the same
  `(session_id, thread_id, kind)` idempotency key `Enqueue` already uses.
- `internal/brain/grader.go`: adds the shared `enqueueIdleHarvest` helper (decision 4);
  `settleEndTurn`'s grading branch calls `EnqueueOutputsHarvest(..., true)` in place of
  today's `Enqueue(..., queue.OutputsHarvest)`; its non-grading branch calls the new
  helper (decision 4, site 1).
- `internal/brain/brain.go`: `failTurn`, `commitFailure`, `settle`, `settleTurn`, and
  `commitTurn` each gain an `envKind string` parameter, threaded from `runTurn`'s existing
  `claimLiveSession` read; `settle`'s idle branch calls the shared helper (decision 4,
  site 2), and `commitTurn`'s confirmation-gate branch calls it from its own `Then`
  (decision 4, site 4).
- `internal/brain/delegate.go`: `commitDelegatedTurn` gains the `envKind` parameter and
  calls the shared helper from its `Then` closure, gated on its own reconciled
  `opts.SetStatus` (decision 4, site 3).
- `internal/executor/harvest.go`: `settleHarvest`'s fence becomes the three-way branch in
  decision 2 (live-state-first, tag-second); the trailing `queue.Enqueue(...,
  queue.ModelTurn)` becomes conditional on the "evaluating" branch only.
- `internal/executor/executor.go`: `sessionForRun`'s widened `live` condition (decision 5).
- Tests, written first per CLAUDE.md's TDD convention:
  - `internal/queue`: a claim round-trip test asserting `chain_grading` survives
    `EnqueueOutputsHarvest` → `Claim` for both `true` and `false`, and that the
    idempotency key still suppresses a second enqueue while one is live.
  - `internal/executor/harvest_test.go` (extend `enqueueHarvest` to call
    `EnqueueOutputsHarvest(..., true)`, making every existing grading test's intent
    explicit with no behavior change; add `enqueueIdleHarvest` calling it with `false`):
    - `TestIdleHarvestPublishesSnapshotWithoutChainingATurn` — idle harvest, no outcome
      seeded → files rows + blobs published, `liveOf(t, queue.ModelTurn) == 0`.
    - `TestIdleHarvestIgnoresUnrelatedTerminalOutcome` — idle-tagged item plus an
      unrelated already-terminal outcome entry on the session → still publishes (proves
      the branch reads `item.ChainGrading`, not "any non-evaluating state discards").
    - `TestFreshOutcomeGradingReclaimsAStaleIdleHarvestSlot` — the decision-2 race: enqueue
      an idle-tagged item, leave it unclaimed, drive a fresh outcome through to its own
      `settleEndTurn` grading enqueue on the same session (a no-op against the live dedup
      slot), then settle the one live item → asserts it publishes **and** chains
      `queue.ModelTurn`, proving the live-state-first check reclaims the stale item rather
      than stranding the fresh outcome in `evaluating`.
    - `TestIdleHarvestDeadSessionDrains` — `status='terminated'` before claim: guards
      decision 5's carve-out against being *widened* past idle (a terminated session must
      still drain). It is not the carve-out's presence guard — a terminated session drains
      on the unfixed code too, so this test passes without the disjunct; the guard that
      the disjunct is *there* is `TestIdleHarvestPublishesSnapshotWithoutChainingATurn`
      (an idle session with no carve-out drains its item unread, so nothing publishes).
      Both are mutation-checked per the project's "mutation-test every new guard"
      convention (removing the disjunct fails the second; widening it to any status fails
      this one).
    - `TestHarvestDiscardsWhenCycleSettled` (existing) stays green unchanged, now exercised
      through the `item.ChainGrading == true` branch.
    - `TestIdleHarvestWithoutBlobStoreCompletesSilently` — idle-tagged item, `h.exec` built
      with `nil` blobs (mirroring `TestHarvestWithoutBlobStoreStillChainsGrading`'s setup),
      no outcome seeded → completes with no publish and no `ModelTurn` chained (pins the
      no-blob-store interaction traced in decision 2).
  - `internal/brain/grader_test.go` / `thread_test.go` (mirroring
    `TestOutcomeCloudSettlementChainsHarvest`, grader_test.go:179, and
    `TestOutcomeGradesAtSessionQuiescenceNotOnAChildsEndTurn`, thread_test.go:57):
    - An end_turn with no outcome, cloud env → `EnqueueOutputsHarvest` called with
      `chainGrading=false` (asserted via `liveOf`/a raw metadata read).
    - `self_hosted` envKind → not enqueued.
    - A child's end_turn under a still-running sibling → not enqueued (session not at
      quiescence).
    - A woken ending (a report delivered mid-turn) → not enqueued (pins the "`woke` and
      `moved==SessionIdle` are mutually exclusive" invariant from decision 4 with a real
      test, not just a read of the code).
  - `internal/brain/brain_test.go` (mirroring `TestProviderErrorFailsTurnVisibly`,
    brain_test.go:1507, the existing retries-exhausted idle-fold coverage — cloud env,
    `liveOf(t, ...)` assertions already established there):
    - `TestRetriesExhaustedIdleEnqueuesOutputsHarvest` — same setup, nothing pending past
      the watermark → the session folds idle `retries_exhausted` → asserts
      `liveOf(t, queue.OutputsHarvest) == 1` and a raw `metadata->>'chain_grading'` read of
      `false` (decision 4, site 2).
    - `TestChildRetriesExhaustedUnderRunningSiblingSkipsHarvest` — a child thread exhausts
      its retries while a sibling thread is still running (the session-level quiescence
      rule decision 6 already states, reused here) → session status stays `running` →
      `liveOf(t, queue.OutputsHarvest) == 0` — the negative decision 4's site 2 needs.
    - `TestPermissionAskIdleEnqueuesOutputsHarvest` — an `always_ask` tool intent on a
      cloud session with no outcome (the existing confirmation-gate coverage's setup) →
      the session idles `requires_action` → `liveOf(t, queue.OutputsHarvest) == 1`,
      `chain_grading=false` (decision 4, site 4); a `self_hosted` variant of the same
      setup → `0`.
  - `internal/brain/delegate_test.go` (mirroring `TestSettlementChainIsCutAtTheCap`,
    delegate_test.go:1794, and `TestCoordinatorSpawnsThreeAgentsAndParksInOneCommit`,
    delegate_test.go:227, commitDelegatedTurn's existing coverage):
    - `TestCappedDelegationChainEnqueuesOutputsHarvest` — the settlement-chain-cut setup
      (a roster added, no child actually spawned) → the coordinator, and with it the
      session, folds idle `end_turn` → asserts `liveOf(t, queue.OutputsHarvest) == 1` and
      `chain_grading=false` via a raw metadata read (decision 4, site 3).
    - `TestParkedCoordinatorWithLiveChildrenSkipsHarvest` — the existing
      three-children-spawn-and-park setup, extended with the new assertion in place rather
      than a duplicated harness build: the coordinator's own thread idles but the session
      stays `running` (its three children are live) → `liveOf(t, queue.OutputsHarvest) ==
      0` — the negative decision 4's site 3 needs, and confirms `commitDelegatedTurn`'s
      `gated` case (a permission ask mixed with a delegation call,
      `TestAskGatedDelegatedTurnGatesExecButStillRunsItsChild`, delegate_test.go:2485) is
      unaffected: that test's spawned, still-running child keeps the session at `running`
      too, so the shared gate excludes it the same way, with no dedicated new test needed.
- `changelog.d/` fragment: user-visible behavior change — session outputs now appear
  without defining an outcome.
- `docs/DIVERGENCES.md`:
  - `:243` ("The outputs harvest" entry) — correct the evidence citation, which today
    names only `platform.claude.com managed-agents/define-outcomes`, to also cite
    `managed-agents/files` (the actual source of the "shortly after... a few seconds after
    the session goes idle" language quoted in §2); extend **When** to name all five brain
    settlement folds this design triggers on (decision 4); extend **Fence** to describe the new
    live-state-first branch.
  - `:109` (self_hosted exclusion) — restate as covering all five trigger sites, not
    grading alone.
  - New INFERRED bullets for the five docs-silent items §2 lists ("What no source pins"),
    including the fifth's now-settled reading (decision 4).
- `docs/ARCHITECTURE.md:112-122, 62, 316` — extend the outcome-grading section's harvest
  description to name all five trigger sites.
- `docs/plan/21_outcomes.md:319-322` — its own text already forward-points to "a follow-up
  issue slice 5 files"; add a one-line pointer to this plan file rather than
  editing its archived body further.
- `README.md:17` — the Outcomes bullet currently reads "a text or file rubric is graded
  each work cycle, deliverables are harvested into the Files API" — i.e. it documents
  harvest as an outcome-grading behavior only. Reword so a reader doesn't come away
  believing harvest still requires an outcome, e.g. split the deliverables clause into its
  own bullet: "session outputs are harvested into the Files API when a turn ends and the
  session goes idle" (or fold a parenthetical into the existing line).
- `STATE.md`: the current Active work entry (`#78`) is done — its checklist is fully
  checked — so this PR clears it to `none` and adds plan 38's Active work block in its
  place, per CLAUDE.md's "what is in flight (**none** when nothing is)" rule; it does not
  coexist alongside `#78`.
- `docs/HISTORY.md`: this slice records the acceptance run (§6) as its own section,
  matching plans 21/24/29/30/32/35/36's convention of recording a real-run acceptance
  criterion there rather than only in the PR description.
- `make verify` green (build, vet, fmt-check, test, cover-gate) before dispatching the
  verifier subagent, per CLAUDE.md's definition of done.

## 6. Acceptance (recorded into docs/HISTORY.md by slice 1)

- The eval-driven-agent-development workshop's `create-slides` run, **without**
  `HARVEST_VIA_OUTCOME`, against a local compose stack built from this branch: the agent
  writes `/mnt/session/outputs/build_deck.py` and `output.pptx`, the session idles
  `end_turn`, and `GET /v1/files?scope_id=<session>` lists both rows (`scope: {type:
  "session"}`, `downloadable: true`) within the workshop's existing retry/backoff window —
  matching the reference docs' "shortly after... a few seconds after idle" behavior and
  closing the exact gap issue #263's 2026-09-03 comment reproduced.
- The existing outcome path (`HARVEST_VIA_OUTCOME=1`, or any real `user.define_outcome`
  session): grading still chains a harvest, the harvest still chains the grading turn, and
  an interrupt landing mid-grading-harvest still discards — `TestHarvestDiscardsWhenCycleSettled`,
  `TestOutcomeCloudSettlementChainsHarvest`, and `TestOutcomeInterruptDuringGrading` all
  green, unchanged in intent.
- `make verify` green; verifier subagent dispatched per CLAUDE.md's "Independent
  verification" and its verdict cited in the PR.
