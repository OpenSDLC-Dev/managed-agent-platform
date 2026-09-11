---
status: archived
issue: "#354"
---

# The delete does not kick the reaper (plan 48)

`DELETE /v1/sessions/{id}` returns `session_deleted` and the session's container
is still `Up`. It stays up until the executor's reaper next ticks — a full
`EXECUTOR_REAP_INTERVAL`, 60 seconds by default (`internal/executor/reaper.go`'s
`reapLoop`, the default resolved at `internal/executor/executor.go:256-258`).
#354 was filed from a workshop cleanup that watched exactly that and reached for
`docker rm -f`.

The latency is deliberate. Plan 24 chose it, and `reaper.go`'s package doc
states it as a property: teardown is "eventual, one reap interval behind the
trigger, which the wire cannot observe". This plan retires the first half of
that sentence and keeps the second.

## What the issue asks, and what the code already answers

The issue's acceptance has three parts. **One of them needs no new code, and
finding that out is what makes this plan small.**

> Still correct under multi-executor: only the owner reaps; kick is at-most-once
> / idempotent with existing `Reap`.

Already true, structurally. `reapSession` classifies, takes a per-session
Postgres advisory try-lock, and re-classifies *under* the lock before acting
(`reaper.go`, plan 24 D4: "a classification is stale the moment it returns").
`Owned()` is what bounds each executor to its own holding, and `Reap` is scoped
to one session's resources on the endpoint it runs against, so an executor asked
to reap a session it does not host lists nothing and does nothing. A kick that
arrives at the wrong executor is therefore inert by construction, not by
agreement — which means the kick does **not** need addressing, ordering, or
delivery guarantees. That is the whole reason a lossy broadcast is admissible
below.

**The other two are latency and blast radius**: teardown within a few seconds on
a healthy deployment, with idle-TTL and orphan behaviour unchanged and without
lowering the sweep interval for everything. Those are what the design has to
earn.

### What plan 24 rejected, and what it never considered

Plan 24's rejected list is specific: "a teardown work kind (FK cascade, above)".
The argument was structural and is still correct —
`work_items.session_id REFERENCES sessions(id) ON DELETE CASCADE`
(`internal/store/migrations/0001_init.sql`), so the delete that creates the need
for teardown destroys the row that would carry it. **A push channel is not in
that list.** Plan 24 rejected one shape on an argument that does not reach this
one, and never evaluated the other. This plan is filling a gap rather than
overturning a considered rejection, and the distinction matters: nothing here
contradicts plan 24's reasoning, only its conclusion about latency.

## What the invariant actually forbids

`docs/ARCHITECTURE.md` says processes never talk to each other directly, and
that the brain and executors communicate "only through the control plane's event
log and work queue". Read with its own purpose clause — "which is what makes
'customer-run worker with zero inbound network access' the same code path as the
platform's own executor" — the sentence is about **inbound network surface**,
and a Postgres `LISTEN` is an outbound connection the executor already holds.
BYOC parity is untouched: the BYOC worker has no database access at all
(`cmd/worker/main.go` opens no pool), and it does not reap — every reap tier is
cloud-only, and plan 24 puts BYOC sandbox lifecycle out of scope on the
reference's own "Managed by you" contract.

