---
status: archived
issue: 810
---

# A graceful stop of acked work goes stopping, and its claim is told (#810)

## The rule

A graceful `POST /v1/environments/{environment_id}/work/{work_id}/stop` of work a worker
has acked, `starting` or `active`, moves it to **`stopping`**: `stop_requested_at` stamped,
`stopped_at` null, and the lease it has kept. For `starting` work that lease is the startup
lease its ack installed. Queued work, polled or not, still stops outright.

A `NO_HEARTBEAT` claim on `stopping` work that no beat has reached answers **200**
`{lease_extended: false, state: "stopping", last_heartbeat: ""}`, with `ttl_seconds` the
claim's effective TTL, and writes nothing. Every other failed claim keeps its 412: a claim
before the ack, a re-claim of active work, a claim on once-claimed `stopping` work, and a
claim on `stopped` work. A missing item stays 404.

This needs a plan because it reverses a registered rule. From #25 on, only `active` work
entered `stopping`, and docs/DIVERGENCES.md argued why: a `starting` item's worker had "no
channel left to be told on", since its only remaining beat was the claim and the claim was
refused. The recordings show the claim is that channel.

## The evidence

The recordings (managed-agents-wire-recordings, 2026-09-19) hold three graceful stops of
acked `starting` work, and each answers `stopping` (`-self-hosted-latest-cli` #2 and #5,
`-custom-mixed-tools` #23). One of them is followed through in `-custom-mixed-tools`
`worker-network.json`:

| # | request | answer |
| --- | --- | --- |
| 22 | ack | 200 `starting`, `acknowledged_at` set, `latest_heartbeat_at` null |
| 23 | stop `{}` | 200 `stopping`, `stop_requested_at` set, `stopped_at` null |
| 44 | heartbeat `?expected_last_heartbeat=NO_HEARTBEAT`, 59 s later | 200 `{"type":"work_heartbeat","lease_extended":false,"state":"stopping","last_heartbeat":"","ttl_seconds":120}` |
| 47 | stop `{"force":true}` | 200 `stopped`, the first `stop_requested_at` kept, `latest_heartbeat_at` null |

Between #44 and #47 the worker ran no tool. The set's README records that "a manual
dispatch of that same work then received heartbeat `stopping` and executed no tool". The
reference worker's heartbeat treats `stopping` as the control plane's stop and force-stops
the item on exit (checked against anthropic-sdk-go v1.70.1 — worker.go runHeartbeat). Every
ordinary recorded claim and echo answers `ttl_seconds: 300`; the stopping claim answers 120.

## Decisions (the owner's, 2026-09-26, on #810)

1. **Match the recordings.** A graceful stop of acked `starting` work goes `stopping`, and
   the claim on it answers 200.
2. **The 200 covers never-claimed `stopping` work only**, the one recorded case. A claim on
   `stopped` work, a claim on once-claimed `stopping` work and a graceful stop of queued work
   keep this platform's answers and are registered as INFERRED under #78. Either answer is
   safe for both workers: each cancels its run on a 412 and on `stopping` alike, and a later
   stop of stopped work is a 200 that changes nothing.
3. **`ttl_seconds` echoes this platform's effective TTL.** The recorded 120 is one sample,
   so it is registered rather than copied, and so is the adjacent gap: this platform's
   default TTL is 30 s, where the reference answers 300.

## Design

- **The stop keeps the startup lease.** Cleared, the item would meet the finalizer's
  null-lease arm, which exists for rows a pre-#25 replica wrote, and would be settled at the
  next poll. A claim as late as the recorded one would then find `stopped` work and get a
  412. Kept, the lease lets the finalizer treat never-claimed work like an abandoned claimed
  wind-down. It settles the item only once the lease has lapsed and `queue.WindDown` has
  passed since the request. Poll never re-offers `stopping` work.
- **The price, accepted to match the recordings.** A graceful stop of `starting` work whose
  worker has already died no longer settles and re-arms at the stop. It settles at the
  first poll of its environment after the lease and WindDown have both run out, a minute or
  more later, or never if nothing polls. Until then an environment delete answers 409
  unless it passes `?force=true`, and a `force: true` stop settles the item at once.
- **The sessions token lives while the item is `stopping`.** It used to die WindDown after
  the request, `stopping` or `stopped`. The reference worker beats and force-stops with the
  token when the item carries one, and the recorded claim was sent 58.9 s after its stop,
  the force stop at 60.1 s. With a token, that force stop would have been refused 401,
  leaving the item to the finalizer. Past WindDown it reaches only the item's heartbeat
  and stop: the finalizer runs only on a poll, which may never come, and a gone worker's
  token must not keep the session and its memories until then. The reference worker's
  memory flush runs before its force stop, so a claim that late cannot flush, as before
  #810; but its session read is refused too, which fails the item before its run starts,
  so nothing was written to flush. Once `stopped`, the token keeps the minute from the
  request, so the finalizer, which settles only past it, still never re-arms a session
  while the abandoned item's token works. A force stop re-arms at once, so its item's
  token can work for the rest of the minute beside the next item, as before #810.
- **The claim writes nothing.** The recorded force stop that follows still carries a null
  `latest_heartbeat_at`, so the answer records no beat and extends no lease. The claim is
  answered from the row read after the claim's update matched nothing. Its test is
  `state = 'stopping' AND last_heartbeat IS NULL`, and a once-claimed item always has a
  last heartbeat.
- **`last_heartbeat` can say "none".** `queue.HeartbeatResult.LastHeartbeat` becomes a
  `*time.Time`. The wire field becomes a string, which is how the SDK types it. When there
  is a heartbeat, the string is byte for byte what `encoding/json` rendered for the
  `time.Time` it replaces. When there is none, it is `""`, as recorded, not null.
- **One re-arm per finished stop.** A move to `stopping` re-arms nothing: `stopWork`
  re-arms only a move to `stopped`, and `StopWith` keeps its `(w, moved, err)`. The
  wind-down owes exactly one re-arm, and the path that finishes it pays it: the worker's
  force stop, or `FinalizeAbandoned` in the poll's transaction.
- **The BYOC worker needs no change.** Its heartbeat reads the reported state before
  `lease_extended`, so the 200 ends the loop as a requested stop: the run is cancelled and
  the item force-stopped, the reference's #44 to #47. Only comments that argued the old
  rule change.

## Scope

- **Control plane:** `queue.StopWith`'s graceful arm, `queue.Heartbeat`'s failed-claim
  path, the heartbeat wire type, `worktoken.Authenticate`'s window for `stopping` work and
  the sessions-token lane's stop-only admission past WindDown, and the comments on `Stop`,
  `Poll` and `stopWork`'s re-arm.
- **Worker:** comments only, on `hbExitStopRequested` and in its tests.
- **Registry:** the graceful-vs-force entry is rewritten as a dated reversal, confirmed by
  recording, and keeps the old rule's argument. The #804 response-shape entry stops saying
  that such work is stopped outright. The sessions-token entry states the token's new
  window. Three INFERRED neighbours are added under #78, and the two TTL differences are
  added as CONFIRMED divergences.

## Verification

- Tests written first fail against the old rule: the queue's stop, claim and finalization
  window; the API replay of #22, #23, #44 and #47; exactly one re-arm on the force-stop path
  and on the abandoned path; the typed SDK decoding the claim as it decodes the recorded
  body; the worker, stopped between its ack and its claim, cancelling its run on the
  claim's answer, posting no tool result, and force-stopping the item; and a sessions token
  authenticating that claim and force stop past WindDown, and refused everywhere else
  there. The neighbouring 412s and the queued stop are pinned too, and each rule is broken
  on its own to show its test catches it.
- `make verify`, `tools/registrycheck` against GitHub and `make sdk-bump-report`, then
  independent verification, both reviews and the PR's CI before the squash merge.
