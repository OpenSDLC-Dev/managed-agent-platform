# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

Test-suite hardening — three CI flake/timeout issues surfaced during plan 36's gate
runs (test-environment artifacts, not memory-store behavior). Each lands as its own
fix PR, in order.

## Tasks

- [ ] **#483** — `TestLongTimeToFirstTokenKeepsLease`: the lease keeper's `Extend` is
      bounded by the remaining lease, so the test's 250 ms TTL let a slow UPDATE on a
      loaded fixture Postgres overrun the budget. Scale the TTL to 1500 ms (the
      keeper-budget tests' proven-tolerant value); production's 2 min TTL is unaffected.
- [ ] **#486** — `TestEnqueueNotifiesWorkChannelOnCommit`: the LISTEN/NOTIFY wake races
      the enqueue's commit visibility under load.
- [ ] **#490** — `internal/api` / `internal/executor` run within 10% of `go test`'s
      10-min package timeout on a loaded box; an explicit `-timeout` in the `test` recipe.
