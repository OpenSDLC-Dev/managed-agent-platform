# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#27](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/27) — Work Stop answered 200 +
JSON where the reference answers a bodiless 204. Plan:
[docs/plan/04_work-stop-204.md](./docs/plan/04_work-stop-204.md) (`in-progress`) — a plan file
because it reverses a CONFIRMED entry in docs/DIVERGENCES.md.

## Tasks

- [x] Settled the contract against the pinned SDK: `lib/environments/poller.go:439-465` states the
      server returns 204 with no body, and its `WithResponseBodyInto` workaround exists for exactly
      that; `worker_test.go:118-120` scripts Stop as 204.
- [x] Adversarial review's counterargument (200 + JSON serves a superset of clients) heard and
      answered in the plan — the superset holds only a consumer already broken against the
      reference, and being lenient is a migration hazard.
- [x] Server: `handleNoContent` adapter beside `handle`; `stopWork` returns only `error`. Errors and
      the 409 conflict path unchanged.
- [x] Worker: `forceStop` adopts the reference poller's response-body bypass, so a successful stop
      no longer logs `worker: force-stop failed`.
- [x] Tests: Stop asserts 204 + zero body bytes + no `Content-Type` for graceful and force, state
      read back via GET; new worker test pins the absent false warning. Mutation-checked — removing
      the bypass reproduces the SDK's quoted decoder error verbatim.
- [x] Docs: the CONFIRMED divergence replaced (not deleted), the stale `Tracked: #27` on the
      graceful/force entry repointed to #25, CHANGELOG entry written.
- [x] `make verify` green (coverage 91.75%); verifier PASS with findings, including a live
      `ant beta:environments:work stop` run against the 204 server (graceful and force, exit 0).
- [x] Review round: reviewers were right that the public Stop Work reference documents a
      `BetaSelfHostedWork` return and ranks *above* the SDK checkout — so this is now recorded as a
      deliberate divergence from the published spec toward the deployed service, not as "no
      divergence"; the compatibility break is stated rather than glossed; the plan's empty-poll
      aside was backwards and is corrected (the poller calls `Work.Poll` with no bypass, so
      `200` + `null` stands); the log-capture helper now restores the stdlib `log` package too.
- [ ] PR open, CI green, review threads settled.
