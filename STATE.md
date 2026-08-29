# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**Plan 37 — scheduled deployments** ([docs/plan/37_scheduled-deployments.md](./docs/plan/37_scheduled-deployments.md), [#51](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/51)), six slices. A deployment binds an agent to an environment, credentials and initial events; an optional 5-field POSIX cron schedule fires it, and every attempt is one immutable deployment run.

## Tasks

- **Slice 1 — CRUD, the three lifecycle actions, `internal/cron`, migration `0031`.** In
  flight. Landed: the cron engine (Due/Next/Upcoming over one walk, embedded `time/tzdata`,
  the two DST rules), both tables, the `Deployment` domain type, the seven routes, and
  every slice-1 registry entry but 25 — 1, 2, 7, 8, 12, 15, 16, 19, 22, 23, 27 and 29.
  Still owed, both of them changes to *other* resources' handlers: the agent-archive
  refusal (plan decision 7, entry 25) and the `DELETE /v1/environments` message naming
  the deployments that block it.
- **Slice 2** — extract `createSessionTx`, behavior-neutral, the plan-36-slice-5 idiom.
- **Slice 3** — `sessions.deployment_id` (migration `0032`), the real list filter, `POST /run`.
- **Slice 4** — the scheduler: tick, claim, fire, auto-pause, catch-up, and
  `deployment.occurrences.skipped`.
- **Slice 5** — the two run lists.
- **Slice 6** — close-out docs: the security-invariant bullets and the README status line.

Not started, and deliberately: webhooks ([#261](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/261)) and budgets ([#432](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/432)) are excluded from the plan and named in it.
