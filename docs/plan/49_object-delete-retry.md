---
status: archived
issue: "#645"
---

# An object delete that fails is retried, not forgotten (plan 49)

Deleting a session removes its `files` rows transactionally and then deletes
their objects best-effort, on the request path, under one wall-clock budget
shared with the checkpoint archive. **Nothing retries what that pass does not
finish.** Three ways it does not finish, and each leaves bytes behind for good:
the store returns an error for a key; the budget runs out, which a sequential
loop of up to two hundred single-object deletes against a store at a few hundred
milliseconds each reaches easily; or the process dies between the commit and the
cleanup.

`internal/api/files.go` states the standing position for a single object — "a
failure here leaves a rare orphaned object, accepted and documented in the plan
— GC is a non-goal". A session's snapshot is not a single object, and "rare"
stops describing it once the failure mode is a slow store rather than a lost
race.

## Why the reaper cannot already be the answer

The checkpoint blob beside the deliverables is deleted best-effort too, and its
comment argues that this is acceptable *because* it is not the only remover: the
reaper's deleted tier deletes the same key before it reaps. That argument has a
hole, and #320 is the hole written down.

`reapPass` lists `provider.Owned()` and visits **only** those sessions
(`internal/executor/reaper.go`). A session whose sandbox the idle tier already
destroyed never appears in another `Owned` listing, so no pass ever revisits it.
For that session the reaper is not a second remover at all — the API's single
attempt is the only one, and the checkpoint orphans exactly the way the
deliverables do. `deleteSession`'s own comment says as much in the next
sentence: "this covers the session whose sandbox is already gone, which no reap
pass will ever visit again."

So a retryable cleanup cannot be a new reaper tier, however natural that sounds.
It needs a sweep that is not scoped by what a sandbox endpoint currently holds.

The reaper's own checkpoint delete stays where it is, which leaves two removers
for that one key. For a session whose sandbox the reaper still owns it is the
faster of them, and for the session this plan is about it was never reached at
all; whichever arrives second deletes nothing, since a missing key is nil for
every backend (decision 5).

This also settles a question #645 left open. It says the checkpoint key "is a
candidate for the same mechanism … but neither is broken today, so neither
should drive the design". The first half is what #320 refutes with evidence: the
checkpoint *is* broken today, in the idle-reaped case. It still should not drive
the design — the deliverables are the larger and more reachable leak — but it
costs one more enqueued key to fix, and leaving a known orphan of the same shape
in the same function would be a strange place to stop. This plan closes both.

## Decisions

1. **A queue table whose rows are deleted on success, not columns on the
   tombstone.** `deleted_sessions` is the other candidate — the deleting
   transaction already writes it — but its own migration says what it is: "three
   small columns … kept indefinitely", because the reaper needs the tombstone as
   permanent evidence that this deployment deleted the session. Keys carried
   there would be kept indefinitely too, long after the objects were gone, so a
   done-marker would have to be added to distinguish them: a queue, built inside
   a table designed never to drain, with worse semantics than a queue. A
   separate table is bounded by the backlog rather than by history, and answers
   a question the tombstone cannot: *what is still owed right now.*

2. **The keys are enqueued inside the deleting transaction**, from the same
   `DELETE FROM files … RETURNING id` that already yields them. The lesson plan
   48 just relearned: a durable statement of what is owed must commit atomically
   with the row removal, or the crash window it exists to close is still open.
   An enqueue after the commit would leave exactly the third failure mode above.

3. **The request path deletes nothing.** `deleteSession` enqueues and returns.
   This is what #645 itself proposes ("the handler could stop deleting objects
   entirely"), and it removes `sessionDeleteCleanupBudget` — thirty seconds, the
   largest contributor to a delete's worst-case response — along with the
   partial-progress accounting the loop needed to report what it had skipped.
   One remover instead of two also removes the question of what the two do when
   they race.

4. **A control-plane sweeper, not an executor one.** The drain needs a blob
   store and a schedule, and both binaries have them; the control plane wins on
   three counts. It owns the `files` domain these keys belong to. It always
   runs, while a deployment can have no cloud executor at all (every reap tier
   is cloud-only), and a cleanup that silently does not happen in some
   topologies is the failure this plan exists to end. And it already runs three
   sibling sweepers — `StartMemoryRetention`, `StartDeploymentScheduler`,
   `StartDreamRunner` — so the shape, the wiring and the shutdown are precedent
   rather than invention.

5. **Claimed with `FOR UPDATE SKIP LOCKED`**, the work queue's pattern, so
   several control-plane replicas drain one table without duplicating work or
   blocking each other. Correctness does not depend on it — deleting a key twice
   is nil for every backend, which is the contract `blobtest`'s
   `DeleteMissingIsNil` rung pins — but doing the same store round trip N times
   is waste a `SKIP LOCKED` claim avoids for free.

6. **A failed key stays, with a backoff; it is never dropped.** The row is the
   only record that an object is still owed, so discarding it after N attempts
   would re-create the leak this plan closes and hide it as well. A permanently
   failing key therefore retries forever, which is why the backoff is capped
   rather than unbounded and why `attempts` and the last error are on the row:
   the table is then also the operator's answer to "what has this deployment
   failed to delete", which no log line can be.

7. **The sweeper is woken in-process after the commit**, by a non-blocking send
   on a capacity-one channel — the coalescing wake of plan 48, without the
   Postgres round trip, because here the producer and the consumer are in the
   same binary. So the ordinary delete still removes bytes in milliseconds
   rather than at the next interval, while the interval remains what catches
   another replica's work and anything a wake lost. The wake cannot fail, cannot
   block, and is never load-bearing: everything it does, the interval also does.

8. **`deleteFile`'s single-object delete stays as it is.** #645 says so, and
   unlike the checkpoint there is no #320 for it — one object, one race, on a
   path with no snapshot behind it. The mechanism is reusable when someone wants
   it.

## Acceptance

1. **A delete whose object store fails still removes the bytes.** With a store
   that errors on every delete until told otherwise, a session delete succeeds,
   the rows are enqueued, and once the store recovers the sweeper removes every
   object and drains the table.
2. **A delete whose process dies before any sweep still removes the bytes** —
   the enqueue rode the commit, so a fresh sweeper finds the work.
3. **The checkpoint key is enqueued with the deliverables** (#320), including
   for a session whose sandbox is already gone.
4. **The request path no longer deletes objects**, and `deleteSession` no longer
   holds a cleanup budget.
5. **A permanently failing key is retried on a bounded backoff and never
   dropped**, and its row carries the attempt count and the last error.
6. **Two replicas draining at once do not duplicate the store round trips**, and
   neither blocks the other.
7. **A wake drains without waiting for the interval**, and the interval drains
   without a wake.
8. **Nothing else changes**: no wire surface, no reaper tier, no `deleteFile`.

## Docs

- `changelog.d/` — one fragment.
- `internal/api/sessions.go`'s cleanup comment, which currently argues the
  best-effort trade this replaces and names #645 as what would end it.
- `internal/api/files.go`'s "GC is a non-goal" position, which stays true of
  `deleteFile` and is no longer true of a session's set.
- `docs/ARCHITECTURE.md` — the object-storage seam gains a retrying remover.
- `STATE.md` — this plan is the active work.
- **Not** `docs/DIVERGENCES.md`: no wire surface changes, and no Managed Agents
  endpoint reports whether an object was removed.
