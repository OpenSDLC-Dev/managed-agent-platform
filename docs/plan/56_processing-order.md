---
status: in-progress
issue: 793
---

# Processing order within a commit (#793, #539)

## The rule

**Within one commit, the log is written in processing order, as the reference
lists it.** The reference sorts every event list by `processed_at`: a client
event takes its list position where it is consumed, not where it was received.
This platform keeps seq as the receipt position across commits (the log is
append-only and its cursors are seq), so the rule applies inside the commit that
consumes an event, which is where every difference below was found.

The evidence, from the recordings of 2026-09-02 to 2026-09-24
(managed-agents-wire-recordings), read against the code at `2eb299a9`:

- Every recorded session and thread event list sorts by `processed_at`: every
  array of `sevt_` events in the recordings' JSON, response bodies embedded as
  strings included, runs non-decreasing with its stamps compared as timestamps
  (non-increasing for an `order=desc` read), though a few look unsorted under a
  string compare (`.899Z` against `.899001Z`). Pending events, with no
  `processed_at`, sit at the tail of every list that holds any, and one moves
  once processed:
  in 2026-09-12-console-141/api-fixtures.json, `user.message`
  `sevt_01C9bDZQjtPsLTqxCmq1YvKT` is at idx 21 of
  `observation.session.approval.events` and at idx 23 of
  `session.approval.final-events`, after the new running pair.
- A waking message is consumed at turn start. All 115 recorded `user.message`
  wakes and the one recorded `user.define_outcome` wake (2026-09-02/batch2.json
  `sessT.events.after-outcome`) list the waking event after
  `session.status_running` and the primary's `session.thread_status_running`:
  110 of the message wakes with the pair directly ahead of the message, five with
  session-start `session.error`s between the two running events, and none with
  the message first (each wake counted once by its `sevt_` id). In all 116 the
  turn's `span.model_request_start` follows the waking event 1 µs later. Both
  recorded deployment runs are among the 110 (console-141
  `deployment.run.final-events` reads the pair at idx 0 to 1 and the message at
  2), and the live 2026-09-12-console-followups/streams/ui-00.sse, which never
  echoes a message at receipt, reads the same.
- An answer is consumed on receipt: all 32 recorded wakes by an answer on the
  primary thread list the answer before the pair it causes — 12 by a
  `user.tool_confirmation` (six of them denies), 9 by a `user.tool_result` and 11
  by a `user.custom_tool_result`; three of the 32 carry two answers each, 35 in
  all (for example console-141 `session.approval.final-events` idx 10 to 13).
