# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Plan 37 — scheduled deployments** ([docs/plan/37_scheduled-deployments.md](./docs/plan/37_scheduled-deployments.md), [#51](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/51)), six slices. A deployment binds an agent to an environment, credentials and initial events; an optional 5-field POSIX cron schedule fires it, and a settled attempt is one persistent deployment run.

## Tasks

- **Slice 1 — CRUD, the three lifecycle actions, `internal/cron`, migration `0031`.** Done:
  the cron engine, both tables, the `Deployment` domain type, the seven routes, the
  agent-archive refusal (plan decision 7), the `DELETE /v1/environments` message that
  names the deployments, and the slice-1 registry entries (plus four §8.1 drafted none
  for). [#523](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/523) deferred.
- **Slices 2 + 3 — the seam, then the manual trigger.** Done: `createSessionInTx`
  (behavior-neutral extraction), then `POST /run` — run row + session + settlement in one
  transaction, classified failures as error-bearing 200 runs, `sessions.deployment_id`
  and the durable `succeeded_at` (migration `0032`;
  [#520](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/520)'s read-path
  half — its render half is slice 5's), the real list filter, three registry entries.
- **Slice 4 — the scheduler.** Done: the controlplane tick (30s, DB clock), the
  unique-index occurrence claim, the fire (savepoint, settle, auto-pause on the
  fourteen), the one-hour catch-up collapse, `lock_timeout` on the fire and the row's
  four writers, the three instruments (`deployment.fires`, `deployment.occurrences.skipped`,
  `deployment.tick.duration`), spans `deployment.tick`/`deployment.fire`, and the
  slice-4 registry entries.
- **Slice 5** — next: the two run lists.
- **Slice 6** — close-out docs: the security-invariant bullets and the README status line.

Not started, and deliberately: webhooks ([#261](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/261)) and budgets ([#432](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/432)) are excluded from the plan and named in it.
