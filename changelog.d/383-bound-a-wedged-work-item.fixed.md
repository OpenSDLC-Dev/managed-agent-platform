- **A work item that stops moving is ended and handed back, instead of being kept alive
  forever** (#383). A sandbox call is unbounded by design — `ExecRequest.Timeout` of zero
  means "as long as the command runs", and the five file primitives carry no timeout field
  at all — so the context is their only bound, and that context had no deadline anywhere:
  the executor roots it at the signal context, and the one derivation around tool work is
  the lease keeper's, which is a cancel, not a timeout. The keeper then made it worse than
  merely unbounded. It cancels only when a lease renewal fails, and a wedged call leaves the
  item's row `active` and untouched, so renewal kept succeeding: the lease never lapsed, no
  other executor could reclaim the item, and the documented crash recovery — the lease
  lapses, a reclaiming pass re-runs the still-unanswered tools — never fired, because
  nothing had crashed. The process was alive and blocked, and the platform's own bookkeeping
  was what protected it. Observed, not theorised: five CI runs in one week (#318) parked
  eight to nine minutes inside a Kubernetes exec stream with every stream blocked, ended
  only by `go test`'s package alarm, which production does not have.
  The bound is now on the item's *silence*, not on its runtime. A ceiling on total runtime
  is the wrong instrument here: a `tool_exec` runs its turn's tools serially, and the only
  tool that carries a cap — `bash`, at ten minutes — may legitimately spend all of it while
  the file tools carry none at all, so any ceiling tight enough to contain a wedge would kill
  a legitimate wide turn, and any ceiling loose enough to spare it would contain little. What
  both admit is progress — the shape the model endpoint has had since plan 17, applied to
  the work item. The holder reports each step it finishes, and a holder that reports nothing
  for its whole budget has its work cancelled and its lease deliberately left to lapse, so
  the item becomes reclaimable exactly as it would have if the process had died. Progress is
  reported, never inferred on a timer: only the holder can tell a long step from a stuck
  one, and a run that keeps finishing steps runs as long as it likes.
  A step is one *item*, not one pass: each of provisioning's own steps — the session's
  credentials resolved, the session lock taken, the sandbox up — then each skill reference
  resolved, each read of the probe that decides whether the set is unchanged, each skill
  written, each repository, each mount, each harvested output, each web call answered, each
  MCP server reached, each MCP call answered, each tool answered, and — in the worker, whose
  scan pages over the wire — each event it walks to find them. Two steps that look like one
  count as two: a tool's run and the post of its result (the worker's, over the wire, with no
  bound of its own), and the last item of a materialization pass and the sentinel write behind
  it. Per-pass reporting would have been the same defect wearing the
  fix's clothes — a session may mount eight repositories at five minutes apiece and dozens of
  files at half a gigabyte each, so a wholly healthy pass outlasts any budget a wedge should
  not, and because a materialization pass writes its sentinel only at the end, the reclaim
  would restart it from zero and be killed at the same place forever. The excluded items
  report too: a file that vanished, a repository this executor cannot clone, a skill whose
  row is gone — each still cost a round trip, and a tree of thousands of them is a run that
  is moving.
  A stalled item commits the results that already answered. That is the one place a stall
  parts company with a lost lease, and it matters: a tool's side effects are spent the moment
  it returns — a push pushed, a POST sent, an MCP call made in someone else's system, a web
  search billed against someone's quota — so discarding an answered result would have the
  reclaiming pass run it a second time. It holds in every lane that answers calls: the sandbox
  tools, the web calls and the MCP calls alike, each keeping what it has and handing the rest
  on. Only the outputs harvest still discards, because half a snapshot is not a snapshot. A stall is this claimant giving the lease up while it
  still holds it, so the answered results commit down the same partial-commit path a backend
  fault already uses (the lease asserted in the same transaction, the item left live, no turn
  enqueued while a use is unanswered), and only the wedged tool and the ones behind it are
  re-derived. That proof now locks the work row instead of merely reading it: an unlocked
  read is a proof at one instant, and the stalled claimant is precisely the one settling with
  nothing renewing its lease, so a reclaim landing between the read and the commit would have
  left two holders writing one session. A lease genuinely lost still commits nothing: there
  the row belongs to someone else. And a stall with nothing left unanswered completes the
  item rather than leaving it live — the turn it schedules would otherwise find its own
  follow-on work swallowed by the live-item dedupe.
  The commit is best-effort by construction, and the two ways it does not happen are worth
  naming: a settlement slower than what remains of the given-up lease finds the item
  reclaimed and rolls back to exactly the pre-fix outcome, and a call that ignores
  cancellation never returns to settle at all — the run that commits nothing is the one
  still wedged.
  Both halves of the pull protocol are bounded, each in its own process and against its own
  monotonic clock, at the loop that was already ticking while the run was wedged: the
  platform executor through the shared lease keeper (`EXECUTOR_STALL_TIMEOUT`), and the BYOC
  worker through its heartbeat (`WORKER_STALL_TIMEOUT`), which stops beating so the lease
  lapses server-side and the control plane re-offers the item. Both default to 30 minutes —
  three times the longest a `bash` call may take, so a full-length one behind a slow mount
  behind a cold image pull still clears it, since each of those now reports for itself. Both
  binaries refuse a budget below the longest single step they can name, plus a minute for the
  kill and the answer that follow a step which hits its cap: eleven minutes for the ten a
  `bash` call may run, and more for the executor when a repository clone is allowed longer —
  `EXECUTOR_REPO_CLONE_TIMEOUT` is one silent interval of exactly its own length, so a
  monorepo deployment that raises it is refused startup until it raises this with it. The
  budget an operator does *not* set is checked too, and that is the case that would actually
  have happened: raising the clone budget alone is the ordinary way to reach this — nobody
  raising a clone timeout thinks to touch a knob about stalls — and the default would have
  gone on cancelling the clone on every reclaim, unremarked. A budget under that step does
  not degrade into a retry, it loops, every reclaim re-running the step and being cancelled at
  the same point. Both refusals — the budget an operator typed and the default they left —
  are tested helpers in `internal/toolset` rather than a copy in each binary, a `main`
  package being outside the coverage gate. The step only a deployment can measure — a
  cold image pull — stays its own to clear, and the knob's documentation says so; a checkpoint
  restore is no longer among them, being five reported steps now — fetched, validated, shipped
  into the sandbox, extracted, and only then the marker consumed, since a pass cancelled
  before that update leaves the marker `ready` and restores again from zero on every reclaim. The worker deliberately does not force-stop
  the item it gave up on, for the same reason it does not stop one whose lease was taken:
  another worker may hold it by then, and a stop is terminal. The brain passes no budget at
  all — its silence is a model endpoint's, already bounded a layer down by the provider's
  own stall guard. Nothing on the wire changed: bounding a BYOC worker by having the control
  plane refuse to extend its lease would have changed observable behaviour whose reference
  semantics are unconfirmed, and it would bound a process this platform does not run.
  The budget bounds when a wedged holder is *cancelled*, not when its lane returns. An MCP
  call cut short is the longest case: that lane dials per call and closes on return, and the
  SDK sends the session-ending DELETE on a context nothing here can cancel, so it is capped by
  the client's own request cap — `CallTimeout`, **two minutes**, on the client tool calls run
  through — which a server whose handler is still blocked spends in full. Discovery's cap is
  its own client's thirty seconds. The item is released for reclaim on time either way, the
  keeper having stopped renewing when the stall fired; what waits is this executor's next
  item, which is the blast radius #396 is about.
  Every guard and every reporting path is measured against the code without it: unwiring
  either budget leaves its wedged-tool test hanging to its own alarm; dropping either lane's
  per-tool reports cuts a healthy thirty-tool run short; routing a stall back through the
  lease-lost branch loses the answered result; unlocking the ownership proof lets a second
  claimant take an item its holder is still settling; keeping only the setup error on a stall
  leaves the report unable to name the tool that wedged; and removing the per-item reports
  from provisioning, the materialization, harvest, web and MCP passes — each lane counted in
  both processes — drops each count to the pass boundaries alone. So is each guard the review
  rounds added: detection stops following the budget when the tick is left riding the lease's
  third, the stall tick buys the wedge another lease when the check is moved after the
  renewal, and the web and MCP lanes lose what they had answered when their stalls are routed
  back through the discard.

  **What this deliberately does not do.** It does not unwedge the call: the goroutine parked
  in the sandbox transport may stay parked (a Kubernetes exec that dies during the SPDY
  handshake, before the stream exists, is not cancellable at any layer). What ends is the
  *item*, not necessarily the call — and both consequences of that are accepted deliberately.
  One: the holder that gave the lease up may still be inside the wedged call when another
  claimant takes the item, so a session's tools can overlap in its sandbox — the same window
  crash recovery has always had, except the corpse may still be twitching. The alternative is
  what #383 filed: an item nobody can ever recover. Two: the wedged process is still wedged.
  The executor runs one item at a time and the worker one session at a time, so the recovery
  is another process picking the item up — which needs another replica to exist. That blast
  radius is #396, for both lanes; bounding a single sandbox call by silence inside the two
  transports, which would end the call itself, is #395.
