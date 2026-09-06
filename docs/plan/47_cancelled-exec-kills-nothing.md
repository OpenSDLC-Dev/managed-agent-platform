---
status: approved
issue: "#598"
---

# A cancelled sandbox command leaves itself running (plan 47)

#598 was filed from the plan 40 review as a reclaim race: the session advisory
lock `provisionSandbox` holds does not serialize a materialization pass against
a crashed *or cancelled* holder whose in-sandbox command outlives the lock, so a
reclaiming executor can adopt the same sandbox and start a second `apt-get`
beside the first. The issue already names the layer the remedy has to reach —
"both sandbox backends return from `Exec` on caller-context cancellation
… without proving the in-sandbox process group has terminated" — and this plan
agrees with it rather than improving on it.

Reading the code for it changed one thing and added another. **The lock covers
less than the issue's blast-radius paragraph assumes**, and **the exposure runs
past materialization into every executor pass but one**. Together with what the
issue already said, they put the subject where the remedy has to go:
`Sandbox.Exec` promises nothing about the command when the caller's context is
cancelled, and both backends kill nothing. The lock is not what fails; the lock
is one of several things built on a contract that does not hold.

## What the issue says, and what the code says

**The lock covers less than the issue assumes.** `provisionSandbox` holds the
advisory lock across reap, provision, restore and `installPackages`, and
releases it with `defer unlockSession` when it returns
(`internal/executor/executor.go:629-632`, `:696`). The four materialization
passes — skills, repositories, files, memory — run in `provisionAndRun` *after*
that return (`executor.go:559-572`), so they were never inside the lock at all.
For them there is no serialization to lose to a crash: they are not serialized
against a healthy holder either. The issue's "shared by every materialization
pass" is right about the blast radius and wrong about the mechanism.

