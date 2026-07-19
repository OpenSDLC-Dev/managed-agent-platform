# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#93](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/93) — every binary's fatal-exit
log reached stderr but never OTLP, because each `main()` logged it after `run()`'s deferred telemetry
shutdown had already stopped the log processor. No plan file (single-PR fix; triage returned
`needs_plan: false`).

## Tasks

- [x] Reproduced against the real binaries: pre-fix `brain` with an unreachable `DATABASE_URL` logs
      to stderr while the collector receives nothing.
- [x] `telemetry.Run` owns init → body → fatal log → flush, so the ordering is not re-implementable
      per binary (`internal/telemetry/service.go`).
- [x] `telemetry.Init` moved ahead of each body, so pre-`Init` failures (missing env vars, a sandbox
      backend that will not construct) fall inside the bridge's lifetime too.
- [x] All four `main()`s rewired; `context.Canceled` is a clean exit in one place instead of three
      (new for the controlplane — a SIGTERM mid-startup now exits 0), flush on `context.Background()`.
- [x] Tests against the in-process OTLP collector the bridge suite already had; confirmed to fail
      under the old ordering.
- [x] End-to-end: post-fix `brain` exports `brain exiting` with `exception.message` to a live
      OTLP/gRPC sink on the identical input that produced nothing before.
- [x] `make verify` green (coverage 91.63%); verifier PASS with findings, all addressed.
- [x] Review round: the exit flush now drains logs first — sharing one deadline with traces and
      metrics could starve the fatal record — and both flush choices have mutation-checked tests.
- [ ] PR open, CI green, review threads settled.
