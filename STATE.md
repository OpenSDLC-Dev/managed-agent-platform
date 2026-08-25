# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

Test-suite hardening — three CI flake/timeout issues surfaced during plan 36's gate
runs (test-environment artifacts, not memory-store behavior). Each lands as its own
fix PR, in order.

## Tasks

- [x] **#483** — `TestLongTimeToFirstTokenKeepsLease`: the lease keeper's `Extend` is
      bounded by the remaining lease, so the test's 250 ms TTL let a slow UPDATE on a
      loaded fixture Postgres overrun the budget. Scale the TTL to 1500 ms (the
      keeper-budget tests' proven-tolerant value); production's 2 min TTL is unaffected.
      The executor's sibling `TestLeaseRenewedDuringSlowProvision` (300 ms) had the same
      flake — CI surfaced it on this PR — and gets the same 1500 ms fix.
- [x] **#486** — `TestEnqueueNotifiesWorkChannelOnCommit`: the broker wakes every
      subscriber once when LISTEN activates (`setReady` then `wakeAll`), and `Ready` can
      return between the two, so the test's non-blocking drain of that coverage-start wake
      raced it into the pre-commit window. Wait for the wake deterministically instead.
- [x] **#490** — `internal/api` / `internal/executor` now run about ten minutes each on a
      loaded 8-CPU box (the executor measured past `go test`'s 10-min default at 613 s),
      and a binary that dies there skips the `defer` removing its pgtest fixture, feeding
      the next run. The `test` recipe now passes `-timeout 30m`.

This is the last of the three; STATE.md returns to idle when it merges.