**A crash is not needed — the issue says so, and the code confirms it.** `kctx`
is the lease-kept context, and provisioning and every tool run happen under it,
"so losing the lease cancels the work" (`executor.go:449-454`). A stall (#383)
or a lost lease cancels `kctx`, both backends return `ctx.Err()` at once, and
the lock's `defer` releases. This is the half the issue's "or whose work item's
lease lapses" already covers, and it matters to the design because it is the
half where code *is* still running and could have cleaned up — which is what
makes slice 1 possible at all.

**The exposure is wider than materialization.** `internal/executor/packages.go`
is the only pass that puts a `Timeout` on its sandbox commands. Every other one
— `extractRepo` and `repoPresent` (`repos.go:321`, `:339`), `mountsPresent`
(`files.go:185`), the memory hash-tree walks (`memory.go:166`, `:299`, `:689`),
the checkpoint restore's whole-filesystem untar (`checkpoint.go:416`),
`collectOutputs` (`harvest.go:210`), and the clone's own cancellation sweep
(`repos.go:212`, the one whose *context* carries a 15-second budget) — sends a
zero `Timeout`, which the contract
defines as "no limit, and then only the context bounds the call"
(`internal/sandbox/sandbox.go:361`). Bounding them by context is a deliberate
choice, not an oversight: plan 33 D1 refused a wall clock on a single call
because "an untimed `Exec` legitimately runs as long as its command does". But
a bound whose only enforcement is a cancellation that kills nothing is not a
bound.

**And the BYOC worker is the same code twice.** `internal/worker` is the
customer-hosted twin of `internal/executor` and makes the same zero-`Timeout`
calls through the same interface (`worker/files.go:247`, `worker/memory.go:230`,
`:482`, `:845`). Slice 1 lands in `internal/sandbox`, so the worker inherits the
fix without a line of its own — but it is inside the blast radius, and plan 35
decision 9's "our BYOC worker does the same on its heartbeat" means it meets the
cancellation half exactly as the executor does.

**The codebase already knows.** `repos.go:200-218` sweeps the clone's staging
tar on a detached context precisely because "a context cancelled between the two
— the clone deadline, or a lost lease — makes `Exec` return without ever running
that tail". The workaround is there; what is missing is that the sweep's
`rm -rf` of the staging path races the cancelled `tar -xf` that may still be
extracting into it. The workaround is racy for the same reason it exists.

**And `Exec` is not the only way a command runs in the sandbox.** This is the
one that decides slice 1's boundary, so it is stated before the remedy rather
than after. Skills never call `Exec` at all — they materialize through
`WriteFiles` (`skills.go:283`) — and files go through `WriteFileStream`
(`files.go:171`). On **Kubernetes** both of those are in-pod commands of their
own: the bulk write rides its content over exec stdin into a `tar`
(`k8s.go:1608`) and the stream write does the same (`k8s.go:1461`), each through
the raw `client.exec` rather than through `Sandbox.Exec`. The code already knows
what that costs on cancellation — "the `tar` on the other end extracted whatever
arrived before the stream died, and a caller that went away mid-transfer …
reaches WriteFiles no other way than here" (`k8s.go:1529-1533`) — and sheds the
residue on a detached context without stopping the `tar`. Checkpoint capture
takes the same raw path (`k8s.go:447`).

Docker is not affected the same way: its bulk write hands a tar archive to the
daemon, which extracts host-side, so there is no in-container process to leave
running. The gap is one backend's, and it is exactly the shape that would let a
slice 1 confined to `Exec` pass its contract tests while Kubernetes
materialization behaves as it does today.

## Why a cancelled Exec kills nothing

Both backends have the same two-armed select — three outcomes, since the
deadline and the cancellation share an arm — and the caller-cancellation branch
returns before anything else happens: `docker.go:1050-1053` and
`k8s.go:1187-1190` both read `case <-runCtx.Done(): if ctx.Err() != nil { return
sandbox.ExecResult{}, ctx.Err() }`. No signal is sent, no second call is made.

Nothing else picks the work up. The only killer on either backend is a watchdog
*inside* the sandbox, and it is armed only when a non-zero timeout was asked
for: docker's `execWrapper` guards its subshell with `if [ "$2" != "0" ]`
(`docker.go:107`) and k8s's does the same (`deadline.go:72`). A zero-`Timeout`
call — which is most of the executor's — has no watchdog at all. Both send
`SIGKILL` to a process **group**, not a tree, so a child that calls `setsid`
escapes either way; both say so in place.

The declared contract is silent on all of this. `Exec`'s doc comment
(`sandbox.go:394-401`) describes cancellation only as an error shape — "the
context cancelled — which the toolset carries up as a backend fault". The shared
contract suite states the opposite of what a fix would need: "It may leave an
orphan behind — that is the container's to reap"
(`internal/sandbox/sandboxtest/contract.go:446-447`). And plan 13 records it as
a known consequence: "an interrupted tool keeps running in its sandbox with
whatever side effects it has left. The outcome is unaffected either way"
(`13_user-interrupt.md:133-140`). That last sentence is the one this plan
overturns: the outcome *is* affected once the abandoned command is a package
install or a tar extraction that a reclaiming executor is about to run again.

## Why the issue's two remedies are not alternatives

#598 offers "have `Exec` guarantee process-group termination before returning on
cancellation, **or** an in-sandbox install lock". They do not substitute for each
other, because they answer different halves:

- A **crashed** executor runs no code. No amount of care in `Exec`'s cancellation
  path executes in a process that is gone, so the contract change cannot reach
  the crashed half at all.
- A **cancelled** executor — a stall, a lost lease, an answered call (plan 35
  decision 9) — is still running and can act. This is the common half, and the
  only one a contract change can close.

So the order is forced: the contract change is the smaller, testable half and it
subsumes several separate workarounds, while every remedy for a dead holder is
larger and needs evidence this plan does not have yet. Slice 1 is the contract;
slice 2 is the dead holder.

## Decisions

1. **The seam terminates the command's process group before returning on caller
   cancellation, on both backends.** This is a change to a published interface
   contract, so it lands in `sandbox.go`'s doc comment and in the shared
   contract suite, not only in two implementations. What it promises is bounded
   deliberately: the process *group* the command leads, best effort, within a
   short budget — not a process tree, and not a guarantee against a tenant who
   forks out of the group.

   **The subject is every in-sandbox command the seam starts, not the `Exec`
   method.** Kubernetes runs the bulk write and the stream write as in-pod
   commands of their own, so a contract written about `Exec` alone would leave
   skills and file materialization — the passes that never call `Exec` — exactly
   as they are, while the contract suite went green. Slice 1 covers those paths
   on k8s too. It does not cover docker's bulk write, which has no in-container
   process to stop, and the asymmetry is the backends', not the contract's: what
   the contract says is that no command the seam started is still running when
   the seam returns from a cancellation.

2. **The kill runs on a detached, budgeted context**, because the caller's is
   cancelled by definition and cannot carry another round trip. This is an
   idiom the repo already has, in four places, and the new code copies it rather
   than inventing one: `cleanup` (`docker.go:1789-1793`, a 10s budget),
   `removeDetached` (`docker/gate.go:143-156`), `discardBulk`
   (`k8s.go:1583-1588`) and the repo sweep (`repos.go:210`). Use
   `context.WithoutCancel(ctx)` rather than `context.Background()`, so the trace
   survives — the reason `gate.go:144-146` gives.

3. **The budget is load-bearing, not decorative.** The kill is itself a round
   trip that can hang, which is the exact failure plan 33 was written about
   ("eight to nine minutes parked in `remotecommand.StreamWithContext`"). A kill
   that blocks would put a wedge in the cancellation path — the one path whose
   whole purpose is to stop waiting. So the budget is short and its expiry is
   not an error: `Exec` returns the caller's cancellation either way. **A
   cancellation must never become a hang.**

4. **k8s needs no wrapper change; docker does.** k8s's wrapper already writes the
   command's in-pod pid unconditionally (`deadline.go:71`), so the kill is one
   more in-pod exec of the shape `probeAlive` already uses. Docker cannot do
   this today for two independent reasons: the daemon exposes no way to signal a
   running exec — "Docker has no API to kill a running exec, so this has to
   happen inside the container" (`docker.go:39-40`) — and the only pid it holds
   is the daemon host's (`api.go:382-384`), which names the wrong group inside a
   container with its own PID namespace. So `execWrapper` gains a pid file the
   way k8s's has one, and the kill is a fresh `execCreate`+`execStart`. Killing
   the *container* is not an option: it is the session's sandbox.

5. **Plan 35 decision 9's fast cancel is preserved, and its cost is stated
   rather than hidden.** That decision cancels the run context when a call is
   answered so "an interrupted `sleep 3600` costs one beat, not
   `toolset.MaxTimeout`" — it bought *return latency* for the serial runner, and
   it explicitly did not buy stopping the command. Adding one budgeted round
   trip keeps the latency win (a beat plus a bounded kill, against ten minutes)
   and converts the orphan into a corpse. The number is deferred to slice 1, but
   not the constraint it has to satisfy, or the claim would be unfalsifiable:
   **the budget must stay well under the keeper's own beat** (`LeaseTTL/3`, the
   interval decision 9 measures itself in), so a cancellation still costs about
   one beat rather than two. Kubernetes is the case that tests it — an in-pod
   exec is materially heavier than docker's daemon call (`probe.go:11`) — and
   its request is bounded by `StreamWithContext` (`client.go:150`), so the
   budget is the whole of the worst case.

