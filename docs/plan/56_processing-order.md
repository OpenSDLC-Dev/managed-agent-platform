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

- All 228 distinct recorded event lists sort by `processed_at` (6 look unsorted
  only under a string compare, `.899Z` against `.899001Z`). Pending events sit at
  the tail with no `processed_at`, and one moves once processed: in
  2026-09-12-console-141/api-fixtures.json, `user.message`
  `sevt_01C9bDZQjtPsLTqxCmq1YvKT` is at idx 21 of
  `observation.session.approval.events` and at idx 23 of
  `session.approval.final-events`, after the new running pair.
- A waking message is consumed at turn start: 109 `user.message` wakes and the one
  recorded `user.define_outcome` wake (2026-09-02/batch2.json
  `sessT.events.after-outcome`) list `session.status_running`, the primary's
  `session.thread_status_running`, the waking event, then
  `span.model_request_start` 1 µs later. Five more message wakes put a
  `session.error` between the two running events, the message still after both. A
  deployment run's list reads the same (console-141 `deployment.run.final-events`,
  idx 0 to 2), and so does the live 2026-09-12-console-followups/streams/ui-00.sse,
  which never echoes a message at receipt.
- An answer is consumed on receipt: on the primary thread, 12
  `user.tool_confirmation`s, 7 `user.tool_result`s and 9
  `user.custom_tool_result`s are listed before the pair they cause (for example
  console-141 `session.approval.final-events` idx 10 to 13).
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
   check, the MCP work check and the archive/delete guards depend on that.
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

**PR-A** (this plan's first PR) lands items 1 and 3 and #539, and registers
item 2:

- `processingOrder` lays a send out as: the posted events nothing else places, what
  an interrupt settles, the interrupts, what follows them (a told coordinator's
  wake and notice, the idle pairs), a wake's running pair, then the posted events
  from the first waking event on. The client's events are found again by a
  pre-minted id, so the echo and the `processed_at` re-read no longer assume they
  lead the batch. A send the platform writes nothing for keeps its posted order.
- `events.DeliverAndWake` and `events.DeliverThreadEnded` own the order of a
  delivery and its wake, so no emitter picks its own.
- `events.AppendTransition` writes a wake's pair ahead of the input it consumes,
  so the brain's harnesses model the trigger; the reclaim's forced pair is not a
  wake and carries no input, so it is unchanged.

Two placements inside PR-A are ours rather than recorded: the redirect batch (no
recording carries an interrupt with a following message), and a client's own
answers posted beside an interrupt, which stay ahead of the synthesized results
because they are consumed on receipt.

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
  and the two helpers themselves.
- `make verify`, `make registry-check`, independent verification, both reviews and
  the PR's CI before squash merge.
