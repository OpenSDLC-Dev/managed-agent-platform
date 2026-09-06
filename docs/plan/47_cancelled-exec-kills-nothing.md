---
status: approved
issue: "#598"
---

# A cancelled sandbox command leaves itself running (plan 47)

#598 was filed from the plan 40 review as a narrow reclaim race: the session
advisory lock `provisionSandbox` holds does not serialize a materialization pass
against a **crashed** holder whose in-sandbox command outlives the lock, so a
reclaiming executor can adopt the same sandbox and start a second `apt-get`
beside the first.

That race is real. Reading the code for it turned up three corrections to the
issue's own account, and together they say the defect is one layer down from
where the issue put it: **`Sandbox.Exec` promises nothing about the command when
the caller's context is cancelled, and both backends kill nothing.** The lock is
not what fails. The lock is one of several things built on a contract that does
not hold.

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

**A crash is not needed.** `kctx` is the lease-kept context, and provisioning and
every tool run happen under it, "so losing the lease cancels the work"
(`executor.go:449-454`). A stall (#383) or a lost lease cancels `kctx`, both
backends return `ctx.Err()` at once, and the lock's `defer` releases. The window
the issue attributes to a crashed process opens on the ordinary stall path too,
where code *is* still running and could have cleaned up.

**The exposure is wider than materialization.** `internal/executor/packages.go`
is the only pass that puts a `Timeout` on its sandbox commands. Every other one
— `extractRepo` and `repoPresent` (`repos.go:321`, `:339`), `mountsPresent`
(`files.go:185`), the memory hash-tree walks (`memory.go:166`, `:299`, `:689`),
the checkpoint restore's whole-filesystem untar (`checkpoint.go:416`),
`collectOutputs` (`harvest.go:210`) — sends a zero `Timeout`, which the contract
defines as "no limit, and then only the context bounds the call"
(`internal/sandbox/sandbox.go:361`). Bounding them by context is a deliberate
choice, not an oversight: plan 33 D1 refused a wall clock on a single call
because "an untimed `Exec` legitimately runs as long as its command does". But
a bound whose only enforcement is a cancellation that kills nothing is not a
bound.

**The codebase already knows.** `repos.go:200-218` sweeps the clone's staging
tar on a detached context precisely because "a context cancelled between the two
— the clone deadline, or a lost lease — makes `Exec` return without ever running
that tail". The workaround is there; what is missing is that the sweep's
`rm -rf` of the staging path races the cancelled `tar -xf` that may still be
extracting into it. The workaround is racy for the same reason it exists.

## Why a cancelled Exec kills nothing

Both backends have the same three-armed select, and the caller-cancellation arm
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
  the case the issue's title names.
- A **cancelled** executor — a stall, a lost lease, an answered call (plan 35
  decision 9) — is still running and can act. This is the common half, and the
  only one a contract change can close.

So the order is forced: the contract change is the smaller, testable half and it
subsumes several separate workarounds; the in-sandbox lock is the only thing
that reaches a dead holder, and it is the one that needs a liveness heuristic
that survives an arbitrary customer image. Slice 1 is the contract; slice 2 is
the lock.

## Decisions

1. **`Exec` terminates the command's process group before returning on caller
   cancellation, on both backends.** This is a change to a published interface
   contract, so it lands in `sandbox.go`'s doc comment and in the shared
   contract suite, not only in two implementations. What it promises is bounded
   deliberately: the process *group* the command leads, best effort, within a
   short budget — not a process tree, and not a guarantee against a tenant who
   forks out of the group.

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
   and converts the orphan into a corpse. The number the budget is set to is the
   whole of what decision 9 pays, and belongs in the slice-1 PR's description.

6. **A cancelled call is still not a timeout.** `TimedOut` stays false and the
   error return stays `ctx.Err()`. `TimedOut` means "the command outlived its
   deadline" (`sandbox.go:367-388`) and a caller who gave up learned nothing
   about the command's own deadline. Nothing in the classification changes.

7. **No new per-call `Timeout`s.** This is the simplification the contract change
   buys, and it is why the remedy is one change rather than a scattering. The
   unbudgeted calls listed above are already bounded by an outer context —
   `RepoCloneTimeout` for the clone (`repos.go:180`), `kctx` and the stall
   detector for the rest — and plan 33 D1's refusal to wall-clock a single call
   stands. What was missing is that cancelling those contexts stopped nothing.
   #383 gave the executor a way to give up; slice 1 is what makes giving up
   actually stop the work. `extractRepo` needs no timeout of its own once its
   deadline kills the `tar`, and the sweep beside it stops racing a live
   process.

8. **Slice 2 is scoped, not designed here.** An in-sandbox lock is the only thing
   that reaches a dead holder, and it carries the hard part: a liveness rule
   deciding when an owner is gone, running in an arbitrary customer image, that
   cannot itself wedge. It also has to earn its place against the alternative of
   widening the advisory lock to cover the four passes that never had it — which
   would serialize them against the reaper as well, and is a change to plan 24
   D4's boundary rather than to the sandbox. Slice 2 decides between those; this
   plan only records that slice 1 does not close the crash case and must not
   claim to.

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

## Slices

**Slice 1 — the cancellation contract.** `sandbox.go`'s `Exec` doc comment; the
kill on both backends with the detached budgeted context; docker's `execWrapper`
pid file; a new rung in the shared contract suite; and the two documents that
currently state the opposite (`contract.go:446-447`'s orphan sentence, plan 13's
known consequence) corrected in the same PR. `repos.go`'s sweep comment loses
the race it describes.

**Slice 2 — the crashed holder.** An in-sandbox lock whose lifetime follows the
manager process and handles a stale owner, or a widened advisory lock, decided
on the evidence at the time. Not started by this plan.

## Acceptance

Each rung is a test that fails before the change and passes after.

1. **The contract rung, on both backends.** A command holding a uniquely named
   marker is cancelled mid-run; `Exec` returns `context.Canceled`, and a
   *following* `Exec` on a fresh context counts zero surviving marker processes.
   One-sided and unpolled, the way `ExecTimeoutKillsTheWholeProcessTree`
   (`contract.go:358`) already is.
2. **With a zero `Timeout`.** The rung must pass on the shape the executor
   actually uses, which is the shape with no watchdog armed — so neither backend
   can satisfy it by leaning on the wrapper's deadline subshell.
3. **The marker is inside the group.** A plain `&` child counts; a `setsid`
   grandchild is deliberately out of scope and must not be asserted.
4. **Cancellation is still not a timeout**: the cancelled call reports
   `TimedOut` false and returns `ctx.Err()`.
5. **A kill that cannot land does not hang the cancellation.** With the kill path
   forced to fail or to exceed its budget, `Exec` still returns the caller's
   cancellation promptly.
6. **The clone's sweep no longer races.** A clone cancelled mid-extraction leaves
   no `tar` running and no staging residue.
7. **Mutation testing**, per the repo rule: every guard above gets a mutant that
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
