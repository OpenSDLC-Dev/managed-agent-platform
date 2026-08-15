---
status: archived
issue: "#383"
---

# Plan 33 — a work item cannot run forever

A wedged sandbox call has no bound anywhere in the production path, and the lease keeper
actively protects it. This plan gives every work item an end, at the one place both halves
of the pull protocol already pass through, and deliberately leaves the finer instrument —
bounding a *call* rather than a whole item — to a later change with its own evidence.

Archived: slice 1 shipped (#383). Slices 2 and 3 were never in its scope and are now
issues #395 (the sandbox seam) and #396 (the executor's blast radius).

## What is broken (#383, verified against source)

1. `ExecRequest.Timeout == 0` means unbounded by design, and the five file primitives carry
   no timeout at all. The context is their only bound.
2. That context has no deadline: `cmd/executor` roots it at `signal.NotifyContext`, and the
   only derivation around tool work is `queue.KeepLease`, which returns `context.WithCancel`
   — cancel, not timeout.
3. The lease keeper renews forever. It cancels only when `Extend` fails, and a wedged exec
   leaves the row `active` and untouched, so `Extend` keeps succeeding. The documented
   recovery — a crash lets the lease lapse and another executor reclaims — never fires,
   because the process is alive, just blocked.
4. One wedged call takes the whole executor with it: `Run` is a single goroutine calling
   `process` synchronously, and `executor.replicas` defaults to 1.

Observed five times in one week of CI (#318), with goroutine dumps: eight to nine minutes
parked in `remotecommand.StreamWithContext` while every stream sat blocked. The only thing
that ended it was `go test`'s package alarm, which production does not have.

## Decisions

**D1 — Bound the item's *silence*, not its runtime, and not a single call.** A wall clock is
the wrong instrument twice over. For one call: `WriteFileStream` legitimately streams a
500 MB mount and an untimed `Exec` legitimately runs as long as its command does. For a
whole item: a `tool_exec` runs its turn's tools *serially*, and the one tool that carries a
cap — `bash`, at `toolset.MaxTimeout`, 10 minutes — may legitimately spend all of it, while
`read`/`write`/`edit` carry no cap at all (that absence is the wedge itself). So a turn of
several tools has no defensible aggregate ceiling: one low enough to contain a wedge would
kill a legitimate wide turn, and one high enough to spare it (hours) would not contain much. What both admit is a *progress* bound — the shape plan 17 already chose for the model
endpoint (`provider.StallGuard`): a run that keeps finishing steps may run as long as it
likes; a run that finishes none for a whole budget is wedged. Progress is reported by the
holder — provisioned, materialized, a tool answered — never inferred on a timer, because
only the holder can tell a long step from a stuck one.

**D2 — Enforce it where the lease is already renewed, so every existing recovery path does
the rest.** Both halves of the pull protocol funnel through a renewal that can fail, and
both already treat that failure as "the lease is gone, abandon the work": the in-process
keeper cancels `kctx`, and the BYOC worker's heartbeat cancels its run. Giving up on a
stalled item there needs no new cancellation machinery, no new state, and no new wire call.
The check runs *before* each renewal, never after, so a wedged item's lease is never bought
another interval it cannot use.

**D3 — Each lane measures with its own monotonic clock, and the control plane is untouched.**
In process, the keeper measures from its own start — the same reason its `Extend` budget is
a duration and not a database timestamp. In BYOC, the worker's heartbeat does the same in
the worker's process. The alternative for BYOC — having the control plane refuse to extend a
lease past some age — is deliberately rejected: it changes observable wire behaviour whose
reference semantics we cannot confirm, which would need a recording and a DIVERGENCES entry
to be honest about, and it would bound a worker the platform does not run.

**D4 — A released item is retried, not failed — and what already answered is kept.** The
lease is deliberately left to lapse rather than actively released, so an ordinary reclaim
retries the item. But a stall is not a lost lease and must not be settled like one: the
claimant still holds the lease at that moment, and the tools that already answered really
ran, with their side effects spent, so discarding them would have the reclaim run each of
them a second time — a duplicate push, a duplicate POST. They therefore commit down the
partial-commit path a backend fault already uses (lease asserted in the same transaction,
item left live, no resume while a use is unanswered); only the wedged step and everything
behind it is re-derived. A lease genuinely lost still commits nothing. The rule is every lane that
*answers* something: the web calls and the MCP calls commit the same way, an answered search
having been billed and an answered MCP call having run in a third party's system. Review
round five is why they do — the first cut discarded both, on the argument that a search and a
discovery are re-derivable, which is true of a *discovery* and false of a call whose answer
someone was charged for. The harvest lane is the one that still discards, and for a reason
that is not about cost: half a snapshot is not a snapshot, so a cut-short outputs pass has
nothing to keep. Where a stalled lane's remaining work is of its own kind it hands *this*
item back rather than enqueuing another, `Enqueue` being keyed (session, kind) over the live
states — the mcp_exec driver's rule, now the web driver's too. The
pre-existing
exception in the other direction stands: the per-repository `session.error` a failed clone
appends as it happens is outside any settlement, so a stalled item can leave one behind, as a
lease lost at the same moment always could. This plan introduces no retry budget; unbounded
retry of a failing item is pre-existing behaviour and is its own question.

**D4a — The ownership proof locks the row, and the commit is best-effort anyway.** `Assert`
takes the work row `FOR UPDATE` rather than reading it: an unlocked read proves ownership only
at the instant it runs, and the stalled claimant is exactly the one settling with nothing
renewing its lease, so a reclaim committing between the read and the commit would leave two
holders writing one session. `Claim` picks its rows `FOR UPDATE SKIP LOCKED`, so it skips a
row being settled rather than blocking on it, and a reclaim that got in first has already
changed `lease_expires_at`, which the read then fails on. Two ways the partial commit still
does not happen, both accepted: a settlement slower than the lease's remainder finds the item
reclaimed and rolls back to the pre-#383 outcome, and a call that ignores cancellation never
returns to settle at all (D4b(b)). The rule is "do not re-run what answered where we can",
not a guarantee. One state is closed rather than accepted: a stall with nothing left
unanswered completes the item instead of leaving it live, because `Enqueue` dedupes per
(session, kind) while a live item exists — the `model_turn` such a settlement schedules would
otherwise have its own follow-on `tool_exec` swallowed until the abandoned lease lapsed.

**D4b — Two consequences of releasing a live holder, accepted with their reasons.** (a) The
holder may still be inside the wedged call when a second claimant takes the item, so one
session's tools can overlap in its sandbox. That is the window crash recovery has always had,
with the difference that the corpse may still be twitching; the alternative is #383 as filed,
an item nobody can recover. (b) The wedged *process* is still wedged — the executor runs one
item at a time and the worker one session at a time — so the recovery depends on another
replica existing. Both are the blast-radius question, deferred whole to #396 rather than
half-answered here.

**D4c — The budget has a floor, because a budget under one step does not degrade, it loops.**
A run cancelled inside a step it cannot finish within the budget is reclaimed and re-runs the
same step, which is cancelled at the same point, forever: no error reaches the session, and
the retry is not a retry but a loop. `cmd/executor` and `cmd/worker` therefore refuse a
configured budget below the longest single step *that binary* can name, plus a minute for the
tail it cannot: a tool that hits its cap is killed a grace period later and still has to get
its result back, so a floor of exactly the cap would cancel the healthy, timed-out tool just
before it answered. `toolset.MaxTimeout` is the step both binaries have, and it is not the
only one — `EXECUTOR_REPO_CLONE_TIMEOUT` gives a single clone a context of exactly its own
length, and the materialization reports once per repository, so a monorepo deployment that
raises the clone budget past the stall budget rebuilds the loop this floor exists to prevent.
The floor follows it (`toolset.StallFloor`), for the budget an operator sets AND for the one
they leave to the default — the second is the case that would really have happened, since
raising a clone timeout for a monorepo is a thing an operator does and touching a knob about
stalls is not — and both refusals live in tested helpers (`ParseStallTimeout` for the value
typed, `CheckStallDefault` for the one left alone) rather than a copy per binary, a `main`
package being outside the coverage gate. The floor saturates rather than adding its minute
blindly: `time.ParseDuration` accepts every duration up to the largest int64 of nanoseconds,
and a step within a minute of that would wrap the sum negative — a floor every budget clears,
so the guard would invert rather than fail. The step no binary
can name — a cold image pull — is the deployment's to clear, which is why provisioning reports
between its own steps (credentials resolved, session lock taken, sandbox up) rather than only
on return, and why the knob's documentation states the loop instead of promising a harmless
retry. A `CheckpointMaxBytes` restore was on that list until the review rounds: it is five reported
steps now — fetched, validated, shipped into the sandbox, extracted, marker consumed — rather
than one silent stretch whose only bound was the budget it could exceed. The same rule caught
three other pairs hiding behind one report: a repository's presence probe and credential
decrypt ahead of the clone the floor is measured against, a tool's run and the post of its
result, and the worker's session-liveness read ahead of its paging scan.

**D5 — Deferred, with reasons.** (a) A stall guard at the sandbox seam, bounding a call by
silence inside both backends, is the finer instrument and the right eventual answer for the
file primitives, where the platform owns both ends. It needs progress plumbing inside the
Docker and Kubernetes transports, and the item bound already removes the outage — #395.
(b) The executor's single-goroutine blast radius is real but is a separate change with a
separate question: whether two items of one session may run concurrently on one executor,
which is already possible across replicas — #396.

## Slices

1. **The stall bound.** `queue.KeepLease` takes a stall budget and
   `LeaseKeeper.Progress`; the executor reports per item — each of provisioning's own steps,
   each skill reference resolved and written, each repository, mount, harvested output, web
   call, MCP server and MCP call, each tool answered — and passes `Config.StallTimeout`; the
   BYOC worker's heartbeat carries the same guard against
   its own `Config.StallTimeout`, exiting `hbExitStalled` and leaving the item for reclaim.
   The brain passes zero — its silence is bounded a layer down by `provider.StallGuard`. Both
   lanes are tested by consequence (a wedged run ends and is left reclaimable; a long but
   moving run finishes untouched; a stall keeps the results that answered) and every guard
   and reporting path is mutation-verified.
2. *(deferred, D5a → #395)* Silence-bounded sandbox primitives.
3. *(deferred, D5b → #396)* The executor's blast radius.
