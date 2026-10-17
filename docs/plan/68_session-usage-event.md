---
status: archived
issue: "#432"
---

# The `session.usage` event and `usage.active_seconds` (plan 68)

#432 tracks the reference's session-budget surface, which this platform
registers and does not build. On 2026-10-17 the owner chose one slice of it:

- the `session.usage` event, written immediately before every
  `session.status_idle`;
- `active_seconds` in that event's `usage` and in the session resource's
  `usage`.

Everything else in #432 stays registered and unbuilt: `list_cost`,
`server_tool_use`, budgets, `budget_reached` and the webhook.

## What the reference does

The public docs (platform.claude.com, `managed-agents/session-observability`,
fetched 2026-10-17) say: "The event is emitted on idle transitions rather than
on a timer: The session emits one immediately before it goes idle, whatever
the stop reason. The session emits one when a thread pauses at a session
budget." They define `active_seconds` as "the cumulative time during which the
session had at least one thread running. Overlapping activity from concurrent
threads is counted once."

The recordings in `managed-agents-wire-recordings` add the shape and the order
(`2026-09-12-*`, `2026-09-19-self-hosted-docker`, `2026-10-15-repo-followups`):

- Each of the 28 `session.status_idle` events read there comes immediately
  after a `session.usage`. The order is `session.thread_status_idle`,
  `session.usage`, `session.status_idle`, one microsecond apart. The stop
  reasons seen are `end_turn`, `requires_action` and `budget_reached`.
- The event's keys are `id`, `processed_at`, `type` and `usage`. `budget` is
  present only when the session has one; it is absent, not null, otherwise
  (`2026-10-15-repo-followups/setup.json` idx 20). There is no
  `session_thread_id`.
- `usage` holds the token buckets, `active_seconds` (a float, to the
  millisecond), `list_cost` and `server_tool_use`. The values are cumulative.
- A child thread's stream carries none. A session that terminates (context
  overflow) writes none. An interrupt sent to an idle session writes neither a
  usage nor an idle.
- In budgeted multiagent sessions one also follows each child thread's idle,
  about 80 ms later, while the session keeps running.

## Decisions

1. **One emission point: `Log.AppendInTx`.** Every event goes through it, under
   the session row lock. It places a `session.usage` immediately before each
   `session.status_idle` in the batch. `events.TransitionThread` was the
   alternative: it builds most idle events, but `terminateThread` builds one
   itself, and the fold does not see the turn's usage.
2. **The snapshot includes the batch's own usage.** It is the stored usage plus
   the append's `AddUsage`, so a turn that ends idle reports its own tokens.
   `AppendInTx` folds `AddUsage` after the insert, so the snapshot cannot read
   the stored value alone.
3. **The payload is `{"usage": {…}}`.** It holds the four token fields and
   `active_seconds`:
   - no `budget` key, because the reference omits it for a session without a
     budget, and no session here has one;
   - no `list_cost` and no `server_tool_use`, because the platform has no price
     table and counts no server tool requests. Both are optional in the SDK's
     snapshot schema;
   - no thread id, so the event is on the session stream and the primary
     thread's view, never on a child's, as recorded.
4. **`active_seconds` is a running sum on the session row.** Two columns hold
   it: `active_seconds` and `running_since` (null while not running).
   `AppendInTx` updates them from the batch's session-level status events, at
   their `processed_at`:
   - `session.status_running` opens an interval;
   - `session.status_idle`, `_rescheduled` and `_terminated` close it;
   - an interval that reads negative, across two hosts' clocks, adds zero.

   No batch this build writes runs a running session again without a close
   first. A running that finds an interval open therefore means a close went
   unseen, written by a replica on the earlier build, and it restarts the
   interval: the count drops that turn's time rather than adding the idle
   after it.

   The session's status is the fold over its threads, running while any thread
   runs, so overlap counts once by construction. Rescheduling does not count:
   the definition says running. Deriving the sum from the log on each read was
   the alternative; it scans the session's whole log per render, and the list
   renders many sessions.
5. **Migration 0055 backfills both columns** from each session's stored
   session-level status events. A session's history keeps its time, and a
   session running at deploy time keeps its open interval. There a running
   inside an open interval keeps it open: the stored log holds every close.
6. **The session resource renders the running time to the millisecond.**
   While the stored status is running, it adds the open interval up to the
   read, so a long turn reports time as it spends it. The read's time is the
   database's clock, the one every replica reads. An idle session ignores an
   interval left open on its row. The reference's GET read 0.1–0.23 s
   above its last event's value (recorded), and its value while running is
   unobserved, so this is an inference.
7. **`domain.Usage` stays the stored token shape.** The session resource
   renders a wider struct that adds `active_seconds`; the thread resource's
   usage is unchanged, since the slice does not cover it.

## Pull requests

One pull request carries the plan, the migration, the event, the render, and
the registry and changelog updates.

## Verification

- `internal/events`: the usage comes immediately before each session-level idle
  and nowhere else; its payload; the running sum across cycles, a reschedule,
  and a reclaim pair.
- `internal/store`: migration 0055's backfill, including an open interval.
- `internal/api`: the session resource's `usage.active_seconds`.
- The tests that assert an exact event sequence around an idle, updated.
- `make verify`, the verifier, and the dual review.
