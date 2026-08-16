---
status: archived
issue: "#121"
---

# Bound a model turn by the endpoint's silence — the provider stall guard

> Archived 2026-08-01: completed, delivered in one PR. The narrative is in CHANGELOG.md and the
> designs evaluated and rejected are in [docs/HISTORY.md](../HISTORY.md) § "Model endpoint stall
> bound (plan 17)". **Everything below describes the state of the repository *before* that PR** —
> read it as the argument for the change, not a description of the result.

The plan for [#121](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/121).

## The defect

Nothing bounds a model turn's wait on the endpoint, at any layer.

`internal/provider/anthropic/anthropic.go` passes `option.WithoutEnvironmentDefaults()` —
correctly, to stop the SDK autoloading ambient `ANTHROPIC_*` credentials and leaking the operator's
real key to a third-party `base_url`. In `anthropic-sdk-go@v1.59.0` that option also routes
construction past `DefaultClientOptions()` (`client.go:207-214`), which is the only place
`option.WithHTTPClient(defaultHTTPClient())` is applied. So the clone that sets a 10-minute
`ResponseHeaderTimeout` never runs, requests fall back to bare `http.DefaultClient`
(`internal/requestconfig/requestconfig.go:176`) whose `Timeout` is 0, and `RequestTimeout` defaults
to 0 as well. `internal/provider/openai/openai.go` reaches the same place by a shorter road: it sets
`client: http.DefaultClient` outright.

No caller supplies a deadline either. `cmd/brain/main.go` builds a signal-only context, and
`internal/brain/brain.go`'s `kctx` comes from `queue.KeepLease`, which only adds cancellation on a
*failed* lease renewal — never on inactivity.

`Brain.Run` processes one turn at a time, and the lease keeper re-extends the work item at
`LeaseTTL/3` for as long as the stream is open. So an endpoint that completes the handshake and then
never sends response headers — a wedged proxy, a black-holing load balancer — does not stall one
turn: that brain replica serves no further session, for any tenant, and no other replica can reclaim
the item because the lease keeps being renewed. One unresponsive endpoint silently takes a replica
out of service with no error, no metric, and no log.

## Why this needs a plan file

The `issue-triage` verdict was `needs_plan: true`. The issue proposes two fixes without choosing
("a shared, package-level client with a timeout, **or** a deadline applied at the brain's call
site") and rejects a third outright, while pinning a constraint that rules out the obvious shape of
the first: `provider.Registry` constructs a provider per turn, which is only affordable because
every instance shares `http.DefaultClient`, so no fix may give an instance its own connection pool
(#88). Choosing between the two, and deciding where the bound lives relative to the SDK, is an
architectural decision on the provider seam. The diff size is not the test.

## The decision

**Bound the turn by silence, not by duration, and enforce it on the request context — one
`provider.StallGuard` per `Generate` call.**

Three things follow from that sentence, and each was the actual choice:

1. **Silence, not duration.** A model streaming a large answer legitimately holds one request open
   for many minutes; only *no progress at all* distinguishes it from a wedged endpoint. A total
   deadline would have to be sized for the longest healthy turn, which makes it far too loose to be
   the bound this issue asks for.
2. **The request context, not the HTTP client.** A `ResponseHeaderTimeout` on a shared client
   surfaces as a transport error, and `requestconfig.shouldRetry` retries on `res == nil` — so a
   wedged endpoint would buy three budgets, not one. Cancelling the context the SDK was handed
   returns immediately instead (`requestconfig.go` checks `ctx.Err()` right after the handler, ahead
   of the retry decision). It also leaves `http.DefaultClient` shared, so the registry's per-turn
   construction stays exactly as cheap as its doc comment claims.
3. **Progress measured in bytes, not in protocol frames.** The frames that prove a quiet endpoint is
   alive never reach an adapter: `ssestream.Stream.Next` swallows Anthropic `ping` events
   (`case "ping": continue`), and an SSE comment is not an event at all. A guard fed by content
   chunks would kill a model that is still thinking. The response body is wrapped instead — in the
   OpenAI adapter directly, and in the Anthropic adapter through `option.WithMiddleware`, which is
   the only hook the SDK offers onto a body it owns.

The budget is **per route**, `stall_timeout` in the `model_providers` file, because the worst
legitimate silence is a property of the endpoint: a hosted gateway answers in seconds, a queued
self-hosted model may take minutes to send its first byte. Absent, it takes
`provider.DefaultStallTimeout` = 10 minutes, which is the SDK's own judgment for the same hazard
(`defaultResponseHeaderTimeout`) — sized never to end a healthy turn.

A tripped guard reports `provider.ErrStalled`, which is a model-side failure like any other, so the
brain needs no change: `streamTurn` returns it unwrapped by `infraError`, and `failTurn` settles it
as a `session.error` of type `model_request_failed_error` — idling with `retries_exhausted` and
completing the item, or chaining a fresh turn and requeueing it when input arrived mid-turn. Either
way it is settled, not abandoned to a lease expiry that would hand the same wedged endpoint to the
next replica.

## Shape of the change

- `internal/provider/stall.go` — `StallGuard`, `NewStallGuard`, `ProgressBody`, `ErrStalled`,
  `DefaultStallTimeout`. The shape mirrors `queue.KeepLease`: the constructor returns the context to
  run the work under, the work reports progress, `Stop` releases the watcher.
- `internal/provider/provider.go` / `config.go` — `Config.StallTimeout` and the `stall_timeout`
  route key (a Go duration string; unparsable or non-positive fails at startup).
- Both adapters — guard the request context, wrap the response body, name a stall in the error the
  stream reports, stop the guard on `Close`.
- `internal/provider/providertest/contract.go` — three subtests every adapter must pass: a wedged
  endpoint and a mid-stream silence each end the turn inside the budget, and a keepalive-only
  upstream is left alone.
- `internal/brain/brain_test.go` — the acceptance end to end, with nothing faked in the model path:
  the real Anthropic adapter against a wedged `httptest` endpoint, the turn over and its
  `session.error` written inside the budget, the work item completed rather than left to reclaim.
