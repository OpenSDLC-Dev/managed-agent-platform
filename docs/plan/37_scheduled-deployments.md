---
status: in-progress
issue: "#51"
---

# Scheduled deployments — a cron-fired deployment and its run history (plan 37)

This plan addresses **#51**, whose title still carries the `(post-v1)` marker: the owner is
electing to pull it forward, so the PR that starts slice 1 drops that marker from the issue
title, or the backlog query in CLAUDE.md stops meaning what it says.

A **deployment** binds an agent to an environment, credentials, initial events and an
optional cron schedule, so the platform starts sessions on its own — no client in the loop
at 09:00. A **deployment run** is the immutable record of one such attempt: it names the
session that was created, or the error that stopped it, and nothing else. At drafting the
seam is reserved and nothing more: `internal/domain/id.go:29-30` already carries
`PrefixDeployment = "depl"` and `PrefixDeploymentRun = "drun"` in `knownPrefixes`
(`:87`), the session row renders `deployment_id` as a literal null
(`internal/api/sessions.go:57`), the sessions list short-circuits its `deployment_id`
filter to an empty page (`internal/api/sessions.go:1046-1050`), and no `/v1/deployments`
route exists.

---

## 1. Scope and goal

**In scope.** The ten wire operations across eight paths (§5.1), the `internal/cron`
expression engine, a controlplane-resident scheduler that commits at most one run per
due occurrence across replicas, the `deployment_runs` history and its two list surfaces,
the auto-pause on a failed scheduled fire, and `sessions.deployment_id` becoming a real
column with a real list filter.