6. **The kill resolves the start, and does not read "no pid yet" as "nothing to
   kill".** This is the failure mode the rest of the design would otherwise hide.
   The pid is written *inside* the sandbox, after the remote exec request is
   accepted — and on k8s after the command is already running, since the wrapper
   backgrounds it before recording it (`deadline.go:69-71`). A cancellation
   landing in either window would find no pid file, kill nothing, return
   successfully, and leave exactly the orphan this contract exists to prevent.
   Worse, it would do so invisibly: an acceptance rung that cancels a command it
   has already observed running never reaches the window at all. So the kill
   path must **resolve the start outcome within its budget** rather than take
   the file's absence as an answer — waiting for the pid to appear, or
   establishing that the command never started — and slice 1 owes a test that
   cancels with startup paused before the pid write.

7. **The kill re-checks before it signals, and says who cleans up.** A pid can
   be reassigned between the command exiting and the detached kill arriving, and
   a blind `kill -9 -<pid>` would then signal "whatever group has since been
   assigned that pid" — not a hypothetical, but the reason docker's own watchdog
   re-checks `kill -0` before its final kill (`docker.go:67-70`). The
   cancellation kill inherits that rule. It also inherits a question the timeout
   path never had to answer: k8s removes its `.pid` and `.exit` state in
   `exitScript` (`deadline.go:128`), which the cancellation arm returns before
   ever reaching (`k8s.go:1187`), so slice 1 must say who removes the state a
   cancelled exec leaves and prove it in a test — or every cancelled call
   accumulates residue in `/tmp` for the sandbox's life.

8. **A cancelled call is still not a timeout.** `TimedOut` stays false and the
   error return stays `ctx.Err()`. `TimedOut` means "the command outlived its
   deadline" (`sandbox.go:367-388`) and a caller who gave up learned nothing
   about the command's own deadline. Nothing in the classification changes.

