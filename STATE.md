# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Plan 37 — scheduled deployments** ([docs/plan/37_scheduled-deployments.md](./docs/plan/37_scheduled-deployments.md), [#51](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/51)), six slices. A deployment binds an agent to an environment, credentials and initial events; an optional 5-field POSIX cron schedule fires it, and every attempt is one immutable deployment run.

## Tasks

- **Slice 1 — CRUD, the three lifecycle actions, `internal/cron`, migration `0031`.** Done:
  the cron engine (Due/Next/Upcoming over one walk, embedded `time/tzdata`, the two DST
  rules), both tables, the `Deployment` domain type, the seven routes, the agent-archive
  refusal (plan decision 7), the `DELETE /v1/environments` message that names the
  deployments blocking it, and every slice-1 registry entry — 1, 2, 7, 8, 12, 15, 16, 19,
  22, 23, 25, 27, 29 — plus four more §8.1 drafted no entry for, one of them plan 35's,
  surfaced here on a typed object.
  [#523](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/523) is deferred.
- **Slices 2 + 3 — the seam, then the manual trigger.** Done: `createSessionInTx`
  (behavior-neutral extraction), then `POST /run` — run row + session + settlement in one
  transaction, classified failures as error-bearing 200 runs, `sessions.deployment_id`
  and the durable `succeeded_at` (migration `0032`;
  [#520](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/520)'s read-path
  half — its render half is slice 5's), the real list filter, three registry entries.
- **Slice 4** — next: the scheduler: tick, claim, fire, auto-pause, catch-up, and
  `deployment.occurrences.skipped`.
- **Slice 5** — the two run lists.
- **Slice 6** — close-out docs: the security-invariant bullets and the README status line.

Not started, and deliberately: webhooks ([#261](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/261)) and budgets ([#432](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/432)) are excluded from the plan and named in it.