- An interrupt is consumed after the results it causes (#539):
  `sessT.events.after-outcome` idx 35 to 37 reads `agent.tool_result`,
  `user.interrupt`, `session.thread_status_idle`, and the live
  2026-09-12-child-lifecycle/streams/ui-01.sse agrees.
- A delivered message is consumed when the woken turn starts: all ten recorded
  report wakes, across six of the seven recorded coordinator sessions, list the
  coordinator's running event 43 to 69 ms before `agent.thread_message_received`,
  and on the live streams too (console-followups ui-02.sse, and the worker-wire
  streams 2026-09-19-self-hosted-docker/worker-wire/0042.sse and
  2026-09-19-self-hosted-latest-cli/worker-wire/0034.sse). All twelve recorded
  spawns do the same on the child's own list, which #675 part 3 already matched.

## Decisions (the owner's, 2026-09-25, on #793)

1. **Item 1, fixed.** A wake by `user.message` or `user.define_outcome` writes
   `session.status_running` and the primary's `session.thread_status_running`
   before the waking event, on the list and the live stream alike, at every wake
   site: the POST send path, the interrupt-plus-message redirect, session create
   with `initial_events` (deployment runs included) and the dream stage.
   **#539 lands under the same rule, in the same change:** the synthesized results
   and outcome ends come before `user.interrupt`, and the idle pair after it. A
   redirect batch therefore reads: results, `user.interrupt`, idle pair, running
   pair, `user.message`.
2. **Item 2, kept and registered as deliberate.** A child's resume of an idle
   session keeps this platform's shape: `session.status_running` with the child's
   own running event, the primary left idle. Session `status` stays a fold that
   answers whether any work is happening in the session; the executor's liveness
   check, the MCP work check and the archive/delete guards depend on that. Its
   residual is registered with it: a coordinator woken while a child keeps the
   fold running writes its own running event without a `session.status_running`,
   because the session never left running.
3. **Item 3, fixed for every wake a delivered message causes.** The target's
   running event is written before the `received` row: a child's report, a
   `send_to_agent` that wakes an idle child, and every ending notice that wakes a
   coordinator. The last two are unrecorded; the rule matches this platform's own
   spawn order and the twelve spawns and ten report wakes that were recorded.
4. **Item 4, added to #793: what the model sees for a message posted mid-turn.**
   Today it replays merged ahead of the assistant reply it never saw
   (`TestMidTurnMessageChainsIntoNextTurn`); in processing order it belongs after
   that reply. This is PR-B, below.

Unchanged by all four: rows already on the append-only log keep their old order,
as #675 part 3 accepted, and the POST echo keeps the order the client posted in.

### Corrections to the issue text

- `user.tool_confirmation` does not belong in item 1. On the primary thread the
  reference lists a confirmation, a `user.tool_result` and a
  `user.custom_tool_result` before the pair, as this platform does. A pair before
  a confirmation appears only for a child's confirmation, and that pair is the
  coordinator's: item 2.
- Item 1's evidence list mixed shapes: sessK2's confirmation wake is a child's
  (item 2), sessW's tool-result wake shows the answer first, and the 2026-09-02
  `.stream.raw` captures replay history rather than tail it live.
- Item 1 also covers two wake sites the issue missed: create with
  `initial_events`, whose comment called its placement INFERRED although console-141
  records it, and the dream stage.
- Item 2's triggers were broader than "a child's tool is confirmed": 5
  confirmations (one a deny), 4 `user.tool_result`s and 2
  `user.custom_tool_result`s. In 6 of the 11 resumes the child's running comes
  before the primary's idle; the session idle's `stop_reason` still names the ask
  just answered; and the idle is absent when a sibling was already running.
- Item 3: this platform's report wake never emits `session.status_running`, because
  the reporting child still holds the fold at running. The `session.status_running`
  in 9 of the 10 recorded report wakes is item 2's residual, not item 3's.

## The split

**PR-A** (this plan's first PR) lands items 1 and 3 and #539, registers item 2,
and fixes one older bug on a line it touches:

- `processingOrder` (internal/api/events.go) lays a send out by what each event
  is, not where it was posted. First comes what is consumed on receipt, in
  receipt order: the answers the send's settlement processes, and each interrupt
  behind the results and outcome ends its settling wrote and ahead of the idle
  pairs it caused. A thread two interrupts reach is ended by the first received.
  Then, for each thread the commit woke, its running pair and the input its woken
  turn consumes: the posted message, outcome or system message addressed to it,
  and any notice delivered to it, whichever arm made the wake. Last comes what
  nothing in the commit consumes, in receipt order: a message to a primary
  already running, a notice to a coordinator left parked, and an answer queued
  behind an earlier call of its thread that is still waiting, which the ordered
  tool flow does not pass. The reference also keeps such a result unprocessed at
  the tail until the earlier call's answer arrives
  (2026-09-19-custom-order-followup `setup[80]` and `[83]`). The settlement stamps
  answers only after the append has placed them, so which ones it leaves queued
  is read first, by walking the thread's calls as the settlement will:
  `events.PendingAnswers` and `AdvanceThreadTools` share one walk. The arms run
  in the same order (the primary's own first unless it is interrupted, then the
  interrupted threads in receipt order), so the status events their transitions
  emit read true in the list. The client's events are found again by a
  pre-minted id, so the echo and the `processed_at` re-read no longer assume
  they lead the batch.
- `processed_at` agrees with that order within a commit. A child-scoped interrupt
  is stamped once its arm has synthesized its results. Every commit's stamps are
  raised to agree with its order: `AppendInTx` raises a stamp that would run
  backwards through a batch to the stamp ahead of it, on every append. An
  ordinary turn's `span.model_request_end`, stamped when built and listed after
  the turn's `agent.*` rows, takes their stamp, as do a later interrupt's results
  listed behind an earlier interrupt's idle and the grading start listed behind
  the wake it runs on. No consumer compares stamp values; the readers key on
  null or not null. An answer the settlement processes, stamped after the
  append, is held inside its list slot by one statement over the batch; a queued
  one stays unstamped.
- Every delivery goes through `events.DeliverAndWake` or
  `events.DeliverThreadEnded`, and their wake rules are no longer exported. Their
  `Delivery` result owns the wake-then-row order for the emitters that append it
  whole. For the send, which lays it out with `processingOrder`, it names the
  row's target. A wake that consumes an input rather than a delivery (a create's
  initial events, a dream stage, a spawn's task, the grading wake) writes its pair
  and then its input at its own site, with one transition and one append each.
- `events.AppendTransition` keys on a wake that happened, not on the status the
  last transition asked for. When one of its transitions wakes a thread, its
  events follow every pair; otherwise they precede them. The reclaim's forced
  pair is not a wake and carries no input, so it is unchanged.
- Archiving a session with a live outcome ended the outcome only when the
  primary was parked on a call the archive settles (a confirmation or a custom
  tool), and even then wrote its interrupted `span.outcome_evaluation_end`
  without flipping the `outcome_evaluations` projection, so GET still reported
  the outcome live. The archive now ends every live outcome as an interrupt
  does, the end and the flip together, whatever the primary is parked on: a call,
  a `wait_for_agents` on children the archive terminates, or nothing at all
  (`retries_exhausted`). The bug predates #793.

Placements inside PR-A that are ours rather than recorded: the redirect batch (no
recording carries an interrupt with a following message); a client's own answers
posted beside an interrupt, which interleave with it in receipt order, being
consumed on receipt: an answer precedes the results only of an interrupt
received after it, and an `[interrupt, answer]` send writes the interrupt's
results, the interrupt and its idle before the answer; a notice and a message
that one woken turn both consumes, kept in receipt order; and input nothing in
the commit consumes, placed at the tail. One placement stays out of reach, and is
registered in docs/DIVERGENCES.md (the primary thread's entry). A thread that an
answer resumes moves after the append, once the answer is on the log, so an input
posted beside that answer precedes the resume's running pair instead of following
it.

**PR-B** places a message posted mid-turn after the assistant reply it never saw,
in what the model sees on replay only. It does not move the message's list or
stream position: seq stays the receipt order across commits, and the difference
that leaves is registered rather than chased. PR-B lands after PR-A, and owns the
registry entry on list order and `processed_at` (docs/DIVERGENCES.md, "GET
/v1/sessions/{id}/events — list filters and order keyed on created_at/seq").

## Alternatives rejected

- **A full processing-order log.** Giving each inbound event its log position when
  it is processed — an ordering column or a pending-input queue, listing and
  streaming by it, stamping `processed_at` at turn start — would match the
  reference everywhere, mid-turn messages included. It breaks the invariant that
  seq is at once the receipt order, the list order and the stream cursor, and the
  one that every accepted event is on the log from receipt; it re-keys the SSE
  cursor, list paging, `MarkProcessedThrough`, the chain and watermark checks, and
  needs a migration. Its only gain over PR-A plus PR-B is the list position of a
  mid-turn message.
- **Mimicking item 2.** Pairing a child's resume with the primary and letting the
  session's events follow the primary would idle the session under a working
  child: an executor would drop a confirmed call unrun, and an archive or delete
  would be admitted mid-work, unless every consumer of the column moved to thread
  liveness. It would build on a resource state no recording shows (a GET of the
  session during a child's work), and would reproduce a stale stop reason and a
  sibling-dependent idle that read as artefacts of the reference, not a contract.

## Verification

- Tests first, red on the old code: the list and live stream of a `user.message`
  wake, a `user.define_outcome` wake, an `initial_events` create, a dream stage,
  a redirect in both posted orders with its echo, an outcome chained through an
  interrupt, an interrupt of a turn stuck on a tool call (#539) from a client and
  from the dream runner, and, for item 3, a report wake, a `send_to_agent` wake of
  an idle child, each ending-notice wake (archive, interrupt, retries exhausted,
  chain cap, delegation budget, ended without reporting, grading at quiescence)
  and the two helpers themselves. After review: a notice beside the message that
  wakes its coordinator, two notices whose second ending wakes the coordinator, a
  message that wakes nothing beside an interrupt, an answer beside a wake and
  beside a redirect, the stamps of an interrupt's commit, the grading start after
  the wake it runs on, `AppendTransition` keyed on the wake, and the archive of a
  session parked mid-outcome. After the second review: an answer queued behind an
  earlier call beside a wake, answers behind an allowed and a denied call, two
  interrupts reaching one thread in both posted orders, the archive of a
  coordinator waiting on a gated child and of a session idle on
  `retries_exhausted`, and the clamp of many answers in one statement. Item 2's
  residual, a coordinator woken by a message under a running child, is pinned
  rather than changed, as is the placement out of reach, a message beside an
  answer that resumes the primary; so is the archive of a primary parked on a
  confirmation, which was already right.
- `make verify`, `make registry-check`, independent verification, both reviews and
  the PR's CI before squash merge.