9. **No new per-call `Timeout`s on the paths a cancellation reaches** — the
   simplification the contract change buys, and why the remedy is one change
   rather than a scattering. Most of the unbudgeted calls are bounded by an
   outer context already: `RepoCloneTimeout` for the clone (`repos.go:180`),
   `kctx` and the stall detector for the rest. Plan 33 D1's refusal to
   wall-clock a single call stands; what was missing is that cancelling those
   contexts stopped nothing. #383 gave the executor a way to give up, and slice
   1 is what makes giving up actually stop the work. `extractRepo` needs no
   timeout of its own once its deadline kills the `tar`, and the sweep beside it
   stops racing a live process.

   **Two paths are the exception, and slice 1 does not reach them.** The
   post-run memory apply runs on `process`'s own context rather than `kctx`
   (`executor.go:524`), deliberately, so the write-back survives the lease's
   cancellation (`executor.go:504-506`); and the reaper's standalone memory sync
   (`reaper.go:161` → `memory.go:752`) runs under a context with no deadline
   anywhere in the chain. Nothing cancels either, so a kill on cancellation
   delivers nothing there — these are the untimed sandbox calls plan 33 was
   written about, still untimed. Naming them is this decision's real content:
   whether they get a budget of their own is slice 2's question, not something
   slice 1 may be read as having handled.

10. **Slice 2 is scoped, not designed here**, and it has at least four candidates
   rather than the two #598 offers. An **in-sandbox lock** carries the hard
   part: a liveness rule deciding when an owner is gone, running in an arbitrary
   customer image, that cannot itself wedge. A **widened advisory lock** covering
   the four passes that never had it would serialize them against the reaper as
   well, making it a change to plan 24 D4's boundary rather than to the sandbox.
The reclaiming provision can **reap and re-provision instead of
   adopting**, which kills every process in the container including a dead
   holder's — `provisionSandbox` already does exactly that inside the same lock
   hold on the restore path (`executor.go:655-660`), so the machinery exists;
   its price is the one the others avoid, since it discards the mutable sandbox
   state checkpoint and restore exist to preserve. And an **adoption-time
   sweep** is the cheapest of the four: both providers already know when they
   are adopting rather than creating (`docker.go:408`, `k8s.go:162`), so an
   adopter could stop whatever a previous holder left running before it hands
   the sandbox back — bounded, and paid only on the reclaim path. It inherits
   decision 7's pid problem in its hardest form, since the state it reads was
   written by a process that is gone.

   **Two of the four cannot cover BYOC, and that is a deciding constraint.**
   `internal/worker` runs the same sandbox seam (`worker/toolexec.go:140`,
   `:204`), cancels on the same stall or lease loss (`worker/lease.go:549`), and
   its detached shutdown memory flush reads the sandbox afterwards
   (`worker/memory.go:378`, `:482`) — racing an abandoned tool exactly as the
   clone sweep races an abandoned `tar`. A Postgres advisory lock reaches none
   of that: plan 24 puts BYOC worker lifecycle out of scope because "the
   platform reaper is executor-only" (`24_sandbox-teardown.md:217-221`). So the
   widened lock is a platform-only remedy by construction, while the in-sandbox
   lock and the adoption-time sweep both travel to BYOC. Slice 2 decides among
   the four on that evidence; this plan records only that slice 1 does not close
   the crash case and must not claim to.

## What this does not close

**A child that leaves the process group.** Both wrappers kill a group, not a
tree, and both say so: a child calling `setsid` escapes, on docker
(`docker.go:49-52`) and on k8s (`deadline.go:17-19`) alike. The container's
teardown remains the outer bound, as it is for the deadline path today.

**A tenant who tampers with the pid file.** It lives in the sandbox's own
filesystem, which the agent can write — the caveat k8s already states at
`deadline.go:94-103`. The kill is best effort against a hostile image, exactly
as the watchdog is; neither is a security boundary, and the sandbox is one trust
domain.

**The crashed holder**, until slice 2. The reclaim race #598 names in its title
stays open after slice 1, narrowed to the case where the previous executor's
process is genuinely gone. Plan 40 decision 2's mitigations still carry it: apt
opens with `dpkg --configure -a`, and an unchanged failing list is capped at
three attempts per sandbox.

