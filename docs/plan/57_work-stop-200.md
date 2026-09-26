---
status: archived
issue: 804
---

# Work Stop answers 200 with the work object (#804)

## The rule

`POST /v1/environments/{environment_id}/work/{work_id}/stop` answers **200 with the
`BetaSelfHostedWork` after the transition**, rendered as `GET …/work/{work_id}` renders it.
A stop that moves nothing (any stop of `stopped` work, a graceful stop of `stopping` work)
answers 200 with the item as it stands: no timestamp moves and nothing is re-armed. Past
validation and auth, the one error left is the 404 for an item the work API cannot see.

This needs a plan because it changes wire behaviour against the recordings and reverses a
registered decision. [Plan 04](./04_work-stop-204.md) (#27) moved this route from 200 to a
bodiless 204, reading the SDK work poller's "Today the server returns 204" as a description
of the service, and docs/DIVERGENCES.md registered that as deliberate, with a 409 for a
conflicting stop.

## The census

The recordings (managed-agents-wire-recordings at `6b9b67f`) hold **27 unique stops**: 3 in
`2026-09-02/batch2.json` and 24 in the `worker-network.json` of the four 2026-09-19
self-hosted sets. Those sets' `worker-wire/` files repeat the same 24 records, which is why
#804 first counted 51. Every stop answers **200** with `Content-Type: application/json` and
the work object, and **none is refused**.

| stop | count | answer |
| --- | --- | --- |
| graceful, of acked `starting` work | 3 | `stopping` |
| forced, of `active` work | 10 | `stopped` |
| forced, of `stopping` work | 1 | `stopped`: `stopped_at` stamped, the first `stop_requested_at` kept |
| graceful, of `stopped` work | 11 | the item unchanged |
| forced, of `stopped` work | 2 | the item unchanged |

That is 3 answering `stopping` and 24 answering `stopped`. The conflicting stops, the ones
this platform answered 409, are the 13 repeats of already-`stopped` work. They come from two
places: the reference worker follows each of its 10 forced stops of active work with a
graceful stop of the same item, and 2026-09-02's `work.stop.graceful`, `work.stop.force` and
`work.stop.again` stop an item already stopped. Each repeat leaves `stop_requested_at` and
`stopped_at` exactly as they were. The forced stop of `stopping` work (`custom-mixed-tools`
#47) was never a conflict: force moves `stopping` to `stopped`, here as on the reference.

## Decisions (the owner's, 2026-09-26, on #804)

1. **Stop answers 200 with the work object after the transition.** The service answers what
   the spec declares. The poller comment plan 04 relied on is falsified.
2. **A repeat stop answers 200 with the current object, changes nothing and re-arms
   nothing.** That includes a graceful stop of `stopping` work, which no recording reaches;
   docs/DIVERGENCES.md registers it as INFERRED, under #78. A stop that moves nothing owes
   the session no re-arm: the transition that stopped a `tool_exec` has already re-armed it,
   and because the reference worker repeats its stops routinely, a repeat that re-armed would
   hand the same calls out again.
3. **The stop semantics the recordings contradict go to #810, not here.** On the reference, a
   graceful stop of acked `starting` work goes `stopping`, where this platform stops it
   outright. A `NO_HEARTBEAT` claim on `stopping` work answers 200 there and 412 here. This
   plan leaves both as they are.

## Scope

- **Control plane:** the route moves from `handleNoContent` to `handle`. `stopWork` answers
  through `toWire`. `queue.StopWith` returns the item after the stop and whether the stop
  moved it, and only a move to `stopped` re-arms. `stopped` is terminal, so a stop of
  `stopped` work is answered with the item as it stands before the session row lock is taken.
- **Worker:** `forceStop` keeps the poller's `WithResponseBodyInto` bypass. It serves this
  200 and an older server's 204, and the worker still ignores an older server's 409. It reads
  the 200's body to the end before closing it, so the transport can reuse the connection.
- **Clients:** the generated SDK's typed `Stop`, which a 204 failed, now decodes the answer,
  and `ant beta:environments:work stop` prints the object.
- **Registry:** the response-shape entry moves from the CONFIRMED divergences to the
  compatibility notes, with the chain of reversals kept auditable. The graceful stop of
  `stopping` work gets its own INFERRED entry, and the graceful-vs-force entry names #810.
  Two differences every recorded work body shows, on every work surface and older than this
  plan, get CONFIRMED entries: the `actor: null` no surface here renders, and an `id` that is
  the session's there and a `work_` id here.

## Verification

- The tests fail against the old handler, and each rule also fails under its own mutation:
  the stop answers GET's rendering; repeat stops of `stopped` work, forced and graceful,
  leave the object unchanged; a graceful stop of `stopping` work leaves it unchanged; a
  forced stop of `stopping` work stamps `stopped_at` and keeps `stop_requested_at`; a stop
  that moves nothing re-arms nothing, one that loses a race to another stop included; the
  typed SDK `Stop` decodes the answer; the worker takes a 200, a 204 and a 409 without a
  warning, and reads the 200's body to the end.
- The real `ant beta:environments:work stop` runs against the branch. Then `make verify`,
  `tools/registrycheck` and `tools/sdkref`, independent verification, both reviews and the
  PR's CI, before the squash merge.