The sentence's *enumeration* is nonetheless short a member, and was before this
plan: the work queue already fires a `NOTIFY` on enqueue that the work API's
blocked poll consumes (`internal/queue/queue.go:239-243` →
`internal/api/workapi.go:181`, added for #74). The correction is owed either
way; this plan pays it.

## Decisions

1. **A `NOTIFY` on a new channel, not a work item and not a shorter interval.**
   The work item is structurally impossible (above). A shorter interval is the
   issue's own option 2 and fails its acceptance: the same ticker drives the
   orphan sweep and the idle tier, so buying delete latency with it buys a
   faster sweep for everything, which the acceptance forbids in as many words.
   A separate fast lane for the deleted tier is a second scanner with its own
   starvation and windowing problems, for a wake the database can deliver for
   free.

2. **The producer fires inside the ending transaction, beside the row the wake
   is owed to.** The rule it has to honour: **the kick may never fail the
   DELETE.** A `NOTIFY` inside the transaction honours it, because Postgres
   queues the notification and delivers it only on commit — it cannot reach a
   reaper before the tombstone is visible, it reaches nobody if the ending rolls
   back, and it cannot fail a delete that has committed, since there is no
   moment at which the delete is committed and the notify has not run. That is
   the argument `NotifyWorkEnqueued` already makes in the same file, and this
   producer now shares its contract and its `Execer`.

   Riding the commit is also what keeps the kick off the response path. One
   statement inside a transaction the request was already running, rather than
   post-commit work with a budget of its own — and no window in which the commit
   lands and the process dies before the wake goes out.

   *Evaluated and rejected:* firing after the commit on the detached context
   `deleteSession` already has. The reasoning was that pgx dooms a transaction
   on a failed statement, so an in-transaction `pg_notify` could abort a delete
   that had otherwise succeeded. It confuses two different things — a statement
   that fails *before* commit fails a delete that has not succeeded yet, which
   is ordinary, and is a risk every work enqueue in this codebase already
   takes. What the post-commit version actually cost was a synchronous 5-second
   budget on `DELETE` and on archive, a crash window, and a second constant
   restating the budget beside it. Both external reviewers raised it; the code
   shipped it first and this plan records the correction rather than hiding it.

3. **The wake carries no payload and is allowed to be lost.** The durable
   evidence is the `deleted_sessions` tombstone, written inside the deleting
   transaction and carrying no foreign key precisely so it survives the cascade
   (migration 0018). The tombstone plus a container still visible to `Owned()`
   is a complete, re-readable description of the work owed, so the wake only has
   to say *look again* — never *what to do*. A session id in the payload would
   be a fact the consumer must not trust anyway, since the kick reaches
   executors that do not own the session.

4. **The consumer wakes the existing `reapLoop` through a capacity-1 coalescing
   channel and runs the unchanged `reapPass`.** This is the decision that keeps
   the change small and the invariants intact. A targeted reap would have to
   re-establish by hand everything `reapPass` gets by construction — the
   `Owned()` gate, the gate-token revoke `Reap` performs first, the cloud-only
   predicate, the deleted tier's blob-delete retry trigger, and the memory-sync
   ordering that must stay ahead of every non-deleted tier's teardown. Waking
   the loop instead of calling into it also leaves `reapSession`'s signature and
   every existing test call site untouched, which is the strongest available
   evidence that idle-TTL and orphan behaviour are unchanged.

   Capacity 1 with a non-blocking send is what makes a full pass affordable: the
   pass is edge-triggered, so a bulk delete of five hundred sessions collapses
   into one or two passes rather than five hundred.

5. **The `LISTEN` holds a dedicated connection outside the pool.** The pool
   floor stays at 4. `pgxpool` defaults `MaxConns` to `max(4, NumCPU)` and
   neither `deploy/compose/docker-compose.yml` nor the Helm chart sets
   `pool_max_conns`, so raising the floor to 5 would turn every ≤4-CPU
   deployment's executor from starting into failing to start — a breaking
   upgrade for a latency improvement. The existing floor guards a *nested pool
   acquisition* deadlock (`cmd/executor/main.go`: the work loop's provision, the
   reaper's lock, the memory-sync transaction inside it, and transients); a
   connection that never participates in that nesting does not belong in that
   budget. The cost is one sentence in the `DATABASE_URL` doc block saying the
   executor holds one connection outside the pool.

   The listener dials that connection from **the pool's parsed configuration**,
   never from `DATABASE_URL` itself. Only `pgxpool.ParseConfig` consumes the
   `pool_*` options a DSN may carry; `pgx.ParseConfig` leaves them in the
   startup packet, where the server refuses the connection over a setting it
   has never heard of (`FATAL: unrecognized configuration parameter
   "pool_max_conns"`). Dialling the raw string would therefore break the kick
   outright in exactly the deployments the doc block tells to tune their pool —
   silently, since the listener's only symptom is a warning every backoff and a
   teardown that falls back to the interval. `internal/executor`'s
   `TestReapKickDialsThePoolsConfigNotTheDSN` pins both halves.

6. **Delete and archive; not terminate.** The issue's acceptance names delete;
   its own option 1 names "deleteSession / archive / terminate". Archive reaches
   the same `reapPass` — shipping delete-only would leave an operator who
   archives filing this issue again. It publishes from `archiveSessionInTx`
   rather than from the handler, so every caller that archives a session kicks,
   the dream runner's closing arm included: once the wake rides the transaction
   it costs a caller nothing, and the latency argument that would have excluded
   the runner goes with it. Gated on the archive having actually happened —
   the statement `COALESCE`s, so re-archiving ends nothing, and a wake for it
   would have every listening executor sweep everything it owns for a request
   that changed no row. Terminate is written by the brain's
   settlement: a second producer in a second binary, on a path unlike either
   handler. It earns its own issue rather than a rushed third call site here,
   and that issue is #688.

7. **One `Info` line the first time `LISTEN` establishes, and a `Warn` each time
   it cannot.** Not decoration: an inert kick is indistinguishable from a slow
   one from the outside, and the deployments where it is inert are ones this
   process cannot detect for itself — a transaction-pooling proxy that refuses
   `LISTEN` (`internal/events/broker.go` already says so) and, worse, one that
   accepts the statement while multiplexing away the backend that would deliver
   the notification, where the listener believes it is covering and never hears
   anything. The `Info` line is what an operator checks against that; a counter
   split by wake source would make it observable rather than inspectable, and is
   left for whoever first needs it. The listener never faults `Run`; it
   reconnects with the broker's backoff and the ticker still covers.

8. **The `Owned()` "natural shard" sentence is corrected here rather than left
   beside a kick.** `reaper.go` and `docs/ARCHITECTURE.md` both say each
   executor sees only its own daemon or namespace. That is true of Docker, whose
   `Owned` lists one daemon's containers, and false of Kubernetes, whose `Owned`
   lists every labelled pod in the release namespace
   (`internal/sandbox/k8s/k8s.go:345`) while the chart puts every replica in that
   one namespace. Nothing is broken by it — `Reap` is idempotent and the
   advisory lock serializes the pair that must not interleave — but a broadcast
   kick makes every replica in a namespace run a pass at once, so the paragraph
   a reader checks first must not be wrong. At the chart's default of one
   replica the herd does not arise, and the coalescing channel bounds it to one
   pass per replica per burst if it ever does. No jitter is built for a cost
   nobody has measured.

## What this does not close

- **Terminate** (decision 6) — #688.
- **BYOC sandbox lifecycle**, which the platform reaper has never covered.
- **The cost of a wake.** A kicked pass is one runtime list plus the
  classification queries per owned session — the same pass the ticker already
  runs, now also triggered by events. So the reaper's database and runtime load
  now follows the rate at which sessions end, multiplied on Kubernetes by the
  replicas sharing a namespace, where before it was one pass per interval
  whatever happened. Coalescing bounds a burst, not a sustained rate; the gate
  on re-archiving (decision 6) is what keeps a wake tied to a real ending. No
  minimum gap between kicked sweeps is imposed, because a sweep per ended
  session is work proportional to work, and a debounce would be a tuning knob
  invented ahead of any evidence that the rate hurts. If it does, that is an
  issue with a measurement attached.

## Slices

One PR. The producer, the channel, the listener, the loop's second wake, the
tests and the documentation are a single reviewable change, and splitting the
producer from the consumer would land a `NOTIFY` nobody hears.

## Acceptance

Each rung is a test that fails before the change and passes after, and every
guard gets a mutant that dies by a named test.

1. **A delete tears the sandbox down without the ticker.** `ReapInterval` set to
   an hour, so a reap observed within a few seconds is arithmetically impossible
   from the tick. This is the issue's own acceptance and the reason the interval
   is the test's lever rather than a real-time wait.
2. **Archive too**, on the same lever.
3. **A refused delete emits no kick.** A running session's delete is rejected
   before the commit; nothing is published. Without this rung the producer could
   fire unconditionally and every rung above still passes.
4. **The kick is coalesced, not amplified.** A burst of deletes does not produce
   a pass per delete.
5. **A lost kick still reaps.** With the listener never established, the session
   is still torn down by the ticker — the degradation decision 2 and decision 7
   both promise, asserted rather than assumed.
6. **The listener survives its connection dying** and resumes delivering, the
   pattern `internal/events`' own failure tests already use, and a garbage
   payload does not wedge it.
7. **Shutdown is unchanged.** `Run` still waits for the reaper, and now also for
   the listener, so neither goroutine outlives the pool it uses.
8. **Idle-TTL and orphan behaviour are untouched**, evidenced by `reapSession`,
   `classifyForReap`, `reapPass` and `idleTTL` keeping their signatures and every
   existing call site compiling unchanged.

## Docs

- `changelog.d/` — one fragment.
- `internal/executor/reaper.go`'s package doc and `reapLoop`'s comment: teardown
  is no longer only eventual. The clause that stays is "which the wire cannot
  observe" — still true, and still the reason no `docs/DIVERGENCES.md` entry is
  owed (plan 24's ground truth: no Managed Agents endpoint exposes container
  existence).
- `internal/executor/executor.go`'s `ReapInterval` comment: it is now the worst
  case for teardown, not the usual one.
- `internal/sandbox/sandbox.go`'s `Owned()` comment, per decision 8 — the same
  correction as the reaper's, at the interface the reaper reads.
- `docs/ARCHITECTURE.md`: the sandbox-lifecycle paragraph's "one interval behind
  its trigger"; the topology sentence's enumeration, including the work-queue
  wake it already omitted; and the `Owned()` sentence per decision 8.
- `cmd/executor/main.go`'s `DATABASE_URL` block: the connection held outside the
  pool.
- **Not** `docs/plan/24_sandbox-teardown.md`, which is archived. Its Decision 1
  is superseded here and the supersession is recorded in this plan and the
  changelog fragment, not by retro-editing a closed plan.
- `STATE.md` — this plan is the active work.

## Closed

Archived by the PR that delivered it, carrying every decision above and every
acceptance rung below. What it left open is #688 (terminate) and
the cost note above; the delivery record is `CHANGELOG.md` and `docs/HISTORY.md`.