**A kill that does not land.** If the detached call fails or spends its budget,
the command survives and `Exec` returns the cancellation anyway. That is the
pre-existing behaviour, not a regression, and decision 3 is why it is preferred
to blocking.

**A command nothing ever cancels.** Decision 9 names the two: the post-run memory
apply and the reaper's standalone sync. A contract about cancellation cannot
reach a path that is never cancelled, and slice 1 does not pretend to.

## Slices

**Slice 1 — the cancellation contract.** `sandbox.go`'s `Exec` doc comment; the
kill on both backends with the detached budgeted context, resolving the start
rather than trusting the pid file's absence; docker's `execWrapper` pid file; a new rung in the shared contract suite; and the two documents that
currently state the opposite (`contract.go:446-447`'s orphan sentence, plan 13's
known consequence) corrected in the same PR. `repos.go`'s sweep comment loses
the race it describes.

**Slice 2 — the crashed holder.** An in-sandbox lock whose lifetime follows the
manager process and handles a stale owner, or a widened advisory lock, decided
on the evidence at the time. Not started by this plan.

## Acceptance

Each rung is a test that fails before the change and passes after.

1. **The contract rung, on both backends — with a start barrier.** A command
   holding a uniquely named marker is cancelled *after it is observably
   running*; the call returns `context.Canceled`, and a *following* command on a
   fresh context counts zero surviving marker processes. The barrier is the
   load-bearing part, not a detail: this repo has already shipped a
   caller-cancellation test that "tripped during `execStart` and never reached
   the branch it claimed to pin" (`docs/history/2026-07.md`), and the corrected
   docker test spells out the rule — "The stream must already be open when the
   caller gives up, so that the cancellation lands where a sandbox deadline
   would: mid-read" (`api_test.go:901-904`). A rung without it passes on a
   cancellation that beat the command to the start, proving nothing. The count
   itself stays one-sided, the way `ExecTimeoutKillsTheWholeProcessTree`
   (`contract.go:358`) is.
2. **`DeadlineExceeded` as well as `Canceled`.** The concrete case this plan is
   written about is a `context.WithTimeout` expiring (`repos.go:180`), not a
   `cancel()`, and the two reach the same arm by different routes.
3. **Cancellation *before* the pid exists.** The window rung 1 cannot reach:
   startup paused between the exec request being accepted and the pid write,
   then cancelled. No orphan may survive it. Without this rung the whole design
   passes while leaving the window open, which is why decision 6 exists.
4. **With a zero `Timeout`.** The rung must pass on the shape the executor
   actually uses, which is the shape with no watchdog armed — so neither backend
   can satisfy it by leaning on the wrapper's deadline subshell.
5. **The marker is inside the group.** A plain `&` child counts; a `setsid`
   grandchild is deliberately out of scope and must not be asserted.
6. **The bulk write, on Kubernetes.** A cancelled `WriteFiles` leaves no in-pod
   `tar` running — the rung that would fail an `Exec`-only implementation, and
   the reason decision 1 names the seam rather than the method.
7. **Cancellation is still not a timeout**: the cancelled call reports
   `TimedOut` false and returns `ctx.Err()`.
8. **A kill that cannot land does not hang the cancellation.** With the kill path
   forced to fail or to exceed its budget, the call still returns the caller's
   cancellation promptly — including when the budget is spent waiting for a pid
   that never appears.
9. **No residue, and no wrong process.** A cancelled call leaves no `.pid`/`.exit`
   state behind, and a kill arriving after the command already exited signals
   nothing.
10. **The clone's sweep no longer races** on the path where the kill lands: a
   clone cancelled mid-extraction leaves no `tar` running and no staging
   residue. Scoped to that path deliberately — decision 1 promises best effort,
   so an unconditional assertion would claim more than the design does.
11. **Mutation testing**, per the repo rule: every guard above gets a mutant that
   removes it, each dying by a *named* test.

## Docs

- `changelog.d/` — one fragment for the plan's approval; slice 1 writes its own.
- Slice 1 rewrites `internal/sandbox/sandboxtest/contract.go`'s orphan sentence,
  `sandbox.go`'s `Exec` comment, and plan 13's "Stopping is not instant"
  consequence, which becomes half true — the item still cannot commit, but the
  command no longer keeps running.
- **Not STATE.md**: nothing starts here. This PR lands an approved plan and no
  behavior, plan 41 is the incumbent active work, and STATE.md tracks what is in
  flight.
