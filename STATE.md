# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#27](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/27) — Work Stop answered 200 +
JSON where the reference answers a bodiless 204. Plan:
[docs/plan/04_work-stop-204.md](./docs/plan/04_work-stop-204.md) (`in-progress`) — a plan file
because it reverses a CONFIRMED entry in docs/DIVERGENCES.md.

## Tasks

- [x] Contract settled against the pinned SDK: `poller.go:439-465` states the server returns 204
      with no body, and its `WithResponseBodyInto` workaround exists for exactly that.
- [x] Server: `handleNoContent` beside `handle`; `stopWork` returns only `error`. Errors and the
      409 conflict path unchanged. Worker: `forceStop` adopts the reference poller's bypass.
- [x] Tests: 204 + zero body bytes + absent `Content-Type` for graceful and force, state read back
      via GET; worker test pins the absent false warning. Mutation-checked in a scratch copy —
      removing the bypass reproduces the SDK's quoted decoder error verbatim.
- [x] `make verify` green (coverage 91.75%); verifier PASS with findings, including a live
      `ant beta:environments:work stop` run against the 204 server (graceful and force, exit 0).
- [x] Review round. The public Stop Work reference documents a `BetaSelfHostedWork` return and
      ranks *above* the SDK checkout, so this is recorded as a deliberate divergence from the
      published spec toward the deployed service, not as "no divergence", and the compatibility
      break is stated rather than glossed. The plan's empty-poll aside was backwards and is
      corrected (`200` + `null` stands). The log-capture helper restores the stdlib `log` too.
- [ ] PR open, CI green, review threads settled.