**Out of scope, deliberately.** Webhooks (#261), budgets (#432), multi-tenant quotas
(#56), per-org rate limiting (#46), and everything else §9 names. `internal/cron` and the
tick are shaped so **#475 (dreams — memory-consolidation jobs)** can reuse them rather
than growing a second timer; that is a design constraint on the package boundary, not a
promise to build #475 here.

**Six slices**, each landing its own PR, its own `changelog.d/` fragment, its STATE.md
movement and its registry entries:

1. Deployment CRUD, the **three lifecycle actions** (`/archive`, `/pause`, `/unpause`),
   `internal/cron`, migration `0031`. `POST /run` is the fourth action endpoint and lands in
   slice 3, because it needs `createSessionTx` and `sessions.deployment_id` — both of which
   are slices 2 and 3.
2. `createSessionTx` extraction — behavior-neutral by construction, the plan-36-slice-5
   idiom, with the whole existing session suite as its regression test.
3. `sessions.deployment_id` (migration `0032`), the real list filter, `POST /run`.
4. The scheduler: tick, claim, fire, auto-pause — **and catch-up**, because a tick cannot
   select due work without a rule for what to do with a backlog. §3.6's window is part of
   the candidate predicate, not a later refinement of it. `deployment.occurrences.skipped`
   lands here too, not with the other lists: §3.6 increments it from the winner of the
   claim, so it cannot exist before the claim does.
5. The two run lists.
6. Close-out docs: the security-invariant bullets and the README status line, both of which
   describe behavior that only exists once slices 1-5 have all landed.

**Between slices 1 and 4 a schedule is stored, echoed and never fired.** `upcoming_runs_at`
computes correctly from slice 1 — it is a pure function of the expression — but nothing
tries the occurrences, so *"Presence enables scheduled execution"* is false in the tree for
the length of slices 2 and 3. That is stated rather than engineered around: the alternative
is rejecting `schedule` on create until slice 4 and then relaxing it, which ships a
validation rule that exists only to be deleted. Plan 36's slices 1-3 shipped the same shape
of dormant CRUD.

Each slice carries the documentation its own behavior needs, per §9's table — the package
reference with `internal/cron` in slice 1, the process-topology and observability
paragraphs with the scheduler in slice 4 — so slice 6 is close-out, not a documentation
backlog.

---

## 2. Ground truth — what the sources actually say

Every claim below is quoted from the Stainless OpenAPI spec or the pinned
`anthropic-sdk-go` / `anthropic-cli` checkouts. **The spec is not in this repository and
not among the six checkouts** `docs/REFERENCE_PROJECTS.md` names: it is the artifact whose
URL and hash are in `.stats.yml` at the SDK's pinned tag, fetched to a scratch path. Bare
`:NNNNN` citations below are line numbers into *that* artifact and resolve only against it —
convenient while this plan is being read, worthless a version later. So every entry that
lands in `docs/DIVERGENCES.md` (§8.1) cites the spec **by schema name** instead, the
convention that file already follows (`docs/DIVERGENCES.md:238-254`); it exists because
line-range citations there have already drifted four separate times across SDK bumps
(`docs/DIVERGENCES.md:77`). Rewriting each entry's evidence into schema-name form is part of
the slice that lands it, where the name can be checked against the spec of the day. Quotes attributed to
**platform.claude.com** were fetched live on 2026-08-27: `docs.claude.com` now 302-redirects
there, the section is `managed-agents/*` rather than `agents-and-tools/managed-agents`, and
appending `.md` to a page path returns clean source markdown. A docs page is the weakest of
the three sources and can change without a version bump, so each registry entry citing one
stamps it `(fetched YYYY-MM-DD)`, the idiom `docs/DIVERGENCES.md:104` already uses; a quote
that no longer re-fetches demotes its entry to INFERRED.

### 2.1 Resources and paths

Enumerated programmatically from `spec.paths` — eight paths, ten operations, **no `delete`
method anywhere**:

| Path | Methods |
|---|---|
| `/v1/deployments` | `post`, `get` (`:13048`) |
| `/v1/deployments/{deployment_id}` | `get`, `post` (update; `:13355`, body at `:13520`) |
| `/v1/deployments/{deployment_id}/archive` | `post` (`:13624`) |
| `/v1/deployments/{deployment_id}/pause` | `post` (`:13755`) |
| `/v1/deployments/{deployment_id}/unpause` | `post` (`:13886`) |
| `/v1/deployments/{deployment_id}/run` | `post` → `BetaManagedAgentsDeploymentRun` (`:14017`, `:14051-14054`) |
| `/v1/deployment_runs` | `get` (`:14150`) |
| `/v1/deployment_runs/{deployment_run_id}` | `get` (`:14342`) |

None of the four action endpoints declares a `requestBody`, and their SDK params structs
carry only the beta header (`betadeployment.go:2233-2254`). `BetaDeploymentService` has
`New`/`Get`/`Update`/`List`/`Archive`/`Pause`/`Run`/`Unpause` and no `Delete`
(`betadeployment.go:44-176`).

### 2.2 The Deployment object

`BetaManagedAgentsDeployment` (`:22982`) has 17 properties and a **16-entry** `required`
list (`:22986-23003`). The one property outside it is `budget` — *"Spend ceiling stamped
onto each session created from this deployment. **Absent when no budget is set.**"*
(`:23089`). Notable descriptions: `agent` is *"resolved to a concrete version"* (`:23022`);
`resources` *"Echoes the input minus write-only credentials"* (`:23051`); `metadata`
*"Maximum 16 pairs"* (`:23057`); `schedule` *"Presence enables scheduled execution; null
means manual-only"* (`:23063`); `status` *"Archived deployments report `active` with
`archived_at` set"* (`:23076`) against the two-value enum `["active","paused"]`
(`:23273-23277`); `paused_reason` *"Non-null exactly when status is paused; null
otherwise"* (`:23081`).

### 2.3 The schedule

`BetaManagedAgentsCronSchedule` (`:22739`) is *"5-field POSIX cron schedule with computed
runtime timestamps"* (`:22740`), `required: ["type","expression","timezone"]` (`:22743`) —
so `last_run_at` and `upcoming_runs_at` are **optional**, and the SDK agrees: `LastRunAt` is
`api:"nullable"` (`betadeployment.go:1490`) while `UpcomingRunsAt` carries **no `api:` tag
at all** (`:1496`).

- **Dialect** (`:22747`, repeated verbatim on the params schema at `:22787`; SDK
  `betadeployment.go:1479-1483`): *"5-field POSIX cron expression: minute hour
  day-of-month month day-of-week … Day-of-week is 0-7 where 0 and 7 both mean Sunday.
  Extended cron syntax - seconds or year fields, and the special characters L, W, #, and ?
  - is not supported, nor are predefined shortcuts (@daily)."* Both schema descriptions
  name the dialect **POSIX** (`:22740`, `:22780`).
- **Timezone** (`:22794`): *"Required. IANA timezone identifier … **Validated against the
  IANA timezone database.**"* The params schema adds *"Literal wall-clock matching in the
  configured timezone"* (`:22780`).
- **`last_run_at`** (`:22760`), in full: *"Time the most recent scheduled run actually
  started. Null until one completes; **preserved after the deployment is archived. Manual
  runs do not update this.**"*
- **`upcoming_runs_at`** (`:22765`), in full: *"Up to 5 timestamps of upcoming cron
  occurrences. **Non-empty for active and paused deployments** (reflects what the schedule
  would do if unpaused); **empty once the deployment is archived (`archived_at` set)**.
  Each fire is offset by a small per-schedule jitter, so a run will actually start at or
  shortly after its listed time."*

### 2.4 Create, update, and their bounds

`BetaManagedAgentsCreateDeploymentParams` (`:22287`),
`required: ["name","agent","environment_id","initial_events"]` (`:22291`). Bounds:
`name` 1-256, `description` ≤2048, `environment_id` ≤128, `vault_ids` *"Maximum 50"*
(`:22314`), `initial_events` *"At least 1, maximum 50"* (`:22319`), `resources`
*"Maximum 500"* (`:22324`), `metadata` *"Maximum 16 pairs, keys up to 64 chars, values up
to 512 chars"* (`:22329`). `agent` *"Accepts the `agent` ID string, **which pins the latest
version**, or an `agent` object with both id and version specified. The agent must exist and
not be archived"* (`:22304`) — and the deployment's agent union is **narrower than the
session's**: `BetaManagedAgentsAgentUnionParams` is `string | AgentParams` (`:21900-21911`;
SDK `:1875-1880`), with no `agent_with_overrides` arm, which `CreateSessionParams` does have
(`:22418-22421`).

`BetaManagedAgentsUpdateDeploymentParams` (`:28376`) is *"Omit a field to preserve its
current value"*. `vault_ids`, `initial_events`, `resources` and `schedule` are **full
replacements** (`:28400`, `:28404`, `:28409`, `:28429`); `metadata` alone is a **patch** —
*"Set a key to a string to upsert it, or to null to delete it. Omit the field to preserve.
The stored bag is limited to 16 keys…"* (`:28419`). `agent` as a bare string **re-pins to
the latest version**; *"Omit to preserve. Cannot be cleared"* (`:28391`).

`initial_events` on a deployment admits **three** types — `user.message`,
`user.define_outcome`, `system.message` (`:23155-23170`) — where session create admits
**two**: *"Supports `user.message` and `user.define_outcome` events. Maximum 50 events"*
(`:22451`). No ordering or adjacency constraint is stated on either.

### 2.5 The run record and its errors

`BetaManagedAgentsDeploymentRun` (`:23230`): *"A persistent, append-only record of a single
deployment execution. Records session creation success or failure — **no session lifecycle
tracking**"* (`:23231`). Eight required fields; `session_id` and `error` are each
*"Exactly one of session_id or error is non-null"*; `agent` is a *"Snapshot of the agent at
fire time"*; `trigger_context` is the discriminated union of
`BetaManagedAgentsScheduleTriggerContext` and `BetaManagedAgentsManualTriggerContext`
(`:28156-28170`), and `BetaManagedAgentsTriggerType` is `["schedule","manual"]` (`:28171`).

`scheduled_at` (`:25955`) is the load-bearing sentence of the whole plan: *"The UTC instant
at which the cron expression matched in the configured timezone, **before jitter is
applied**. **At most one run is recorded per (deployment_id, scheduled_at) pair.**"* The
manual context is *"The run was started manually by creating a session directly against the
deployment"* (`:24862`).

`BetaManagedAgentsRunError` (`:25886`) has **sixteen** members (`:25892-25907`), each
`required: ["type","message"]` with `message` = *"Human-readable error description"* (e.g.
`:21409-21418`). `BetaManagedAgentsDeploymentPausedReasonError` (`:23187`) has **fourteen**
(`:23193-23206`), each `required: ["type"]` **only** — no `message` (e.g. `:21402-21408`) —
and *"Matches the failed run's `error.type`"* (`:23188`). The two omitted from the pausing
set are `session_rate_limited_error` and `session_creation_rejected_error`. Their
descriptions cut in opposite directions: the rate-limited one says *"Session creation was
rejected due to rate limiting. The schedule keeps firing; subsequent runs may succeed"*
(`:26658`); the rejected one says *"The session create request was rejected with a
**non-retryable** validation error"* (`:26469`).

`BetaManagedAgentsErrorDeploymentPausedReason` (`:23573`) narrows the auto-pause trigger
precisely: *"**A scheduled fire** recorded a failed run whose error auto-pauses the
deployment."*

### 2.6 The two lists

| | Deployments (`:13195-13246`) | Runs (`:14173-14237`) |
|---|---|---|
| `limit` | Default 20, **max 100** (`:13205`) | Default 20, **max 1000** (`:14178`) |
| `created_at` | `gte`, `lte` only | `gte`, `lte`, `gt`, `lt` |
| filters | `agent_id`, `status`, `include_archived` (default false) | `deployment_id`, `trigger_type`, `has_error` |
| published rule | `status` *"To include archived deployments, use include_archived instead; **the two cannot be combined**"* (`:13229`) | *"Filtering by a non-existent deployment_id returns **200 with empty data**"* (`:14195`); `has_error` *"true for runs with non-null error, false for runs with non-null session_id"* (`:14209`) |

### 2.7 Sessions

`BetaManagedAgentsSession` (`:26095`) has 17 properties and a 16-entry `required` list
(`:26099-26116`) that omits exactly one: `deployment_id` — *"Deployment ID when the session
was created from a deployment reference. Null otherwise"*, `anyOf: [string, null]`
(`:26236-26238`); SDK `betasession.go:1257` marks it `api:"nullable"`. `budget`, by
contrast, **is** required on a Session. `CreateSessionParams` (`:22412`) carries **no
deployment field**. The sessions-list `deployment_id` param says only *"Filter sessions
created by this deployment ID"* (`:9615`).

### 2.8 Webhooks

Nine deployment events in the discriminator mapping — `deployment.created` / `.updated` /
`.paused` / `.unpaused` / `.archived` / `.deleted` and `deployment_run.started` /
`.succeeded` / `.failed` — at `:35175-35179` and `:35181-35184` (`:35180` is `agent.updated`
and is not one of them). Each payload is exactly
`{type, id, organization_id, workspace_id}`, all four required (e.g. `:34909-34919`); SDK
`betawebhook.go:180`, `:272`.

### 2.9 NOT OBSERVED

Checked in the spec and the SDK, found in neither:

- **Daylight-saving semantics.** `grep -iE 'daylight|\bDST\b|non-existent|ambiguous'` over
  the whole spec returns one hit, `:14195`, about a non-existent `deployment_id`. (Spelled
  `nonexistent`, the pattern returns zero — the spec hyphenates.) The
  `BetaManagedAgentsSchedule` and `…ScheduleParams` doc comments contain no DST text. The
  only adjacent published statement is *"Literal wall-clock matching in the configured
  timezone"* (`:22780`).
- **Catch-up, backfill or misfire policy after downtime.**
- **Overlap or concurrency policy.** `grep -i 'overlap\|concurren'` returns the boilerplate
  409 *"Aborted - The operation was aborted due to concurrency issue"* on every path plus
  seven substantive hits, none of them deployment-related: `:3967` and `:3970`
  (work-item `last_heartbeat` preconditions), `:22168` and `:28452` (memory and agent
  optimistic-concurrency preconditions), `:28274` (agent version, *"prevent concurrent
  overwrites"*), and `:27373` and `:27434` — the spec's only substantive use of the word,
  *"Overlapping activity…"*, describing a session's `active_seconds` accounting. Nothing
  about deployment scheduling.
- **Retention of run records.** `grep -iE 'retention|retained|purged'` has **exactly one
  hit in the entire spec** — `:8569`, a tunnel-certificate sentence. There is no
  memory-version retention prose in this file, and no run-retention rule.
- **Per-endpoint error semantics on any deployment path** (every path carries the same
  boilerplate 400/401/403/404/408/409/412/413/429/431/499/500/501/503/504 block), so:
  pause-when-paused, unpause-when-active, run-when-archived and a non-empty body on an
  action endpoint are all unpublished.
- **What a fired session is titled**, and **whether a fired session inherits the
  deployment's metadata**.

---

## 3. Scheduling semantics

### 3.1 The cron engine — `internal/cron`

Hand-rolled, ~250 lines, stdlib-only, three exported functions over one parsed expression:
`Due(expr, tz, from, to)`, `Next(expr, tz, after)` and `Upcoming(expr, tz, after, n)`. All
three share one occurrence generator, so the list a user reads in `upcoming_runs_at` and the
instant the scheduler fires can never disagree — the property test in §7 asserts exactly
that.

A dependency was considered and rejected: the reference's dialect is a strict *subset*
(no seconds, no year, no `L`/`W`/`#`/`?`, no `@daily` — `:22747`), so a general library
would need a rejection layer in front of it to refuse what it happily accepts, and would
still not supply the two DST rules of §3.2. The rejection layer plus the DST work is most
of the 250 lines; the library would be the smaller half.

**Day-of-month ∪ day-of-week is POSIX union**, not intersection: a restricted DOM and a
restricted DOW match on *either*. The spec never states the rule, but it names the dialect
*"5-field **POSIX** cron"* twice (`:22740`, `:22780`), and union is what POSIX specifies —
that naming is the strongest available evidence and it is what we implement. Registered as
INFERRED (§8.1 entry 15).

### 3.2 Timezone and daylight saving

`timezone` is validated at create and update time by `time.LoadLocation`; an unknown
identifier is a 400, which is what *"Validated against the IANA timezone database"*
(`BetaManagedAgentsCronScheduleParams.timezone`) requires. Two of its inputs are refused
**before** the call, because Go accepts both and neither is an IANA identifier: the empty
string, which `LoadLocation` reads as UTC while the field is required, and `Local`, which
resolves to whatever the host is configured for and would make one deployment fire at
different instants on different replicas.

Two DST rules — a wall clock that does not exist on the spring-forward day is **skipped**,
a wall clock that occurs twice on the fall-back day **fires twice** — are the two answers a
literal-wall-clock matcher must give to questions the spec does not ask. Neither is
published in the spec or the SDK (§2.9), so both are registered as **INFERRED** (§8.1 entry
19). They are nonetheless the highest-risk code in the plan, because a wrong answer looks
exactly like a right one; §7's fixed-zone contract tables are the bound.

**The tzdata problem is a build decision, not an environmental accident.** The server image
is `debian:stable-slim` with `ca-certificates` and nothing else installed (`Dockerfile:46-50`),
and a repo-wide grep finds no `time/tzdata` import and no `LoadLocation` call anywhere
today. If the image has no zoneinfo, every non-UTC deployment 400s at create time — in the
image, never in the gate, whose host has a zoneinfo tree. So: **`import _ "time/tzdata"`
lives in `internal/cron` itself**, not in a `main`. That is unusual for a library and
deliberate: it costs ~450 KB of binary and makes it impossible for a binary or a test to
forget. The compose smoke test creates a deployment with `America/New_York` so the
guarantee is exercised outside the gate host too.

### 3.3 `upcoming_runs_at` and `last_run_at`

`upcoming_runs_at` is computed on every read: five occurrences whenever five exist inside a
**twelve-year** search bound, `[]` once `archived_at` is set, populated while paused — the
first two behaviors of that triple are published verbatim on
`BetaManagedAgentsCronSchedule.upcoming_runs_at`, the "always five" is not (§8.1 entry 16).

The bound is twelve years rather than a round four because the sparsest *satisfiable*
5-field expression is `0 0 29 2 *`, and the Gregorian rule that 2100 is not a leap year puts
an **eight-year** gap in it. A four-year window would return `[]` for such a schedule while
it is active, contradicting the published non-empty rule.

The longer bound costs nothing on the read path because the matcher steps **fields, not
minutes** — it advances to the next candidate month, then day, then hour, then minute,
rather than testing all 6.3 million minutes twelve years hold. An unsatisfiable expression
is refuted in a few hundred steps, so the worst case — a 100-deployment list page (§2.6)
recomputing five occurrences each — stays cheap enough to need no cache.

That leaves the expressions with **no** occurrence at all. `0 0 31 2 *` parses, every field
is in range, and February never has 31 days; `upcoming_runs_at` would be `[]` for an active
deployment, which is the shape the wire reserves for an archived one. So create and update
**reject an expression with no occurrence inside the twelve-year bound** with a 400 rather
than storing a schedule that can never fire. Registered as INFERRED (§8.1 entry 27): the
reference publishes no validation rule for an unsatisfiable expression, and its own
non-empty guarantee is the argument that it must have one.

`last_run_at` is **derived, not stored**: the `created_at` of the latest such run —
`ORDER BY scheduled_at DESC LIMIT 1` over this deployment's runs where the trigger was
`schedule`, `session_id IS NOT NULL` **and `scheduled_at IS NOT NULL`**, reading that row's
`created_at`. The value is `created_at` and not `scheduled_at` because the field is *"the
most recent scheduled run actually **started**"* while `scheduled_at` is the pre-jitter cron
match (§3.4); the row is inserted as the fire begins, so `created_at` is the instant the
sentence asks for. The two differ by at most one tick interval in steady state, and after an
outage by up to the catch-up window (§3.6).

**That third conjunct is redundant and load-bearing.** A scheduled run always has a
`scheduled_at`, so the predicate excludes nothing — but the index §6 adds is *partial*
(`WHERE scheduled_at IS NOT NULL`), and Postgres will not use a partial index unless the
query's own `WHERE` clause implies its predicate. Without the conjunct written out the
planner refuses the index even with `enable_seqscan` off. Measured against the plan's DDL on
977,000 run rows, a 100-deployment page: **5,179 ms without it, 1.4 ms with**. The
watermark's `MAX(scheduled_at)` (§4.2) needs no such help — Postgres's own MIN/MAX rewrite
injects an `IS NOT NULL` for it, which is why only this half has to say it.

**Ordering by `scheduled_at` and taking the maximum `created_at` are not the same query**,
and the difference is deliberate. Two replicas whose fires overlap can insert an earlier
occurrence's row *after* a later one's: §4.2's watermark is read in the candidate scan and
never re-read inside the fire (§4.1 step 2 re-reads only `archived_at` and `paused_at`), and
§4.4's bounded concurrency can delay a fire past the next tick. `MAX(created_at)` would then
report whichever fire was inserted last; ordering by `scheduled_at` reports the most recent
*occurrence*, which is what *"the most recent scheduled run"* names. They disagree only when
one fire is queued longer than the whole interval to the next occurrence, so typically by
seconds — and by however much further that queue slips, which is the regime
`deployment.tick.duration` exists to surface (§4.4). The field is display-only either way:
the due-watermark is a separate `MAX(scheduled_at)` (§4.2) and is untouched by this.

The `session_id IS NOT NULL` conjunct — a fire that failed to create a session does not move
the field — is **INFERRED**, not published: the schema says *"the most recent scheduled run
actually started"* and never says what a failed start does (§8.1 entry 13). §7 asserts it
with a failed-fire case, because it is the half of the expression no source confirms.

Three published rules fall out of that one expression rather than needing three code paths —
*"Manual runs do not update this"* (the trigger filter excludes them), *"preserved after the
deployment is archived"* (archive touches no run row), and *"Null until one completes"* (no successful
scheduled run, no maximum).

Both are emitted always, never omitted: `last_run_at: null` and `upcoming_runs_at: []` where
they are empty. The reference makes both keys optional (§2.3), so emitting them is
compatible in both directions; omitting them would be too, and emitting is what every other
computed field on this platform does. The Deployment's `budget` is the one key we **omit**,
because *"Absent when no budget is set"* (`:23089`) makes absence the reference's own shape
for unset. Session `deployment_id` keeps emitting an explicit null, which is what
`internal/api/sessions.go:57` already does and what the SDK's `api:"nullable"` reads.

### 3.4 Jitter — not implemented

The reference offsets each fire by *"a small per-schedule jitter"* (`:22765`); the docs page
quantifies it (up to 15% of the inter-run interval, 5-second floor, 9-minute ceiling). A
self-hosted single-organization deployment has no fleet to spread, so we fire at the first
tick that sees an occurrence due — 0-30 seconds after it, the tick interval being a
30-second package constant, so that range is exactly one interval — which satisfies the
reference's
own weaker published claim that a run starts *"at or shortly after its listed time"*
(`:22765`). `scheduled_at` records the exact pre-jitter cron match either way (`:25955`), so
**the wire value is byte-identical** and only the wall-clock latency differs. Registered
(§8.1 entry 3).

### 3.5 Overlap — the reference states no policy, and neither do we

The reference expresses no concurrency policy: no create or update field carries one, and
the run record *"records session creation success or failure — no session lifecycle
tracking"* (`:23231`), so "is a prior run still executing" is not even a fact it holds. A due
occurrence fires whether or not the previous one is still running, and **so does ours**
(§8.3 decision 10, decided by the owner on 2026-08-29). Wire compatibility is this project's
premise, and a brake the reference does not have would be a behavioral divergence in a case
it never defined.

**The cost is accepted, not bounded.** The reference's brakes are budgets and
per-organization rate limits; we reject `budget` (#432), impose no session-creation rate
limit (#46), decline the 1,000-deployment cap, and accept `* * * * *`. And this platform's
reaper reclaims only three tiers — archived, `terminated`, and `idle` past its TTL with no
live work *and* no unanswered ask — so a `running` or `rescheduling` session is never
reclaimed, and a session on a `self_hosted` environment is not even considered
(`internal/executor/reaper.go:283-297`). A per-minute schedule pointed at an agent that does
not terminate therefore produces one session and one sandbox container per minute until the
Docker daemon or the Kubernetes namespace is exhausted.

What the plan owes instead of a brake is **visibility**, which costs no compatibility:
§3.8's `deployment.fires` counts every fire, and `GET /v1/sessions?deployment_id=…` — a real
filter from slice 3 — lists what one deployment has accumulated. Matching schedule
granularity to expected session duration is the operator's job, and §9 records the residue
as the one risk in this plan with no technical bound.

### 3.6 Catch-up after downtime

Nothing in any source describes what a schedule does about occurrences missed while the
platform was down (§2.9). The one adjacent published rule is about pause — *"Unpause resumes
the schedule from the next scheduled occurrence. Missed triggers are not backfilled"*
(docs page). We generalize it: **a tick fires at most the single most recent due occurrence
per deployment, and only if it falls inside a one-hour catch-up window.** A day-long outage
on a `*/5` schedule would otherwise spawn 288 sessions at once, and the reference's own
`(deployment_id, scheduled_at)` key admits every one of them.

The window is a **package constant with its reasoning in its comment**, the
`memoryVersionRetention` / `memoryVersionsKept` idiom
(`internal/api/memoryretention.go:36-46`) — not an environment variable. An
env knob would have exactly two useful values, and its `0` would mean "any restart
straddling an occurrence drops that run", which is every rolling upgrade. Adding a knob also
costs three deployment surfaces (`values.yaml`, `controlplane-deployment.yaml`,
`docker-compose.yml`, plus `deploy/gcp/staging-values.yaml`) for a value nobody has asked to
tune. If an operator asks, the knob is a two-line change then.

Because the collapse is a real loss of intermediate occurrences, it is **counted**, not
silent: §3.8's `deployment.occurrences.skipped`. **The counter rides the claim rather than
the tick**, or it would count the same collapse once per replica per tick: the winner of
§4.1 step 3 knows how many occurrences it passed over, and adds that number. A replica that
loses the claim, or finds nothing due, adds nothing.

Two details decide whether that number is honest. It is emitted **after the commit, never
inside the transaction**: an OTel counter has no rollback, so an increment made inside a
transaction that step 7 then discards would outlive the claim it was counting and be counted
again by the retry — the double-count this paragraph exists to prevent, reintroduced one
layer down. And the span it counts is bounded below by
`max(watermark, schedule_resumed_at)` rather than by the watermark alone, because
occurrences inside a pause were *paused*, not skipped; without the second bound a deployment
resuming after three days would report the whole paused interval as lost.

Two limits remain, and are named rather than engineered away. A process that dies between the
`COMMIT` and the increment loses that count — the ordinary cost of a counter that cannot
enlist in a transaction, and the reason this is an observability signal and not an audit
trail. And **tightening an expression resets nothing**, so the next winner counts the span
back to the old watermark under the *new* expression: a daily schedule changed to `*/5`
reports one large false skip, once.

The one collapse this cannot attribute is an occurrence that ages out of the window with no
later fire at all — no claim is ever won, so nothing counts it — which is the same
best-effort boundary §4.1 draws around liveness. Note the loss is bounded and specific —
with a per-occurrence claim and a one-hour window, a tick that runs late *delays* a fire
rather than dropping it, because the next tick still finds the occurrence due and inside the
window. What a collapse drops is the older occurrences of a backlog on a schedule finer than
the lateness.

Because `last_run_at` and the due-watermark are derived from `deployment_runs` (§3.3, §4.2)
rather than stored, changing `schedule.expression` or `schedule.timezone` resets nothing and
needs no special case: the new expression simply produces different occurrences, and the
catch-up window is the only thing that bounds a newly-tightened expression from back-firing.

### 3.7 Permission policy under a schedule

Agent-toolset defaults are `always_allow` and MCP-toolset defaults are `always_ask`
(`internal/domain/agent.go:27-29`). A scheduled session whose agent has MCP tools therefore
parks at 03:00 on a confirmation no human will answer, goes `idle` with a pending ask, and
is never reaped (`reaper.go:293-295`). We do **not** refuse such a deployment at create time
— a human who approves during the working day is a legitimate setup, and refusing would be a
divergence with no reference support. We document it in the security-invariant bullet
(slice 6). With no overlap brake (§3.5) nothing stops such sessions accumulating: a nightly
schedule against an `always_ask` MCP toolset parks one unreapable session per night until a
human answers or archives them. That is §3.5's accepted risk in its most likely form — a
schedule slow enough that nobody notices it accruing.

### 3.8 Observability

Design principle 3 is not optional, and the plan owes three things.

A fire has **no inbound `traceparent`** — there is no HTTP request — so the tick **roots**
its own trace, exactly as a `model_turn` does (`docs/ARCHITECTURE.md:500-502`). Span
`deployment.fire`, with the deployment id, the trigger type and `scheduled_at` as
attributes; the tick itself is `deployment.tick`.

Three instruments, named by exported constants asserted by the tests — the
`MetricMemoryVersionsPruned` idiom, which exists so a test can pin the exact string
(`internal/api/memoryretention.go:42-45`):

- `deployment.fires` — counted by outcome: `created`, `failed` (a classified error was
  recorded, sub-counted by run-error type) and `abandoned` (§4.1 step 7 rolled the whole
  transaction back, so there is no run row and no error type to label it with). Without the
  third, an infrastructure failure is invisible in the only place that counts fires. There
  is no *skipped* outcome: decision 10 declines the overlap brake, so a due occurrence
  always attempts a session.
- `deployment.occurrences.skipped` — the §3.6 collapse count, the one number that makes a
  dropped backlog debuggable.
- `deployment.tick.duration` — a tick that approaches the tick interval is the signal that
  the fleet has outgrown one sweep.

Slice 6 adds the paragraph to ARCHITECTURE's Observability section, which today names the
platform's instruments in prose without being exhaustive (`memory.versions.pruned` is
absent from it).

---

## 4. Architecture

### 4.1 Where a fire happens, and how one occurrence yields at most one run

The scheduler is a **controlplane background sweep** — a ticker calling one stateless
function — not a fifth binary. §8.3 decision 1 argues the alternative.

The guarantee is **at most one committed run per occurrence**, not one fire per due
occurrence, and it rests on **one unique index and no leader election**, because this
platform has leader election nowhere: the reference publishes its own idempotency key,
*"At most one run is recorded per (deployment_id, scheduled_at) pair"*
(`BetaManagedAgentsScheduleTriggerContext.scheduled_at`), and a partial unique index on
exactly that pair is that key.

Stating it as *exactly*-once would overclaim, and the four ways an occurrence gets no run
are all deliberate: an unclassified failure rolls its claim back (step 7 below), catch-up
collapses a backlog to its most recent member (§3.6), an occurrence older than the catch-up
window is never selected, and a pause beginning after an occurrence falls due but before the
tick that would have fired it discards that occurrence — at any duration, since every tick
inside the pause is skipped and §4.2's cutoff is a wall-clock instant rather than a record of
what was already owed. Liveness is best-effort inside that window; **uniqueness
is absolute**. §3.8's `deployment.occurrences.skipped` observes the first three, but only ever through a
fire that wins a claim to carry the count: loss two has one by construction, since the
collapse *is* a claim, while losses one and three are counted only if some later fire
eventually wins — an occurrence with no later fire at all goes unattributed either way
(§3.6). The fourth it cannot observe by construction:
§3.6 bounds the count below by `max(watermark, schedule_resumed_at)`, and an occurrence the
cutoff discards falls below that bound by definition.

**There is no advisory lock.** An earlier draft took one, on the reaper's model. The reaper
needs its lock because it races `provisionSandbox`, a **non-database** actor holding a
container (`internal/executor/reaper.go:54-63`, `:119-121`); the scheduler races only other
schedulers, over rows, which is what a unique index is for. The lock would also have bought
two failure branches to cover (`pg_try_advisory_lock` returning false, and a failed unlock
forcing the connection closed, `reaper.go:300-313`), a second pinned connection per fire —
the trap the reaper names out loud, *"a second pool acquisition here would deadlock a
deliberately tiny pool"* (`reaper.go:136-137`) — and, if the run row were inserted after the
session, a window in which the losing replica has created a live session that no run row
references.

**The fire, in one transaction:**

1. `BEGIN`.
2. `SELECT 1 FROM deployments WHERE id = $1 AND archived_at IS NULL AND paused_at IS NULL FOR SHARE`. **Zero rows means the deployment was archived or paused after the candidate scan read it** — roll back, return `nil`. The candidate scan and the fire are separate statements, so without this re-read a fire could create a session for a deployment whose archive had already returned 200, and archive is terminal. Archive, pause and unpause take `FOR UPDATE` on the same row, so the two cannot interleave; this is the discipline decision 7 applies to the agent row, applied here to the deployment's own. The cost is latency, and it is one-directional: a fire in flight blocks a concurrent pause or archive for the length of its `createSessionTx`, so those three handlers set a short `lock_timeout` and surface a wedged fire as a failed request rather than an indefinite hang.
3. `INSERT INTO deployment_runs (…, deployment_id, trigger_type='schedule', scheduled_at, agent, session_id=NULL, error=NULL) … ON CONFLICT (deployment_id, scheduled_at) WHERE scheduled_at IS NOT NULL DO NOTHING RETURNING id`. **Zero rows returned means another replica owns this occurrence** — roll back and return a clean `nil`. **The conflict target is named on purpose.** A bare `ON CONFLICT DO NOTHING` swallows *every* unique violation — an id collision, a constraint some later migration adds — and reports each as a lost race, which silently drops a fire that should have been a 500. Naming the target means only the occurrence claim is absorbed and anything else still raises. **And zero rows is not always immediate.** Against a *committed* conflicting row the insert returns zero rows at once; against one a concurrent replica has inserted and not yet committed, Postgres has no snapshot to test and waits on that transaction's id until it ends — which here is the winner's whole fire, `createSessionTx` included. **The fire's own transaction therefore sets a short `lock_timeout`** — step 2 gives one to archive, pause and unpause, which is the other side of this contention and not this side of it — and a `55P03` raised by this statement is read as a lost claim rather than an error: roll back, return `nil`. Being transaction-scoped it covers step 2's `FOR SHARE` too, where a timeout means the same thing by a different route: another writer holds the deployment row, so this tick does not fire it. Without it a loser holds a connection for the winner's entire fire, and §4.4's budget would be short by one connection per replica per contested occurrence.
4. `SAVEPOINT fire`; run `createSessionTx` (slice 2) in the same transaction.
5. On success: `UPDATE deployment_runs SET session_id = … WHERE id = $run RETURNING id`. The settlement **must** return exactly one row; zero aborts the transaction rather than committing a run with both columns null. `COMMIT`.
6. On a **classified** failure (an archived environment, a missing vault, an archived memory store — §5.2 is the inventory): `ROLLBACK TO SAVEPOINT fire` — which discards the half-made session and keeps the claim — then the same one-row-returning `UPDATE … SET error = {type, message}`, auto-pause if the type is one of the fourteen, and `COMMIT`.
7. On an **unclassified** infrastructure failure: roll the whole transaction back. The occurrence stays unclaimed, and a later tick retries it **only while it is still the most recent due occurrence inside the catch-up window** — once a newer occurrence commits a run, the watermark has passed it and it is counted as collapsed (§4.2), not retried forever.

The savepoint is what makes step 6 possible without a second transaction, and a second
transaction is what would let a crash leave a run row with `session_id` and `error` both
null — a shape the reference forbids (`BetaManagedAgentsDeploymentRun`, *"Exactly one of
session_id or error is non-null"*).

**That invariant is enforced by construction, not by the database, and that is a choice.**
One transaction plus a settlement that must affect exactly one row leaves no path that
commits a null/null row; a test asserts it over every branch of §5.2. A database-level
guarantee is possible but costs more than it buys here: `CHECK` cannot be deferred in
Postgres, so the only option is a `DEFERRABLE INITIALLY DEFERRED` constraint trigger, and
one trigger for one invariant on a table with two writers — the scheduler's fire and the
`POST /run` handler, which records a failed manual run the same way (§5.2) — is more
machinery than the risk warrants.

**One path does break it after the commit, and it is not the writers':
[#520](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/520).** `session_id` is
`ON DELETE SET NULL`, so deleting the session nulls the column on a run that has already
settled — leaving a committed row with neither arm, and dropping it out of `last_run_at`'s
`session_id IS NOT NULL` filter so that field silently regresses to an older run or to null.
Nothing observes this in slice 1, which serves no fire and writes no run row, and migration
`0031` is merged and therefore immutable. Whoever writes the fire owns the fix, and it wants
a durable success marker — a `succeeded_at` column in slice 3's migration is the obvious
shape — with the read query and the union arm keyed off that instead of off a link that may
go stale. Restricting session deletion is the other option and is almost certainly wrong:
run history is meant to be readable *"independent of the session lifecycle"*.

No cipher call happens at fire time: a `github_repository` resource's `authorization_token`
is sealed once, at deployment create/update, outside the transaction (§5.1), and the fire
copies ciphertext.

### 4.2 The clock is Postgres's, and the watermark is history

This platform states one rule about time out loud —
`internal/api/memoryretention.go:103-107`: *"The window is a duration the statement
subtracts from the database's own clock, never a timestamp computed here … **every other
age predicate in this platform** compares against `now()` for the same reason."* The reaper
obeys it (`reaper.go:256-259`), and so does the retention delete.

The scheduler obeys it too: **one `SELECT now()` per tick**, fed into the pure occurrence
computation. `now` stays an injectable parameter so tests drive it, and in production the
parameter's value comes from that query, never from `time.Now()`. This is not a nicety.
Sourced from the replica's clock, an hour-fast replica would fire the 10:00 occurrence at
09:00, the unique index would then refuse the honest 10:00 fire, and the correct-clock
replicas could never recover it — one misconfigured host silently shifting the whole fleet's
schedule. With one shared clock that failure class does not exist, and no skew clamp is
needed.

**The watermark is a floor, not a retry queue.** Because it is `MAX(scheduled_at)` over
committed runs, a later occurrence that commits while an earlier one is rolling back moves
the floor past the earlier one, and that earlier occurrence is never retried. Two replicas
firing adjacent occurrences is enough: A claims 10:00 and stalls, B commits 11:00, A rolls
back. This is bounded and counted rather than fixed — the alternative is durable pending
claims, a second lifecycle on a table whose whole value is that it holds settled history —
so §4.1 step 7 says "only while it is still the most recent due occurrence", and the loss
increments `deployment.occurrences.skipped` like any other collapse.

**One column of scheduling state is stored, and only one:** `schedule_resumed_at`, set from
the database clock at create and at every unpause. The candidate predicate requires
`scheduled_at > schedule_resumed_at`. Without it the published unpause rule is
unimplementable: no run row advances the watermark while a deployment is paused, so a
deployment paused at 08:00 and unpaused at 10:05 would immediately fire 10:00 — backfilling
exactly the trigger *"Unpause resumes the schedule from the next scheduled occurrence.
Missed triggers are not backfilled"* forbids (§3.6). The same column stops a newly created
deployment from firing occurrences that predate it.

**No other derived state is stored on the deployment row.** Both `last_run_at` (§3.3 — the
`created_at` of the row the index's greatest `scheduled_at` selects) and the
due-watermark (`MAX(scheduled_at)` over *all* scheduled runs, which is what stops a failed
occurrence being retried every tick) are computed from `deployment_runs` in the candidate
query, via a `LEFT JOIN LATERAL` over the unique index the plan adds anyway — the watermark
reaching it through Postgres's MIN/MAX rewrite, `last_run_at` only because §3.3 spells out
the `scheduled_at IS NOT NULL` the partial predicate needs. Two denormalized
columns would be a second source of truth for facts the run table already holds, and any
path that wrote a run row and forgot the watermark — or committed them in different
transactions — would desynchronize a schedule from its own history, with the symptom being a
deployment that re-fires or never fires again while the run list says otherwise. If profiling
later demands the denormalization, it is added then, with the run insert as its only writer,
in the same statement.

### 4.3 Rejected: a `visible_at` primitive on `work_items`

A delayed-work column on `work_items` is genuinely wanted — by plan 35's `wait_for_agents`
sweeper, not by this. Adding it here would touch the platform's hottest table and its
`ON CONFLICT` dedup index for a feature whose uniqueness story is already a unique index
on a table two handlers write.

### 4.4 Concurrency, connections, and a mixed-version fleet

A fire is a full session creation — `resolveAgent`, a `FOR SHARE` on the environment row
(`internal/api/sessions.go:583`), the resource materialization and the initial-event append.
Thirty of them serialized behind one tick will overrun the 30-second interval (§3.4). The tick
therefore fires with a small bounded concurrency (a constant, with the reason in its
comment), and `deployment.tick.duration` is the instrument that says when the constant is
wrong.

The connection budget is **one per in-flight fire, plus one for each replica briefly losing
a claim** — a loser blocks on the winner's transaction id and holds its connection until its
`lock_timeout` expires (§4.1 step 3), which is why that timeout is short and why the budget
is not simply the concurrency constant. Beyond those two there is nothing, now that the
advisory lock is gone. There is no `pool_max_conns` floor to
raise on the control plane: `cmd/controlplane/main.go` contains zero `MaxConns` references.
If the bounded concurrency is set above a small number, the explicit startup check
`cmd/executor/main.go:288-297` models is the pattern to copy — refuse a too-small pool at
startup rather than wedging silently under load.

**A rolling upgrade is safe by construction.** Migrations `0031` and `0032` are
additive-only, so an old replica still serving requests is unaffected, and an old replica
simply has no scheduler — the new replicas' fires are claimed through the unique index the
old one never touches. (#468 records the separate hazard of two *releases* sharing one
`externalDatabase.url`; that is unchanged here.)

---

## 5. The API surface

### 5.1 Routes, roles, and shapes

Ten registrations, each carrying an `identity.Role` the way every route in
`internal/api/server.go:50-69` does. `POST /run` takes `RoleDeveloper` **because it does
session-create's work**, not because it looks like a read:

| Operation | Role |
|---|---|
| `POST /v1/deployments` | `RoleDeveloper` |
| `GET /v1/deployments` | `RoleViewer` |
| `GET /v1/deployments/{id}` | `RoleViewer` |
| `POST /v1/deployments/{id}` (update) | `RoleDeveloper` |
| `POST /v1/deployments/{id}/archive` · `/pause` · `/unpause` | `RoleDeveloper` |
| `POST /v1/deployments/{id}/run` | `RoleDeveloper` |
| `GET /v1/deployment_runs` · `/{id}` | `RoleViewer` |

- **Unknown keys** are refused by `rejectUnknownKeys` on create and update, the
  `createSession` precedent (`internal/api/sessions.go:517-520`) — which is how `budget`
  becomes a 400 (§8.1 entry 2).
- **The four action endpoints ignore the request body entirely**: the reference declares no
  `requestBody` and its SDK params carry only the beta header (§2.1), so there is nothing to
  validate and a body is neither read nor refused. `?beta=true`, `anthropic-version` and
  `anthropic-beta` are already accepted and ignored globally (`internal/api/doc.go:16`), so
  the beta surface costs this plan nothing.
- **`metadata` is a patch on update** (`:28419`) — upsert on a string, delete on a null,
  preserve on omission — while `vault_ids`, `initial_events`, `resources` and `schedule` are
  full replacements. The 16/64/512 bag limits apply to the *stored* result, not the patch.
- **Repo tokens are sealed at deployment create and update**, before the transaction opens,
  because the cipher does a network round trip — the exact discipline
  `internal/api/sessions.go:538-549` states (*"Seal before the transaction opens — the
  cipher's network round trip must not run under the session/environment row locks"*). A
  `github_repository` resource on a cipher-less deployment is refused with
  `errRepoSecretsUnavailable`, plan 25 decision 2's *"never stored unencrypted"*, exactly as
  `POST /v1/sessions` refuses it. `resources` echoes back minus credentials (`:23051`).
- **`user.define_outcome` in `initial_events` is validated at deployment-create time**, not
  at every fire: a file rubric on a deployment whose platform has no object storage is
  refused with the existing message *"file rubrics require the files surface, which this
  deployment does not configure"* (`internal/api/events.go:728`). Refusing once beats
  failing every nightly fire. Grading on a `self_hosted` environment stays transcript-only,
  the standing behavior of `docs/DIVERGENCES.md:102`.
- **Archive is terminal for every mutating action.** `POST /v1/deployments/{id}` (update),
  `/pause`, `/unpause` and `/run` all answer 400 `deployment %s is archived`; `GET` and the
  list (with `include_archived`) still return the row. Leaving update unspecified would
  leave an implementer to invent whether hidden configuration stays mutable on a row nothing
  can ever fire. The reference publishes a status code for none of the four: entry 11 registers
  `/run`, which lands with it in slice 3, and entry 29 the other three, which land in
  slice 1.
- **Archived deployments render `status: "active"`, `paused_reason: null`,
  `upcoming_runs_at: []`, `last_run_at` preserved.** That is forced by composing two
  published sentences — *"Archived deployments report `active` with `archived_at` set"*
  (`:23076`) and *"Non-null exactly when status is paused"* (`:23081`) — with `:22765` and
  `:22760`. Archive writes `archived_at` and leaves the pause columns as they are; nothing
  can ever unarchive, so nothing can observe them again.
- **The deployments list** rejects `status` combined with `include_archived` with a 400,
  because `:13229` publishes the rule; excludes archived rows by default; offers `gte`/`lte`
  only, through the existing `parseTimeParam` idiom (`internal/api/agents.go:397`).
- **The runs list** needs `maxRunLimit = 1000` beside `maxEventLimit` in
  `internal/api/page.go:21-33`. The shared `maxLimit = 100` would 400 a legal `limit=500` in
  the exact place a "did my schedule fire?" client pages. It also carries `trigger_type`,
  `has_error` and all four `created_at` comparators (the `internal/api/events.go:814`
  four-arm precedent), and answers 200-with-empty-data for a shape-valid unknown
  `deployment_id` (`:14195`).

### 5.2 The fire's failure branches

Each classified failure writes a run row with an error and, for the fourteen pausing types,
sets the deployment's pause columns. Two published constraints shape the mapping:

- **The `message` is dropped on the pause side.** Run errors are
  `required: ["type","message"]`; paused reasons are `required: ["type"]` and carry no
  message field at all (§2.5). `paused_reason.error` is type-only.
- **Only a scheduled fire auto-pauses.** *"**A scheduled fire** recorded a failed run whose
  error auto-pauses the deployment"* (`:23573`). A failed `POST /run` writes its run row and
  leaves the schedule alone.

| Cause | Run error type | Pauses? |
|---|---|---|
| Environment archived (`sessions.go:591`) | `environment_archived_error` | yes |
| Environment row gone | `environment_not_found_error` | **unreachable** — the FK refuses the delete (§5.3), so no stored deployment can observe it (§8.1 entry 26) |
| Agent version archived (`sessions.go:310`) | `agent_archived_error` | yes, and only for a roster member (§8.1 entry 18) |
| Vault missing / archived (`sessions.go:435`) | `vault_not_found_error` / `vault_archived_error` | yes |
| Memory store archived (`memorystores.go:200`) | `memory_store_archived_error` | yes |
| File resource gone | `file_not_found_error` | yes |
| Other resource gone | `session_resource_not_found_error` | yes |
| Anything unclassified | **not recorded** — the transaction rolls back whole and the tick retries; the reference's `unknown_error` is deliberately not emitted (§8.1 entry 28) | — |

**Seven** of the fourteen pausing types are produced by no path above, each for its own
reason and each registered: `environment_not_found_error` (the FK, entry 26),
`workspace_archived_error` and `organization_disabled_error` (scope reserved but not
enforced, entry 6), `mcp_egress_blocked_error` (the address guard runs at dial time inside
the executor, entry 5), `self_hosted_resources_unsupported_error` (accepted and fired,
materializing nothing, entry 22), `skill_not_found_error` (the paragraph below, entry 21),
and `unknown_error` (the row above, entry 28). Add the two the pausing union omits (§2.5) —
`session_rate_limited_error`, which has no cause here at all (entry 4), and
`session_creation_rejected_error`, whose cause the table routes through a full rollback
with no run row (entry 14) — and that is **nine of the sixteen** run-error types with no
emitter, which is why §7 asserts the `paused_error_type` mapping on two properties that can
fail rather than against a copy of itself.

`skill_not_found_error` is in both unions and is **not** obviously unreachable here, unlike
the multi-tenant variants: skills are implemented (`skill_`/`skillver_` prefixes,
`internal/skills/`, the `/v1/skills` routes). The fire validates exactly what
`POST /v1/sessions` validates and no more, and session create does not resolve an agent's
skill references — so a missing skill surfaces inside the running session, not as a failed
run, and the type is produced by nothing. Registered (§8.1 entry 21).

A **failed manual run is a 200 carrying an error-bearing run**, not an HTTP error: the
endpoint's only success response is `BetaManagedAgentsDeploymentRun` (`:14051-14054`) and
that object's `error` is required-and-nullable. The one HTTP error we add is the
archived-deployment 400 (§8.1 entry 11).

### 5.3 Two existing handlers this plan must change

- **`DELETE /v1/environments/{id}` reports a lie once deployments exist.**
  `internal/api/environments.go:523-527` maps **any** `23503` to *"environment %s still has
  sessions; delete them first"*. With an FK from `deployments.environment_id`, an operator
  deleting an environment whose only referent is an archived deployment is told to delete
  sessions that do not exist — and since archive is terminal and we ship neither
  `DELETE /v1/deployments` nor unarchive, nothing clears it. Slice 1 branches on
  `pgErr.ConstraintName`, the pattern `createSession` already uses
  (`internal/api/sessions.go:650-655`), and names the deployments. With a test.
- **`POST /v1/agents/{id}/archive` starts refusing** (§8.3 decision 7): a 400 naming the
  deployments, whenever a deployment with `archived_at IS NULL` pins the agent. An
  *archived* deployment never blocks — it is terminal and can never fire, and blocking on one
  would build exactly the dead end the environment-delete bullet above describes. Teardown
  becomes ordered: archive the deployments, then the agent.

  It is a single `UPDATE … RETURNING` on the pool today with no transaction
  (`internal/api/agents.go:578-584`), and it still becomes a transaction as its own
  behavior-neutral step — not for a cascade now, but for the check: between "no deployment
  pins this agent" and the archive write, a concurrent write could add one. The archive takes
  `FOR UPDATE` on the agent row; **both deployment create and deployment update take
  `FOR SHARE` on the agent they pin**, and update rechecks that it is not archived while
  holding the lock. Create alone is not enough — `agent` on update *"Cannot be cleared"* but
  can be repinned, so an update racing the archive would leave a live deployment pinning an
  archived agent and make entry 18's claim false. This is the discipline
  `internal/api/sessions.go:583` already applies to the environment row. There is
  no version-level archive route to consider — `POST /v1/agents/{id}/archive` is the only one
  (`internal/api/server.go:55`), and `resolveAgent` refuses an archived agent wholesale
  (`internal/api/sessions.go:309-310`).

### 5.4 Cascade and dangling — the agent is the exception, by refusal

The reference's own paused-reason list is the evidence that it cascades for nothing: it
pauses at fire time instead, with
`environment_archived_error`, `environment_not_found_error`, `vault_not_found_error`,
`vault_archived_error`, `memory_store_archived_error`, `file_not_found_error` and
`session_resource_not_found_error` all in the fourteen (`:23193-23206`). We follow that for
every resource. The agent is the one place we do something the reference does not, and it is
a refusal rather than a cascade (decision 7). So:

| Operation | Effect on deployments pinning the resource |
|---|---|
| `POST /v1/agents/{id}/archive` | **refused** while any non-archived deployment pins the agent, with a message naming them (decision 7) |
| `POST /v1/environments/{id}/archive` | nothing; the next fire records `environment_archived_error` and pauses |
| `DELETE /v1/environments/{id}` | refused by the FK, with a message that names the deployments (§5.3) |
| Vault archive · memory-store archive · file delete | nothing; the next fire records the matching type and pauses |

---

## 6. Data model

Two migrations, both additive, named here so two parallel worktrees do not both claim
`0031` (which is what it landed as; CLAUDE.md assigns the number when the file enters the
repo):

- **`0031_deployments.sql`** (slice 1) — `deployments` and `deployment_runs`.
- **`0032_session_deployment_id.sql`** (slice 3) — `sessions.deployment_id` and
  `sessions_deployment_idx`.

Six conventions the compiler will not enforce:

- **`deployments` carries the reserved scope columns** `org_id` / `workspace_id` /
  `project_id` as `text NOT NULL DEFAULT 'default'`, with the comment
  `0028_memory_stores.sql:8-12` models — it is a top-level resource table and
  `internal/store/store.go:34-42` states the rule. `deployment_runs` inherits scope through
  its FK and omits them. The reserved columns are also what make `workspace_archived_error`
  and `organization_disabled_error` admissible values rather than nonsense (§8.1 entry 6).
- **`deployments.created_by text`**, audit-only and never on the wire, as
  `0001_init.sql:82-85` and `0028_memory_stores.sql:17-19` carry it. It matters more here
  than usual: a fired session's `created_by` is NULL (§9), so the deployment row is the only
  place the audit trail survives.
- **The unique index is partial and is not `CONCURRENTLY`**:
  `UNIQUE (deployment_id, scheduled_at) WHERE scheduled_at IS NOT NULL`. Manual runs have no
  `scheduled_at`, and while Postgres admits unlimited NULLs in a plain unique index, the
  partial form spells the intent. `CREATE INDEX CONCURRENTLY` is forbidden outright —
  migrations run inside one transaction (`internal/store/store.go:14-19`).
- **`scheduled_at` is `timestamptz`**, stated in the DDL comment with its reason: §3.2's
  fall-back rule produces two rows only if the column holds two distinct UTC instants. Stored
  as a local wall clock the second 01:30 collides on the unique index and is silently
  swallowed — a wrong answer that looks exactly like a right one. §7 asserts the catalog type
  directly, because the behavioral test alone cannot prove it. The table also carries
  `created_at` — a required field on the wire resource, and the instant §3.3's `last_run_at`
  reads.
- **The run list gets two indexes and no more** — one for each of its two scans, since
  `deployment_id` is an optional filter (*"omit to list across all deployments"*) and no
  single index serves both: a leading `created_at` cannot restrict the filtered scan, and a
  leading `deployment_id` cannot order the unfiltered one. So `(created_at DESC, id DESC)`
  and `(deployment_id, created_at DESC, id DESC)`. This bullet originally reasoned that the
  unique index would serve the filtered listing on its own, which is wrong twice over: that
  index is **partial** on `scheduled_at IS NOT NULL`, so it does not cover manual runs at
  all, and it orders by `scheduled_at` rather than by the list's own key. §2.6's remaining
  filters — `trigger_type` and `has_error` — still get nothing, and that part stands: they
  are low-cardinality and the composite already bounds the scan.
- **The DDL comment states the pruning constraint** the run table's double duty creates: the
  row is history *and* the occurrence claim *and* the watermark, so deleting one newer than
  the catch-up window un-claims an occurrence a tick can then fire again (§9).

`deployments` stores `paused_at`, `paused_kind` (`'manual' | 'error'`) and
`paused_error_type` under a `CHECK` admitting all fourteen values; `status` and
`paused_reason` are rendered from them. It stores exactly one piece of scheduling state,
`schedule_resumed_at timestamptz NOT NULL` — written from the database clock at create and
at every unpause, and the reason the published no-backfill-on-unpause rule is implementable
(§4.2). It stores **no** `last_run_at` and no watermark (§4.2).

---

## 7. Testing

**The seams.** Two exported test hooks, each returning a `restore` — the shape every seam in
`internal/api/export_test.go` uses (`AllowLoopbackProbeForTest:14-23`,
`SetPingIntervalForTest:52-56`, `SetMemoryPruneIntervalForTest:66-73`). An exported
`*time.Duration` would have no restore, and `internal/api` is a single test binary running
the whole suite, so a 20 ms tick interval set by one test would leak into every later one:

```go
// internal/api/export_test.go
func SetDeploymentTickIntervalForTest(d time.Duration) (restore func())
func SetDeploymentCatchupWindowForTest(d time.Duration) (restore func())

// SchedulerTick runs exactly one tick against the pool at the given instant.
// The scheduler's loop is a ticker calling this, so a test that drives `now`
// covers every branch without a wall clock. In production `now` is the tick's
// own SELECT now() (§4.2), never time.Now().
func SchedulerTick(ctx context.Context, pool *pgxpool.Pool, now time.Time) error
```

`SchedulerTick` takes the **pool**, not the handler. `NewHandler` returns
`withRequestID(withTracing(dispatchAuth(pool, verifier, mux)))` (`internal/api/server.go:257`);
the wrappers are anonymous `http.HandlerFunc` closures and the `*server` holding `pool` is
unexported and unreachable by any type assertion (`server.go:22-33`, `:46-47`). The test
helper already holds the pool directly (`internal/api/apitest_test.go:73-80`). Context
first, the convention every function in this repo follows (`reaper.go:110`,
`memoryretention.go:108`).

**One wall-clock test.** `TestSchedulerTickerRuns` sets the interval to 20 ms, seeds a
`* * * * *` deployment with an occurrence an instant in the past, waits on a **polled
condition, not a sleep**, and asserts one run row. It proves the ticker is wired; every
other scheduler assertion drives a fixed `now`.

**The multi-replica test drives *different* instants.** Two goroutines pinned to the *same*
`now` cannot catch the bug that matters — a `scheduled_at` derived from the observing clock
(truncated to the tick boundary, or taken from `now`, or from the watermark) rather than from
the cron occurrence — because that bug passes such a test every time and produces duplicate
runs in production. So: drive the callers at `T-ε`, `T+2s` and `T+59s`, and assert **one run
row whose `scheduled_at` equals the exact cron instant**. That assertion is what pins
`scheduled_at` to the expression. Add a case where the second caller's tick begins before
the first's transaction commits, using the `reapHookAfterClassify` idiom
(`internal/executor/reaper.go:64-69`). Assert both callers return `nil` — the loser's
`ON CONFLICT DO NOTHING` returns zero rows and never a `23505` escaping as a 500.

**The failed fire is its own case**, because it is where the three published rules §3.3
derives from one expression can silently come apart from the one clause no source confirms.
`TestSchedulerTickFireFails` points a due deployment at an archived environment and asserts,
after one tick: a committed run row with `session_id` null and
`error.type = "environment_archived_error"`; `last_run_at` **still null**, which is the
INFERRED half of the expression (§8.1 entry 13); `paused_kind = 'error'` with
`paused_error_type` carrying the same value. It deliberately does **not** drive a second tick
to prove the claim survived: the same case leaves the deployment paused, so §4.1 step 2's
candidate scan skips it on the next tick whether or not the claim survived, and the assertion
would pass for the wrong reason. The committed row *is* the claim (§6's DDL comment), which
the first tick already asserts. A variant whose failure is *unclassified* asserts the opposite shape: the transaction
rolls back whole, no row survives, and the next tick fires the same occurrence again. The
pair is what pins §4.1's savepoint boundary, which neither case alone can.

**DST.** Fixed-zone contract tables for `America/New_York` and `Australia/Lord_Howe` (a
30-minute offset, which catches arithmetic that assumes whole hours). The fall-back case
asserts **two rows with different `scheduled_at`**. That behavioral assertion is necessary
but not sufficient: a driver can round-trip two distinct instants through a
`timestamp without time zone` column without colliding, so the migration test also asserts
the catalog type — `information_schema.columns.data_type` for `deployment_runs.scheduled_at`
is `timestamp with time zone` — which is the only check that actually pins the DDL. Test against transitions whose rules are decades old rather
than a freshly-changed zone, so a host with slightly stale tzdata does not produce a
spurious red.

**The property test** ties `Due`, `Next` and `Upcoming` to one implementation, so
`upcoming_runs_at` and the instant we fire cannot disagree.

**The `paused_error_type` mapping is asserted on two properties that can fail**, not against
a literal copy of itself — this repo names that anti-pattern in its own build, *"an
assertion that cannot fail is a comment"* (`Makefile:416-417`), and it is worse here because
seven of the fourteen values are produced by nothing (§8.1 entries 5, 6, 21, 22, 26 and
28 — §5.2 does the accounting, and entry 6 covers two of the seven). The two
properties: every value in the Go mapping is admitted by the migration's `CHECK` (queried
through `pg_get_constraintdef`), and every reachable failure path in §5.2 produces a value
in the mapping — one test per reachable path.

**One `acceptance/` case, driving the SDK.** The earlier draft skipped the suite on the
grounds that it is doc-example-shaped. That is true of `acceptance/dcf_test.go:22-24` and
false of the suite: `acceptance/stack_test.go:1-45` is an in-process full-stack rehearsal
wiring `api` + `brain` + `executor` + `queue` + a scripted provider + real Docker behind
`httptest`. And `dcf_test.go:14-15` and `live_test.go` drive our server with the **real
`anthropic-sdk-go` client**, which already ships `betadeployment.go`. A brand-new resource
family whose entire justification is wire compatibility would otherwise get zero executable
proof that the typed SDK can drive it, with a hand-pasted `ant` transcript — which cannot
regress — as its only end-to-end evidence. The case drives `Deployments.New` / `Get` /
`List` / `Run` and stops at the run record. "No model call, no Docker" is not automatic:
`initial_events` is required, and session creation enqueues a `queue.ModelTurn` in the same
transaction, while `newStack` starts the brain and executor loops — so the case builds its
stack **without those two consumers**, or drains the queued turn before asserting. Otherwise
the assertion races a model call it did not intend to make.

**No new eval.** A deployment is control-plane-only; the model never sees one. Plan 36's
slices 1-3 (also pure CRUD) added none, and its slice 4 added two only because memory
changed what the model could do.

**Coverage.** `internal/cron` enters the ≥90% denominator automatically — it is not a
`*test` support package, so the `test` recipe's exclusion expression
(`Makefile:82-86`) needs no edit — and should land near 100%: pure functions, a table per
branch, no I/O to stub. The drag risk is `internal/api`'s fire failure branches (§5.2), each
of which gets its own test.

**Two operational cautions carried from the environment.** `internal/api` already runs about
9.5 minutes under the WSL gate against the suite's 30-minute ceiling (#490, #499), and three
of six slices add to it — prefer table cases inside one `newTestServer` over a new server per
case. And a timed-out run leaks `pgtest` fixture containers that sink the next run; sweep
before re-running.

---

## 8. Divergences and open questions

### 8.1 Entries for `docs/DIVERGENCES.md`

**The entries below are bullets carrying an explicit `Entry N` label, not an ordered list.**
The numbers are registry identifiers cross-referenced throughout this document, and they
are deliberately non-sequential — entry 10 never existed and entry 20 was deleted when
decision 10 was settled. Under CommonMark only a list's first marker sets its start and the
rest are renumbered, so an ordered list would render "entry 21" as *8* while every
cross-reference still said 21. Bullets keep the identifier and the rendered text agreeing.

Format per the file's own legend at `docs/DIVERGENCES.md:10`. The head segment of `Tracked:` before the first
top-level `;` must name an **open** issue; every INFERRED entry requires a live tracker, and
where that tracker is shared — #78 above all — it carries a parenthetical naming what *this*
entry leaves open. Each entry lands in the PR that introduces its behavior, which is also where its evidence
is rewritten from spec line numbers into schema names (§2).

**Rewrites of existing entries:**

- **`:43`** — **edited, not deleted**: it covers `stats` *and* `deployment_id` together, and
  `stats` stays stubbed. New text covers `stats` alone.
- **`:47`** — rewritten in place, the move the `memory_store_id` filter entry already made:
  `- **GET /v1/sessions list filter deployment_id — now a real filter (plan 37 slice 3)** — The filter no longer short-circuits to an empty page: sessions carry an optional, nullable deployment_id — written only by a deployment fire, emitted as an explicit null when unset, which the reference's optional key admits — and the filter is a keyset predicate over sessions_deployment_idx. A shape-valid unknown id is a 200 with an empty page, the rule the reference publishes for the runs list and we apply to both; a shape-invalid id is a 400, which the reference publishes for neither. *Evidence: anthropic-openapi.yml:9615 (the sessions-list param), :14195 ("Filtering by a non-existent deployment_id returns 200 with empty data"), :26099-26116 and :26236-26238 (deployment_id is the Session object's only non-required property); anthropic-sdk-go betasession.go:1257; internal/api/sessions.go listSessions; internal/api/sessiondeployment_test.go.* *(no tracker: delivered)*`
- **`:54`** — the v1.62.0 "registered, not built" entry says deployment `budget` *"fall\[s]
  under the deployment/webhook absences already recorded"*. Deployments now exist, so that
  clause is false; rewrite it to point deployment `budget` at #432 directly.

**New CONFIRMED entries:**

- **Entry 1.** `- **Deployment and deployment-run webhooks — the nine deployment.* / deployment_run.* events are never delivered (plan 37 slice 1)** — The reference emits deployment.created / .updated / .paused / .unpaused / .archived / .deleted and deployment_run.started / .succeeded / .failed, each carrying only {type, id, organization_id, workspace_id}; this platform has no webhook subsystem at all, so none of the nine is delivered. Whatever delivers session.outcome_evaluation_ended will deliver these nine by the same machinery. *Evidence: anthropic-sdk-go betawebhook.go:180 and :272 (two of the nine data types); anthropic-openapi.yml:35175-35179 and :35181-35184 (the nine discriminator entries; :35180 between them is agent.updated), :34909-34919 (the four-field payload, all required); a full-tree grep of this repo finds no webhook code.* *Tracked: #261 (the webhook subsystem itself; the deployment family is the second event group waiting on it, not the first).*`

- **Entry 2.** `- **POST /v1/deployments and the update route reject budget (plan 37 slice 1)** — The reference stamps a deployment's spend ceiling onto each session it starts; this platform has no session budgets, so budget is absent from both allow-lists and a request carrying it is a 400 unknown field, exactly as POST /v1/sessions answers today. This one breaks a real client path rather than an obscure field: the ant CLI exposes --budget, --budget.max-list-cost and --budget.type on both beta:deployments create and beta:deployments update, so those invocations 400 against this platform. Storing and echoing an unenforced cap would be worse: an operator would believe a ceiling exists. The Deployment object never emits the key, which is the reference's own shape for an unset budget — the one property outside its 16-entry required list, "Absent when no budget is set". *Evidence: anthropic-openapi.yml:22337-22338 (create), :28432-28433 (update), :22986-23003 and :23088-23091 (optional, absent when unset); anthropic-cli pkg/cmd/betadeployment.go:45-49 and :127-135 (create flags), :241-243 and :311-319 (update flags); internal/api/sessions.go rejectUnknownKeys.* *Tracked: #432.*`

- **Entry 3.** `- **Scheduled fires apply no jitter (plan 37 slice 4, decision 6)** — The reference offsets each fire by a small per-schedule jitter to spread a multi-tenant fleet's load. A self-hosted single-organization deployment has nothing to spread, so we fire at the first tick that sees an occurrence due, which lands 0-30 seconds after it and satisfies the reference's own published claim that a run starts "at or shortly after its listed time". scheduled_at still records the exact pre-jitter cron match, so the wire value is identical either way. *Evidence: anthropic-openapi.yml:22765 ("Each fire is offset by a small per-schedule jitter, so a run will actually start at or shortly after its listed time"), :25955 (scheduled_at is the pre-jitter instant); platform.claude.com managed-agents/scheduled-deployments (fetched 2026-08-27 — the 15% / 5-second / 9-minute quantification).*`

- **Entry 4.** `- **session_rate_limited_error is never recorded (plan 37 slice 4)** — The reference throttles session creation and records the rejection as a failed run that does not pause the schedule. This platform imposes no session-creation rate limit at all, so the type's only cause never arises and nothing produces it; it is admitted by the store so a future emitter needs no migration. An earlier draft reused the type to mark a scheduled fire skipped for overlap, which decision 10 declined — this platform installs no overlap brake, so there is no second cause either. *Evidence: anthropic-openapi.yml:26657-26666 ("Session creation was rejected due to rate limiting. The schedule keeps firing; subsequent runs may succeed."), :25892-25907 and :23193-23206 (the type is one of the two the pausing union omits); no rate limiter exists in internal/api.* *Tracked: #46 (per-org rate limiting on the management API; when it lands, this type gains its reference cause).*`

- **Entry 5.** `- **mcp_egress_blocked_error is never recorded at fire time (plan 37 slice 4)** — The reference fails a run when an MCP server host the agent uses is blocked by the environment's network policy. This platform's address guard runs at dial time inside the executor, not at session creation, so a blocked host surfaces as a tool error inside a running session rather than as a failed deployment run. *Evidence: anthropic-openapi.yml:24904-24920 ("An MCP server host used by the deployment's agent is blocked by the environment's network policy"); internal/dialguard (its three live call sites are the vault probe, the MCP client's DefaultClient and the token-endpoint refresh — none of them session creation).*`

- **Entry 6.** `- **workspace_archived_error and organization_disabled_error are never recorded (plan 37 slice 4)** — Both describe a scope this platform reserves but does not enforce: org_id / workspace_id / project_id carry single-tenant defaults on every top-level table, deployments included, and no query filters on them. The store admits both values so a future emitter needs no migration. *Evidence: anthropic-openapi.yml:29161 and :25626 (the two run-error schemas), :23193-23206 (both in the pausing fourteen); internal/store/store.go:34-42 (reserved, not enforced).* *Tracked: #56.*`

- **Entry 7.** `- **The 1,000-scheduled-deployments-per-organization cap is not enforced (plan 37 slice 1)** — The reference caps a customer at 1,000 scheduled deployments and directs the rest to support. A self-hosted operator owns their own capacity, and a limit we cannot tune for them is a limit that only ever fails a legitimate request. The cap appears in neither the spec nor the SDK. *Evidence: platform.claude.com managed-agents/scheduled-deployments (fetched 2026-08-27 — "A maximum of 1,000 scheduled deployments is supported per organization."); anthropic-openapi.yml (no maximum on any deployment count).*`

- **Entry 21.** `- **skill_not_found_error is never recorded (plan 37 slice 4)** — Unlike the multi-tenant variants this one has a live surface here: skills are implemented, so an agent's skill reference can genuinely be missing. The fire validates exactly what POST /v1/sessions validates and no more, and session create does not resolve an agent's skill references — so a missing skill fails inside the running session's materialization rather than as a failed deployment run, and the type is produced by nothing. *Evidence: anthropic-openapi.yml:27461 (BetaManagedAgentsSkillNotFoundRunError), :23202 (in the pausing fourteen); internal/api/sessions.go createSession (no skill resolution); internal/executor/telemetry.go (skills materialization outcomes, which is where a missing skill surfaces).*`

- **Entry 22.** `- **A deployment configuring resources against a self_hosted environment is accepted and fires (plan 37 slice 1)** — The reference pauses such a deployment with self_hosted_resources_unsupported_error, because a self-hosted environment "cannot mount them". This platform already accepts a superset at session level — file and github_repository alongside memory_store on self_hosted — so the precondition the reference pauses on is not one this platform holds, and the type is produced by nothing. The consequence is real and worth stating: a github_repository on a self_hosted deployment materializes nothing and the agent is not told, so a schedule can produce a session whose repository is absent, every night. That is the standing no-mount of #322 under a timer, not a new gap. *Evidence: anthropic-openapi.yml:26006-26021 (both variants, "The deployment configures resources, but its environment is self-hosted and cannot mount them"), :23205 and :25906 (in both unions); docs/DIVERGENCES.md:127 (the accepted superset) and :104 (the silent no-mount).* *Tracked: #322.*`
- **Entry 25.** `- **Archiving an agent is refused while a deployment pins it, where the reference cascades (plan 37 slice 1, decision 7)** — A docs-page sentence says an archived agent archives its deployments in the same operation. We refuse instead: POST /v1/agents/{agent_id}/archive answers 400 naming the deployments whenever one with archived_at IS NULL pins the agent, and the operator archives those first. The reference behavior we diverge from rests on that single docs sentence — the operation carries summary "Archive Agent" and no description, a spec-wide grep for a cascade finds nothing, and no deployment schema mentions it — so what is confirmed here is our behavior, not theirs. The trade is deliberate: a cascade destroys every schedule pinning an agent in one unrecoverable step, since this platform ships neither unarchive nor DELETE /v1/deployments, while a refusal is recoverable by retrying in the right order. It also makes agent_archived_error unreachable for the deployment's own agent by construction rather than by convention (entry 18). *Evidence: anthropic-openapi.yml (the agent archive operation, summary only, no description); platform.claude.com managed-agents/scheduled-deployments (fetched 2026-08-27 — the cascade sentence); internal/api/agents.go:578-584 (the single UPDATE this converts to a transaction); internal/api/sessions.go:583 (the FOR SHARE discipline the check reuses); internal/api/environments.go:523-527 (the dead end an archived-deployment block would rebuild).* *Tracked: #78 (archive an agent that a deployment pins, and read both the agent and the deployment).*`

- **Entry 26.** `- **environment_not_found_error is never recorded (plan 37 slice 4)** — The reference fails a run when the deployment's environment row is gone. Here it cannot be: deployments.environment_id carries a foreign key, and DELETE /v1/environments refuses while any deployment references it — with a message naming them, which is the other half of this plan's change to that handler. An archived environment is a different type and is reachable. The store admits the value so a future emitter needs no migration. *Evidence: the OpenAPI spec's BetaManagedAgentsEnvironmentNotFoundDeploymentPausedReasonError; internal/api/environments.go:523-527 (the delete refusal this plan makes name deployments); migration 0031_deployments.sql (the FK).* *Tracked: #78 (delete an environment a deployment pins, on the reference, and read the error).*`


- **Entry 28.** `- **unknown_error is never recorded, and an unclassified fire failure records nothing at all (plan 37 slice 4)** — Both published unions carry unknown_error as an explicit fallback variant — "An unknown or unexpected error caused the run to fail" on the run side, "An unrecognized error auto-paused the deployment" on the pause side — so the reference's answer to an unclassified fire failure is a recorded run that pauses the schedule. We roll the whole transaction back instead: the claim is released and a later tick retries the same occurrence — but only while it is still the most recent due one, which on a */5 schedule is the five minutes until the next occurrence falls due, not the hour the catch-up window suggests. After that the occurrence is counted as collapsed rather than retried, so a persistent fault abandons every occurrence it touches. The trade is deliberate. unknown_error is one of the fourteen pausing types, so emitting it would auto-pause a healthy deployment on a transient database blip, and this platform ships no auto-resume — an operator would have to unpause by hand. A rollback recovers unattended — the deployment, at least, if not always the occurrence. The cost is that the failure leaves no database trace whatever: no run row, last_run_at unmoved, no paused_reason, nothing in either list surface. The only record is telemetry: deployment.fires increments with outcome="abandoned" and the deployment.fire span is emitted either way. The store admits the value under the same CHECK as the other thirteen, so a future emitter needs no migration. *Evidence: anthropic-sdk-go betadeploymentrun.go:798-800 and betadeployment.go:1726-1728 (both fallback variants, both documented as fallbacks); no auto-resume exists — of the eight routes on /v1/deployments only POST /unpause clears the pause columns, and there is no timed or automatic counterpart.* *Tracked: #78 (make a fire fail unclassifiably on the reference, then read the run and the deployment).*`

**Architecture & compatibility notes (not divergences)** — two entries belong in that
section rather than among the mismatches:

- **Entry 8.** `- **No DELETE on /v1/deployments, mirroring the reference's own absence (plan 37 slice 1)** — The reference publishes a deployment.deleted webhook but exposes no delete path: enumerating spec.paths shows post/get on /v1/deployments, get/post on /v1/deployments/{id}, and post alone on archive, pause, unpause and run — no delete method anywhere — and BetaDeploymentService has no Delete. Archive is the terminal verb here as it is there, and unarchive exists in neither. *Evidence: anthropic-openapi.yml:13048, :13355, :13624, :13755, :13886, :14017 (the six paths and their methods); anthropic-sdk-go betadeployment.go:44-176 (New/Get/Update/List/Archive/Pause/Run/Unpause).*`

- The scheduler's at-most-one-run-per-occurrence guarantee across replicas is the partial unique
  index on `(deployment_id, scheduled_at)` — the reference's own published idempotency key —
  and not leader election, which this platform has nowhere. This sentence goes in
  **`docs/ARCHITECTURE.md`**'s process-topology row beside the sentence it falsifies, not in
  the registry: `docs/DIVERGENCES.md:3-9` scopes that file to divergences from the reference
  and inferences about it, and our own process topology is neither.

**New INFERRED entries** (each carries #78 with a parenthetical, per the shared-tracker
rule):

- **Entry 9.** `- **Catch-up after downtime — at most one occurrence, inside a one-hour window (plan 37 slice 4, decision 2)** — Nothing in the spec, the SDK, the CLI or the docs describes what a scheduled deployment does about occurrences missed while the platform was down; a spec-wide grep for catch-up / backfill / misfire returns nothing. The one adjacent published rule is about pause: "Unpause resumes the schedule from the next scheduled occurrence. Missed triggers are not backfilled." We generalize it: a tick fires only the most recent due occurrence per deployment, and only if it falls inside a one-hour window (a package constant, not a knob). A day-long outage on a */5 schedule would otherwise spawn 288 sessions at once, and the reference's own (deployment_id, scheduled_at) key would admit every one of them. The same rule is what bounds a newly-tightened expression from back-firing, since no watermark is stored to reset. *Evidence: platform.claude.com managed-agents/scheduled-deployments (fetched 2026-08-27 — the unpause sentence); anthropic-openapi.yml:25955 (the dedup key, which bounds duplicates but not backlogs); internal/api/deploymentscheduler.go.* *Tracked: #78 (what a real schedule does across a deliberate outage, and what it does when an expression is tightened; note this is the one inference a recording may never settle, since the reference's downtime is not ours to arrange).*`

- **Entry 11.** `- **POST /v1/deployments/{id}/run on an archived deployment is a 400 (plan 37 slice 3)** — The docs call archive terminal, but publish no status code for a manual run against one, and the spec attaches no operation-specific errors to any deployment endpoint — every path carries the same boilerplate error block. We answer 400 "deployment %s is archived", matching how this platform already refuses to act on an archived resource. Running while merely paused is explicitly allowed and we allow it. *Evidence: anthropic-openapi.yml:14017-14150 (the run path's responses: no archived-specific arm); platform.claude.com managed-agents/scheduled-deployments (fetched 2026-08-27 — "Archive, unlike pause, is terminal"; "Manual runs through the run endpoint are still allowed while paused"); internal/api/memorystores.go:200 (the archived-is-400 precedent).* *Tracked: #78 (archive then run, and record the status).*`

- **Entry 29.** `- **Update, pause and unpause on an archived deployment are 400s (plan 37 slice 1)** — Archive is terminal and the reference publishes no status code for acting on an archived deployment through any path; the spec attaches no operation-specific errors to any deployment endpoint. We answer 400 "deployment %s is archived" on update, pause and unpause alike, matching how this platform already refuses to act on an archived resource, while GET and list (with include_archived) still return the row. Leaving update unspecified would leave hidden configuration mutable on a row nothing can ever fire. The manual-run path is the same refusal one slice later (entry 11). *Evidence: anthropic-openapi.yml:13355 (the update path, read by entry 8; :13624 is archive), :13755-13886 and :13886-14017 (pause and unpause, read by entry 12 for the same premise — no per-endpoint error semantics); platform.claude.com managed-agents/scheduled-deployments (fetched 2026-08-27 — archive described as terminal, with no status code for acting on one).* *Tracked: #78 (update, pause and unpause an archived deployment on the reference, and read the status codes).*`

- **Entry 12.** `- **Pause and unpause are idempotent 200s, unpause does not re-validate the cause, and a body on an action endpoint is ignored (plan 37 slice 1)** — The reference documents neither a conflict for pausing an already-paused deployment nor a precondition failure for unpausing an active one, says nothing about whether unpausing an error-paused deployment is refused while the cause persists, and declares no requestBody on any of the four action endpoints. We answer 200 in all four idempotency cases, returning the resource; unpause clears paused_at and both reason columns unconditionally, and the next scheduled fire re-discovers the cause and re-pauses if it is still there; a request body is neither read nor refused. *Evidence: anthropic-openapi.yml:13755-13886 and :13886-14017 (pause and unpause: no requestBody, no per-endpoint error semantics); anthropic-sdk-go betadeployment.go:2233-2254 (all four params structs carry only the beta header).* *Tracked: #78 (six calls: pause twice, unpause twice, each against an archived deployment, and one with a non-empty body).*`

- **Entry 13.** `- **schedule.last_run_at moves only on a successful scheduled fire (plan 37 slice 4)** — The field is "Time the most recent scheduled run actually started. Null until one completes; preserved after the deployment is archived. Manual runs do not update this." The last two clauses are published and we honor them exactly; what the sentence does not say is what a fire that failed to create a session does to it. We read "started" as "started a session" and leave the field untouched on a failed fire. All three behaviors fall out of one derivation rather than three code paths: last_run_at is the created_at of the row ORDER BY scheduled_at DESC LIMIT 1 selects over this deployment's runs where trigger_type = 'schedule', session_id IS NOT NULL and scheduled_at IS NOT NULL — the trigger filter excludes manual runs, archive touches no run row, and a failed fire's row has a null session_id. created_at rather than scheduled_at because the field says the run "actually started" and scheduled_at is the pre-jitter cron match, which this platform fires 0-30 seconds after in steady state and up to an hour after across a restart that triggers catch-up. The row is selected by ordering on scheduled_at with an explicit scheduled_at IS NOT NULL, which is what lets the partial unique index serve it; that is not the same query as MAX(created_at), and deliberately so — under overlapping fires it names the most recent occurrence rather than the most recently inserted row. *Evidence: anthropic-openapi.yml:22759-22763; internal/api/deployments.go (the LATERAL derivation).* *Tracked: #78 (let a scheduled fire fail against an archived environment, then read the deployment).*`

- **Entry 14.** `- **session_creation_rejected_error does not pause the deployment (plan 37 slice 4)** — The paused-reason union carries fourteen of the run error union's sixteen types, omitting session_rate_limited_error and session_creation_rejected_error. The prose cuts the other way for this one — it is "rejected with a non-retryable validation error", and non-retryable is an argument for stopping the schedule — but the structure settles it: paused_reason.error "Matches the failed run's error.type", and the union contains no member this type could take, so a pause carrying it is unrepresentable — were one ever recorded, the schedule would keep firing. Nothing here records one: the fire routes every unclassified rejection through a full rollback that leaves no run row at all (entry 28), so the store admits the type and no path emits it. *Evidence: anthropic-openapi.yml:26468-26477 ("non-retryable validation error"), :23188 ("Matches the failed run's error.type"), :23193-23206 (the fourteen), :25892-25907 (the sixteen).* *Tracked: #78 (record a run of that type and read the deployment's status).*`

- **Entry 15.** `- **Cron day-of-month and day-of-week are combined with POSIX union semantics, and every minute is an accepted expression (plan 37 slice 1)** — The published dialect fixes the grammar precisely — five fields, DOW 0-7 with both 0 and 7 Sunday, no seconds or year field, no L / W / # / ?, no @daily — but says nothing about the classic POSIX rule that a restricted day-of-month and a restricted day-of-week are OR'd rather than AND'd. The strongest evidence for the union is the dialect's own name: both schemas call it a "5-field POSIX cron schedule", and union is what POSIX specifies. No floor on frequency is published either, so we accept "* * * * *". *Evidence: anthropic-openapi.yml:22740 and :22780 ("5-field POSIX cron"), :22747 and :22787 (the dialect, verbatim, on both schemas); anthropic-sdk-go betadeployment.go:1479-1483; platform.claude.com managed-agents/scheduled-deployments (fetched 2026-08-27 — "Maximum granularity supported is at the minute level"); internal/cron.* *Tracked: #78 (create a deployment with "0 0 13 * 5" and with "* * * * *", and read upcoming_runs_at).*`

- **Entry 16.** `- **upcoming_runs_at returns five occurrences whenever five exist (plan 37 slice 1)** — The schema says "Up to 5" and publishes when the array is non-empty and when it is empty; it does not say when fewer than five come back for a live schedule. We return five whenever five exist within a twelve-year search bound — twelve rather than a round four because 0 0 29 2 * has an eight-year gap across the non-leap year 2100, and a shorter bound would return an empty array for a live schedule the same schema requires to be non-empty. An expression with no occurrence inside that bound is refused at create and update rather than stored (entry 27). (The two behaviors the schema does state — non-empty for active and paused deployments, empty once archived_at is set — are implemented as published and are not inferences.) *Evidence: anthropic-openapi.yml:22764-22768; anthropic-sdk-go betadeployment.go:1491-1496 (the same sentence, and no api: tag, making the key optional).* *Tracked: #78 (compare a weekly and a per-minute schedule's arrays on one recording).*`

- **Entry 17.** `- **A deployment is fired only through POST /run and the schedule; sessions cannot name one (plan 37 slice 3)** — The reference's manual trigger context reads "The run was started manually by creating a session directly against the deployment", yet its own session-create params carry no deployment field and no /v1/deployments/{id}/sessions path exists. We take POST /run to be that path, described from the server's point of view, and keep deployment_id out of the session-create allow-list — where a client sending it gets the same 400 it gets today. *Evidence: anthropic-openapi.yml:24862 (the wording), :22412-22456 (create-session params: required agent and environment_id, no deployment field); anthropic-sdk-go betasession.go (SessionNewParams has no deployment field).* *Tracked: #78 (whether any request can set a session's deployment_id directly).*`

- **Entry 18.** `- **agent_archived_error is recorded for a roster member, never for the deployment's own agent (plan 37 slice 4)** — The type's schema description says "The deployment's agent was archived", while the guide says an archived top-level agent archives the deployment in the same operation with no run recorded, and that this error type is what an archived subagent produces. Both hold only if the type is unreachable for the top-level case, and under decision 7 it is unreachable by construction rather than by convention: a live deployment refuses the archive of the agent it pins, so the deployment's own agent can never become archived while the deployment can still fire. *Evidence: anthropic-openapi.yml:21402-21418 (both variants of the type); platform.claude.com managed-agents/scheduled-deployments (fetched 2026-08-27 — the subagent rule and "In both cases no deployment run is recorded" for the top-level agent); internal/api/agents.go archiveAgent.* *Tracked: #78 (archive a top-level agent and a roster member in turn, and read the run list both times).*`

- **Entry 19.** `- **Daylight-saving semantics: a nonexistent wall clock is skipped, a doubled one fires twice (plan 37 slice 1)** — Neither machine-readable source says anything about DST: a spec-wide grep for daylight / DST / non-existent / ambiguous returns one unrelated hit, and the BetaManagedAgentsSchedule and ScheduleParams doc comments carry no DST text. The only adjacent statement is "Literal wall-clock matching in the configured timezone", which decides neither case. Literal wall-clock matching admits exactly two readings per transition, and we take the conventional pair: a 02:30 that does not exist on the spring-forward day is skipped; a 01:30 that occurs twice on the fall-back day fires twice, as two rows with distinct scheduled_at instants. *Evidence: anthropic-openapi.yml:22780 (the one adjacent sentence); anthropic-sdk-go betadeployment.go:1478-1507 (no DST text); internal/cron.* *Tracked: #78 (run a schedule across both transitions in America/New_York and count the runs). A docs-page statement of either rule, if one is found on re-fetch, converts this entry to CONFIRMED in the PR that finds it.*`

- **Entry 23.** `- **A deployment's initial_events must satisfy the same adjacency rule a posted batch does (plan 37 slice 1)** — The reference's deployment initial_events union admits user.message, user.define_outcome and system.message with no stated ordering constraint. This platform routes a deployment's list through events.NormalizeInbound, the same normalizer POST /v1/sessions/{id}/events uses, which requires a system.message to be the final event of the request and to immediately follow a user.message / user.tool_result / user.custom_tool_result. So [user.message, system.message] is accepted and a lone system.message is a 400 — narrower than the union states. Applying one rule at every entry point beats a second normalizer whose ordering could drift from the first. *Evidence: anthropic-openapi.yml:23155-23170 (the three-type params union, no ordering constraint); internal/events/inbound.go:37-45 (the adjacency rule).* *Tracked: #78 (post a deployment whose initial_events is a lone system.message, and record the status).*`

- **Entry 24.** `- **A fired session inherits the deployment's environment, agent, vaults, resources and initial events — but not its metadata, and carries no title (plan 37 slice 4)** — No source says what a scheduled session's title is, or whether a deployment's metadata is copied onto the sessions it starts. We leave title null and metadata empty. Metadata especially: CLAUDE.md principle 5 names session metadata as the application layer's ownership hook, so a schedule that wrote into it would overwrite the one seam apps are told to use. *Evidence: anthropic-openapi.yml:23057 (deployment metadata) and :22435-22439 (session metadata) — two separate bags with no stated relationship; :22430-22434 (session title is nullable on create).* *Tracked: #78 (fire a schedule and read the resulting session's title and metadata).*`
- **Entry 27.** `- **A cron expression with no occurrence is refused at create and update (plan 37 slice 1)** — 0 0 31 2 * parses, every field is in range, and February never has 31 days; the reference publishes no validation rule for such an expression, only that upcoming_runs_at is non-empty for an active deployment — which is unsatisfiable together. We answer 400 rather than storing a schedule that can never fire, searching the same twelve-year bound upcoming_runs_at uses. Twelve years rather than four because 0 0 29 2 * has an eight-year gap across 2100, which a shorter bound would misreport as unsatisfiable. *Evidence: the OpenAPI spec's BetaManagedAgentsCronScheduleParams.expression (the dialect, with no validation rule) and BetaManagedAgentsCronSchedule.upcoming_runs_at ("Non-empty for active and paused deployments"); internal/cron.* *Tracked: #78 (post an unsatisfiable expression to the reference and read the status code).*`


### 8.2 Webhooks — stated plainly

**This platform has no webhook mechanism of any kind.** A full-repository grep for `webhook`
returns zero production lines: no `/v1/webhooks` route, no registration table, no signing, no
delivery queue, no retry policy. Every Go hit is an unrelated comment — the reference's
webhooks *page* cited as a doc source in `internal/events/status.go`, *GitHub's* webhook
payloads in `internal/executor/repoclone.go`, a Kubernetes *mutating admission* webhook in
`internal/sandbox/k8s/k8s.go`.

The absence is already registered twice: `docs/DIVERGENCES.md:123` states it verbatim
(*"this platform has no webhook subsystem at all"*) and commits to the sibling rule
(*"whatever delivers `session.outcome_evaluation_ended` will deliver these three by the same
machinery"*), and `:54` records the v1.62.0 family.

**The nine deployment webhook events are out of scope for plan 37**, registered as §8.1 entry
1 and tracked by the open #261. Three reasons: a webhook subsystem — registration surface,
signature scheme, delivery with retry and backoff, an outbox that survives a controlplane
restart — is a plan in its own right and larger than this one; #261 already scopes it and
names its first emitter, so building it here would be building ahead of a filed deferral,
which CLAUDE.md forbids; and the events carry no payload beyond
`{type, id, organization_id, workspace_id}`, so a consumer must `GET` the object anyway —
a `deployment_run` list poll is a workable substitute for a self-hosted operator, and
`deployment_run.started` is not even representable in our model, since the run record has no
started/settled distinction to report. (The substitute is not complete: a fire whose whole
transaction rolls back leaves no row for a poller to see, which is why §3.8's counters
matter.) **Do not file a new issue**; add the deployment family to #261's scope in a comment
when slice 1 lands.

### 8.3 Decisions — four settled, six standing recommendations

The owner settled decisions 3, 5, 7 and 10 on 2026-08-29; each is marked below with what was
chosen and what it costs. The other six were put as recommendations and drew no objection.
Every entry states the alternative it beat, so a later reader can tell a choice from an
inheritance.

1. **Host the scheduler in `cmd/controlplane`** (§4.1) rather than a fifth binary
   `cmd/scheduler`. A new binary adds a Dockerfile stage, a Helm deployment, a compose
   service, a `deploy/gcp` entry, an ARCHITECTURE process-topology row, and the failure mode
   of an operator who never deployed it.
2. **Catch-up: at most the single most recent due occurrence, inside a one-hour window, as a
   package constant with no environment variable** (§3.6). Confirm the hour and the absence
   of a knob.
3. **Reject `budget` on deployment create and update with a 400** (§8.1 entry 2).
   **Settled 2026-08-29.** The cost is a real client path rather than an obscure field: `ant
   beta:deployments` exposes `--budget`, `--budget.max-list-cost` and `--budget.type` on
   both create and update (`anthropic-cli pkg/cmd/betadeployment.go:47`, `:127-134`,
   `:241`, `:311-318`), and every one of those invocations 400s here. Storing and echoing an
   unenforced cap is the worse failure: an operator would believe a ceiling exists.
4. **Hand-roll `internal/cron`** (§3.1, ~250 lines, stdlib only) rather than taking a
   dependency that would need a rejection layer for the reference's strict-subset dialect and
   would still not supply the two DST rules. Bundled with it: `import _ "time/tzdata"` inside
   `internal/cron` (§3.2), ~450 KB of binary so no image or test can lack zoneinfo.
5. **Accept `system.message` in a deployment's `initial_events`, matching the reference.**
   **Settled 2026-08-29.** The reference's deployment union admits it where its session-create
   union does not (`:23155-23170` against `:22451`), and this platform's session-create gate
   refuses it today with *"initial_events supports user.message and user.define_outcome
   events only"* (`internal/api/sessions.go:448`, `:499`) — so the deployment path needs its
   own three-type allow-list. What it does **not** need is any relaxation of
   `internal/events/inbound.go:37-45`: a deployment's list runs through the same normalizer,
   which requires a `system.message` to be the **final** event of the request and to
   **immediately follow** a `user.message`, `user.tool_result` or `user.custom_tool_result`,
   so it can never stand alone (§8.1 entry 23). The grant is made knowingly rather than
   inherited: a `system.message` is *"Privileged context for the accompanying turn and all
   subsequent turns, appended to the session's system context as a `role: "system"` turn
   rather than replacing the top-level system prompt"* (`:23279`), so under a schedule it is
   re-injected into an unattended session on every fire — whoever can create or update a
   deployment can plant standing privileged instructions that run repeatedly with nobody
   reading them. `RoleDeveloper` already governs both surfaces, which is why the grant is
   admitted rather than gated further.
6. **Do not implement the documented fire jitter** (§3.4).
7. **Refuse to archive an agent while a live deployment pins it; do not cascade.**
   **Settled 2026-08-29**, and it is a third option neither the reference nor the earlier
   draft offered. `POST /v1/agents/{id}/archive` answers 400 naming the deployments whenever
   one with `archived_at IS NULL` pins the agent; an archived deployment never blocks, since
   it is terminal and can never fire; teardown becomes ordered — deployments first, then the
   agent (§5.3). It trades an unrecoverable destructive action for a recoverable refusal,
   which is what matters on a platform shipping neither unarchive nor
   `DELETE /v1/deployments`. The cost is a divergence from the reference, whose cascade rests
   on a single docs-page sentence: §8.1 entry 25 is therefore now a confirmed statement about
   *our* behavior rather than an inference about theirs. It also makes `agent_archived_error`
   unreachable for the deployment's own agent by construction rather than by convention
   (entry 18). `archiveAgent` still becomes a transaction as its own behavior-neutral step,
   now for the check-and-lock instead of for a cascade.
8. **Do not enforce the 1,000-deployment cap** (§8.1 entry 7).
9. **Plan number 37 and issue #51**, with the webhook family deferred to #261 and no new
   issues filed — and the `(post-v1)` marker removed from #51's title in the PR that starts
   slice 1.
10. **Fire unconditionally; install no overlap brake, matching the reference.**
    **Settled 2026-08-29**, against the earlier draft's recommendation. The reasoning is the
    project's premise: the reference states no concurrency policy anywhere, so a brake would
    be a behavioral divergence in a case it never defined. The consequence is accepted rather
    than mitigated — none of the reference's own brakes exists here (budget rejected, no
    session-creation rate limit, no deployment cap), and the reaper reclaims neither a
    `running` session nor an ask-blocked idle one, so a `* * * * *` schedule against a
    non-terminating agent exhausts the sandbox host and nothing stops it (§3.5, §9). The plan
    answers with observability instead, and the draft registry entry that would have recorded
    the brake is gone: with no brake there is no divergence to register.

---

## 9. Risks and non-goals

### Deliberately not built

- **Webhooks** — all nine deployment events. #261 owns the subsystem (§8.2).
- **Budgets** — the deployment `budget` field and everything downstream. #432 owns them; a
  deployment cannot stamp a cap onto a session that has no cap column.
- **`DELETE /v1/deployments` and unarchive** — the reference exposes neither (§8.1 entry 8).
- **`deployment_run.started` as a distinct state** — the run record carries no `status`,
  `started_at` or `completed_at` in any source, and its own description says *"no session
  lifecycle tracking"* (`:23231`). Adding one would be a divergence *from* the reference, not
  a gap filled *toward* it.
- **Run retention or pruning** — the reference publishes no retention rule for run records;
  a spec-wide grep for `retention|retained|purged` has exactly one hit, `:8569`, about tunnel
  certificates. Runs accumulate; an operator prunes on their own schedule, the
  `0022_principals.sql:23-31` stance — **with one constraint stated out loud, because the run
  row is not only history but the occurrence claim and the watermark**: deleting a row newer
  than the catch-up window un-claims its occurrence, which a tick can then fire a second
  time, and deleting the newest row moves the watermark backwards. Prune by
  `scheduled_at < now() - interval '7 days'` or anything coarser; never by row count, and
  never inside the window. The DDL comment says so beside the table.
- **Multi-tenant quotas and per-org rate limits** — the reserved `org_id`/`workspace_id`/
  `project_id` columns stay reserved. #56 and #46.
- **A `visible_at` delayed-work primitive on `work_items`** — wanted by plan 35's
  `wait_for_agents` sweeper, not by this (§4.3).
- **A cron-expression validation or preview endpoint** — the reference's is a Console
  feature, not an API one, and `upcoming_runs_at` on a created deployment already answers
  "what will this do".
- **Any Console UI** — this is the `/v1` surface only.

### Risks, and what bounds each

- **DST correctness is the highest-risk code in the plan.** Both rules are inferences
  (§8.1 entry 19), and both are easy to get silently wrong, because a wrong answer looks like
  a normal answer. Bounded by §7's fixed-zone contract tables — including the fall-back
  assertion of *two rows with distinct `scheduled_at`*, which is what catches a wall-clock
  column — and by the property test tying `Due`, `Next` and `Upcoming` to one implementation.
  Residual risk: stale tzdata on a host. Mitigated by embedding the database in the binary
  (§3.2) and by testing against transitions whose rules are decades old.
- **Clock skew is designed out, not tolerated.** Every replica reads one clock — Postgres's,
  one `SELECT now()` per tick (§4.2) — so skew cannot shift what fires, and no clamp is
  needed. Sourced from the replica's clock it would have been a silent correctness bug, not a
  latency bug: an early fire claims the occurrence's `(deployment_id, scheduled_at)` row and
  the honest fire is then refused. NTP still matters for log timestamps; it is not
  load-bearing for scheduling, and the ARCHITECTURE paragraph says so.
- **A tick that overruns its interval.** Bounded by firing with a small bounded concurrency
  rather than sequentially, and made visible by `deployment.tick.duration`. Lateness inside
  the catch-up window delays a fire rather than dropping it; what a collapse drops is the
  older occurrences of a backlog, and `deployment.occurrences.skipped` is the number that
  makes that debuggable (§3.6, §3.8).
- **A runaway schedule exhausting the sandbox host is unbounded, by decision.** Decision 10
  declines the overlap brake to stay wire-compatible, and nothing else here substitutes for
  it: budget is rejected, there is no session-creation rate limit, no deployment cap, and the
  reaper reclaims neither a `running` session, nor an ask-blocked idle one, nor anything on a
  `self_hosted` environment (`internal/executor/reaper.go:283-297`). A `* * * * *` schedule
  against a non-terminating agent consumes one sandbox per minute until the host gives out.
  The plan makes it **observable** rather than impossible — `deployment.fires` (§3.8) and
  `GET /v1/sessions?deployment_id=…` (slice 3) — and matching schedule granularity to the
  session duration an agent actually needs is the operator's job. This is the only risk in
  the plan carrying no technical bound, and it is named here so that reads as a choice on the
  record rather than an oversight.
- **An archived deployment blocks its environment's delete forever.** The foreign key
  refuses the delete while any deployment references the environment (§5.3), and archiving a
  deployment does not remove the row — this platform ships neither unarchive nor
  `DELETE /v1/deployments`, so the reference is permanent. §5.4 records it as "refused by
  the FK", which understates it: the operator's only recourse is to leave the environment in
  place. It is named here because §8.3 decision 7 cites this same dead end as its reason to
  refuse an agent archive rather than cascade one, and a reason that costly should appear in
  the risk list it creates.
- **A persistent unclassified fire failure leaves no database trace.** Entry 28 trades
  `unknown_error` for a rollback-and-retry, which recovers from a transient fault
  unattended; a permanent one writes no run row, moves no `last_run_at` and pauses nothing,
  so both list surfaces simply stop growing and an operator reading the API sees a
  deployment whose runs quietly stop. The record is telemetry only — `deployment.fires` with
  `outcome="abandoned"` and the `deployment.fire` span (§3.8) — which is why that third
  outcome exists.
- **Connection budget.** One connection per in-flight fire, now that the advisory lock is
  gone (§4.4). There is no controlplane `pool_max_conns` floor today; if the concurrency
  constant grows, add the startup check `cmd/executor/main.go:288-297` models rather than
  letting the pool wedge silently.
- **The `createSessionTx` extraction is a refactor of the platform's most load-bearing
  handler.** Bounded by landing it alone, behavior-neutral, in slice 2, with the entire
  existing session suite as its regression test — the plan-36-slice-5 idiom.
- **Test wall-clock in `internal/api`** — ~9.5 minutes already against a 30-minute ceiling
  (#490, #499), and three of six slices add to it (§7).
- **A scheduled session runs with no principal.** `created_by` is NULL on a session a fire
  creates, because there is no authenticated caller at 09:00. That is correct for a column
  that is audit-only and never on the wire, but it means the audit trail attributes a
  scheduled session to the deployment rather than to a person — which is why
  `deployments.created_by` exists (§6) and why the security-invariant bullet in slice 6 says
  it out loud rather than leaving a reader to notice the gap.
- **A scheduled session can strand on a permission ask.** An `always_ask` MCP toolset under a
  3 a.m. schedule goes idle on a confirmation nobody answers, and the reaper will not reclaim
  it (§3.7). Documented rather than refused, and unbounded for the same reason as the risk
  above: a nightly `always_ask` schedule parks one unreapable session per night until a human
  answers or archives them.

### Documentation this plan owes

A doc that lags the code is a defect by CLAUDE.md's own rule, and the verifier checks it on a
dedicated rung. Beyond `docs/DIVERGENCES.md` (§8.1) and the acceptance record in
`docs/HISTORY.md` at close-out:

| File | What changes | Slice |
|---|---|---|
| `STATE.md` | Active work and Tasks — currently "None"; every slice moves it | all six |
| `changelog.d/` | one fragment per slice, the change's narrative written once | all six |
| `docs/ARCHITECTURE.md:60` | the controlplane row says it runs *"the one background sweep"* — now two; plus the at-most-one-run-per-occurrence and clock-source sentences | 4 |
| `docs/ARCHITECTURE.md:277-368` | Package reference gains `internal/cron` | 1 |
| `docs/ARCHITECTURE.md:496-529` | Observability gains the scheduler's span and three instruments | 4 |
| `docs/ARCHITECTURE.md:369-495` | Security invariants gain the null-`created_by` and always-ask bullets | 6 |
| `CLAUDE.md` repo-layout tree | `internal/cron` | 1 |
| `AGENTS.md` | its distilled mirror, if the tree line reaches it | 1 |
| `README.md:7` and "What runs today" | the status line enumerates shipped capabilities and would go stale | 6 |
