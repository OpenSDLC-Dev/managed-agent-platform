---
status: archived
issue: "#92"
---

# Correlate the brain's turn-fault log — the `model_turn` consumer span

> Archived 2026-07-23: completed. Delivered in one PR; the narrative is in CHANGELOG.md and the
> decisions evaluated and rejected are in [docs/HISTORY.md](../HISTORY.md) § "Brain turn-fault
> correlation (plan 09)". **Everything below describes the state of the repository *before* that
> PR** — read it as the argument for the change, not a description of the result.

The plan for [#92](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/92).

## The defect

`internal/brain/brain.go`'s `Run` reports a failed turn with a bare

```go
slog.Error("brain: turn failed, lease left to expire", "error", err)
```

`slog.Error` logs against `context.Background()`, and `internal/telemetry`'s OTLP bridge
(`otelslog`) correlates a record by reading the span context off the *logging* context — so this
line reaches the collector with no trace, no span, and no session. It is the brain's counterpart of
the executor's fault log, which `internal/executor/executor.go`'s `report` already emits with
`slog.ErrorContext` from inside the open `tool_exec` span. A failed model turn is the more common
cause of a stalled session, so an operator who opens the trace finds every fault except the one
that stopped the turn.

## Why this needs a plan file

The `issue-triage` verdict was `needs_plan: true`, on the grounds that the issue proposes two
designs and choosing between them is an architectural decision on the observability seam that
CLAUDE.md principle 3 and ARCHITECTURE.md's `internal/queue` / `internal/brain` rows describe. The
diff size is explicitly not the test. What a plan file is worth here is the **decision record**: one
of the two proposed designs turns out to be inert against this repository's source, and the fix
brushes against a documented queue-level decision it deliberately does not reverse.

## The design space, resolved against source

**The issue's "cheap version" — `telemetry.Extract(ctx, item.TraceContext)` in `RunOnce` — cannot
work.** `internal/queue/queue.go`'s `Enqueue` captures a trace context **only** for `kind ==
ToolExec` and deliberately stores SQL NULL for a `model_turn`:

> Only tool_exec work is ever run as a tool execution, so only it carries a trace context […] A
> model_turn drives the brain, which opens its own model_request span per turn and never reads this
> back, so capturing it there would only persist an unread payload; leave it NULL.

ARCHITECTURE.md's Observability section states the same ("a `model_turn` item deliberately stores
none: nothing reads it back"). So `item.TraceContext` is always nil on the brain's path, `Extract`
returns the context unchanged, and the log would stay exactly as uncorrelated as it is today. The
issue was filed by a reviewer working from the executor's shape and did not check this.

**The symmetric version is therefore the only proposal that fixes the defect**: give the brain a
`model_turn` consumer span at the `RunOnce` level, mirroring the executor's `tool_exec` span and the
BYOC worker's, and emit the fault log from inside it.

`events.StartModelRequest`'s `model_request` span cannot carry the log itself. It opens inside
`runTurn` and is closed by `Finish` before `runTurn` returns, and the faults that matter happen on
both sides of it — before it opens (session-liveness lookup, replay, provider resolution, span-start
append) and after it closes (settlement, the lease proof). A consumer span stands for the handling
of one claimed message end to end, which is exactly the interval a stalled turn's cause lives in.

## Deliberately not in scope

Extending trace-context capture to `model_turn` enqueues — so a turn joins the trace of the API
request, the previous turn, or the tool run that woke it — is a **separate** decision. It would
reverse the queue-level choice quoted above at three enqueue sites (the API's `user.message`
trigger, the executor's resume-enqueue, the brain's own chained requeue), and those three are
different traces with different end-to-end semantics; picking one is not implied by anything #92
asks for. Without it the `model_turn` span roots the turn's own trace, which is what
`model_request` already does today — the trace topology is unchanged in shape, only deepened by one
level. The `tool_exec` items a turn enqueues keep parenting on its `model_request` span, so the
executor's and worker's existing correlation is untouched.

#92 asks for this to be weighed against
[#87](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/87), which has moved since: its
substance — the BYOC worker's `tool_exec` span recording an error status on a platform fault —
shipped in PR #138, and the issue stays open only for the deferred remainder, "lifting the shared
`Start` into one helper […] until both spans' scopes are reconciled (the executor's now also covers
its results commit)". This change adds a **third** near-identical `Start` call, and it belongs on
that deferred pile rather than being extracted here: the brain's scope diverges further still (no
`telemetry.Extract`, because there is no enqueuing trace to continue), so a helper written now would
have to be parameterised over exactly the difference that is unresolved. Nothing here changes the
worker.

## The fix

1. Open a `model_turn` span in `RunOnce` immediately after a successful claim, `SpanKindConsumer`,
   with `session.id` and `work.id` attributes — the executor's `tool_exec` attribute set.
2. Close it from a deferred exit that, on a returned error, sets `codes.Error` with the error text
   and emits the fault log with `slog.ErrorContext` under the span's context. The status matters as
   much as the log: an operator finds the log by clicking the red span.
3. `Run` keeps a log only for the path that has no span to hang one on — a `Claim` that failed
   before producing an item (`found == false`, `err != nil`). A claimed turn's fault is now reported
   once, from inside its span.

Only brain-side faults reach this exit. A model failure or a deterministic input problem is settled
onto the wire as a `session.error` by `failTurn` and returns no error, so it does not redden the
span — the executor's rule ("a tool-level failure is not a platform fault") applied to the brain.

## Acceptance criteria → coverage

- The fault log carries the `model_turn` span's **span id**, not merely its trace id — a parent and
  child share a trace id, so only the span id distinguishes this design from the cheap one. New
  `TestTurnFaultLogLandsOnTheModelTurnSpan`, the shape of `internal/executor`'s
  `TestFaultLogLandsOnTheToolExecSpan`, driving a real lease-loss fault through `RunOnce`.
- The faulted turn's span carries `codes.Error` and a description — same test.
- `model_request` is a **child** of `model_turn`, and `model_turn` is a consumer span — new
  `TestModelRequestSpanIsAChildOfTheModelTurnSpan`, pinning that the outer span really covers the
  whole claimed item.
- `tool_exec` items still carry the turn's `model_request` span as their parent — retained by the
  existing `TestToolExecEnqueueCapturesTurnTrace`.
- `make verify` green (build, crossbuild, vet, fmt, test, ≥90% coverage).
