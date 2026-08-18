---
status: draft
issue: "#53"
---

# Multi-agent session threads — the coordinator topology (plan 35)

This plan addresses #53 (its last slice closes it): a session's primary thread orchestrates work by spawning **session
threads**, each running an agent from the coordinator's roster. Today the seam is
reserved and nothing more — `internal/api/agents.go:55` rejects `multiagent` on agent
create/update, `internal/events/inbound.go:423` (`requireNullThread`) rejects any non-null
`session_thread_id`, `internal/brain/brain.go:510` stamps `session_thread_id: nil` on every
tool intent, `internal/store/migrations/0001_init.sql:106` reserves `events.thread_id`
(never written, never read), `internal/domain/event.go:97`'s `ThreadID` is dead, and no
`/threads` route exists — `ant beta:sessions:threads list` returns our `no such endpoint`
404 on **any** session, single-agent ones included (`internal/api/server.go:168`). After
this plan a coordinator agent's session runs its roster as concurrent threads on one
shared sandbox, every thread has its own event log, status, usage and stream, and the
real `ant` CLI drives all of it unchanged.

Scope decisions, proposed 2026-08-17 and settled with the user on 2026-08-19 (decision 2
was revised in that dialogue; the others stand as proposed):

1. **Bump the SDK pin to v1.63.1 as slice 0.** All managed-agents drift from the pinned
   v1.61.0 landed in one additive release (v1.62.0: advisor, budgets, `session.usage`,
   cost/server-tool usage fields, `redacted` block, `inference_geo`); v1.63.0 adds only
   the unrelated dream `output_behavior`, and v1.63.1 changes one client behavior (the
   tool runner posts "(no output)" for an empty text result — harmless to a server). The
   v1.61.0 thread surface is a forward-compatible subset: routes and params are
   identical, and the event unions grew only additive members (the thread stream gains
   `session.usage`, the idle stop reason `budget_reached`, the thread agent an advisor
   variant, content unions `redacted` — every one on decision 3's exclusion list; the
   thread agent snapshot already carries `type:"agent"`), so a bump obligates nothing —
   but the official docs this plan cites describe the v1.62 service, and a plan citing a
   schema the repo refuses to read is the doc/code lag the verifier exists to catch.
   Rejected: keeping v1.61.0 and carrying decision 3's exclusion list verbatim, reading
   post-pin surface as evidence, never as contract (docs/REFERENCE_PROJECTS.md's caveat).
2. **Coordinator sessions run on `self_hosted` environments too, under reading (a) of an
   unrecorded behavior; the worker stays thread-unaware.** The docs define cross-posting
   narrowly — a child's event reaches the session-level stream when the child "needs
   something from your client" (an `always_ask` permission, a custom tool's result) — and
   say nothing about self-hosted sandboxes in a multiagent session (the multiagent page
   never mentions self-hosting; the self-hosted-sandboxes page never mentions threads).
   Yet the reference's own worker reads only the session-level stream and has no thread
   concept at any tag. So either **(a)** the reference surfaces child threads' built-in
   tool calls on the session-level view of a `self_hosted` session — the only reading
   under which its own worker can serve them — or **(b)** it refuses coordinators on
   `self_hosted`. This plan implements (a), registered INFERRED (design decision 13):
   the BYOC model is the one the user set out — the worker fetches the session's
   unanswered tool calls, runs them in order, and the tool-call → thread mapping lives
   on the platform. If a recording later shows (b), what we hold is an accepts-where-
   the-reference-refuses divergence to register, not a redesign. Rejected: refusing
   coordinator sessions on `self_hosted` until a recording exists (the earlier proposal).
3. **Excluded, each behind its own issue and a DIVERGENCES entry:** the platform
   **advisor** roster entry (`{type:"advisor",model}`, reserved name `anthropic.advisor`,
   advisor threads, `redacted` advice) — a hosted model consulted mid-turn has no
   self-hostable analogue short of a second provider binding, a feature of its own;
   **session budgets** (`budget`, `budget_reached`, `session.usage`, `list_cost`,
   `server_tool_use`, the `session.budget_reached` webhook) — orthogonal to threads and
   needing list-price accounting nothing here has; **thread webhooks** — this platform
   delivers no webhooks at all (plan 21's precedent); **`agent.thread_context_compacted`** —
   nothing here compacts. Under decision 1 these become CONFIRMED divergences; under its
   alternative they are post-pin surface the registry need not name.
4. **Every session gets a primary thread, retroactively.** The reference's event catalog
   states that *every* session emits `session.thread_status_running` for its primary
   thread, and the multiagent page shows `GET /threads` listing it. So threads are not
   purely additive: existing single-agent sessions gain a listable primary thread, and
   transitions they make after the slice lands emit primary-thread status events beside
   the session ones (their history before it holds none — the log is append-only, and no
   backfill invents events). Design decision 2 keeps the retro cost to one
   SQL backfill and zero events-table changes; the alternative — thread events only in
   multiagent sessions, registered as a divergence — leaves `ant beta:sessions:threads
   list` broken on ordinary sessions and grows a permanently thread-event-less back
   catalogue for as long as it stands.
5. **No official recording exists; the delegation tools are implemented as inferences.**
   Neither cookbook notebook stores outputs, the SDK types no delegation tool at any tag,
   and the docs name the six tools in one cookbook paragraph and one advisor sentence.
   Every shape this plan infers is registered INFERRED against #78 and listed in the
   recording checklist; whether a real coordinator session can be recorded (Managed
   Agents API access) is a question for the user, and its answer decides how many of
   those entries close before slice 4 merges rather than after.

Out of scope, besides decision 3: per-thread skills directories (design decision 11
materializes the roster's union once, on both deployment kinds), a fork/inherit-context
primitive (the reference has none — every spawn is fresh), thread-count or wake budgets
beyond the documented 25-thread cap (argued under design decision 8), `rescheduling`
semantics (nothing writes the value today; the fold ranks it, no code produces it), and
a long-lived, stream-tailing BYOC worker (the reference's shape; ours stays one pass per
work item — decision 13 says why that is enough).

## Ground truth (verified 2026-08-17)

Resolved per CLAUDE.md's order: public docs (platform.claude.com/docs/en/managed-agents:
`multiagent-orchestration`, `events-and-streaming` § "Preview session thread events",
`agent-setup`, `sessions`, `budgets`, `webhooks`, `tools`, `reference` — the event
catalog — and the cookbooks `CMA_plan_big_execute_small`, `CMA_coordinate_specialist_team`,
`CMA_watch_subagents_live`, all fetched 2026-08-17; `self-hosted-sandboxes` fetched
2026-08-18) → `anthropic-sdk-go` read at the
pinned tag **v1.61.0** via `git show v1.61.0:<file>` (the checkout is at v1.63.1; every
citation below is a v1.61.0 line unless marked *v1.63.1*) → the `ant` CLI (v1.23.0,
Stainless-generated, adds no semantics). Four further local checkouts served as design
reference only, never as wire sources: `claude-code-source`, `deepseek-harness`,
`openai/codex`, `adk-go` (docs/REFERENCE_PROJECTS.md).

### Wire shapes

- **Coordinator config** — `multiagent: {type:"coordinator", agents:[…]}` on agent
  create/update (`betaagent.go:2620-2652`, `:2860-2900`, optional); response
  `BetaManagedAgentsMultiagent{type, agents:[{id,type:"agent",version}]}` — required on
  every agent (`betaagent.go:146`, `betasession.go:851-876`). Roster entry request union
  (`betasession.go:924-932`): a bare agent-id string (the roster doc says only "an agent
  ID string"; that it pins the member's *current* version is this plan's inference, by
  analogy with the session-create `agent` param's documented "pins the latest version",
  `betasession.go:2075`), `{type:"agent",id,version?}` (`betasession.go:178-197`), `{type:"self"}`
  (`betaagent.go:2223-2241`). Constraints, verbatim doc comment
  (`betasession.go:884-889`): "1–20 entries … Entries must reference distinct agents
  (after resolving `self` and string forms); at most one `self`. Referenced agents must
  exist, must not be archived, and must not themselves have `multiagent` set (depth
  limit 1)." Docs add: the roster is snapshotted at coordinator create/update and never
  follows later member updates; on update `multiagent` "is replaced as a whole … Pass
  `null` to clear it" (agent-setup); an `inference_geo` mismatch across the roster is a
  400 (post-pin field, excluded).
- **Session snapshot** — session create accepts no `multiagent`
  (`BetaSessionNewParams`, `betasession.go:2074-2096`) and `agent_with_overrides` cannot
  override it (`betasession.go:236-271`; docs: overrides "apply to the coordinator and
  its `self` copies", roster members by id are unaffected). The session's
  `agent.multiagent` is `SessionMultiagentCoordinator{type, agents:[SessionThreadAgent…]}`
  — **full definition snapshots**, not references (`betasession.go:1584-1610`;
  `SessionThreadAgent` at `betaagent.go:2244-2280`: `id, description, mcp_servers, model,
  name, skills, system, tools, type:"agent", version`, all required, and per its doc "The
  multiagent roster is not repeated here").
- **SessionThread resource** (`betasessionthread.go:115-160`): `id` (`sthr_`), `agent`
  (a `SessionThreadAgent`; *v1.63.1*: a union with the advisor variant, discriminated on
  `type`), `archived_at`, `created_at`, `parent_thread_id` ("Null for the primary
  thread"), `session_id`, `stats{active_seconds, duration_seconds, startup_seconds}`
  ("Zero for child threads, which start immediately"), `status` ∈ `running | idle |
  rescheduling | terminated` (`:201-210`; the same four values as sessions), `type:
  "session_thread"`, `updated_at`, `usage` — four fields at the pin, identical to session
  usage (`:211-236`; *v1.63.1* adds `active_seconds`, `list_cost`, `server_tool_use`).
- **Routes** (`betasessionthread.go:45-112`, `betasessionthreadevent.go:39-99`, `api.md`
  § Threads): `GET /v1/sessions/{sid}/threads/{tid}`, `GET …/threads` (PageCursor;
  `limit` "Defaults to 1000", `page` forward-only; **no** `order`, `include_archived` or
  status filter), `POST …/threads/{tid}/archive` (no body), `GET …/threads/{tid}/events`
  (PageCursor of the session event union; only `limit`/`page` — no `types[]`, no
  `order`), `GET …/threads/{tid}/stream` (SSE with the same `event_deltas[]` opt-in;
  note `/threads/{tid}/stream`, not `/events/stream`). No create, no delete. All
  `?beta=true` + `anthropic-beta: managed-agents-2026-04-01`.
- **Thread events** (`betasessionevent.go`): `session.thread_created {agent_name,
  session_thread_id}` — "Written to the parent thread's output stream" (`:4393-4428`);
  `session.thread_status_running | _idle{stop_reason} | _rescheduled | _terminated
  {agent_name, session_thread_id}` — each "Emitted on the thread's own stream and
  cross-posted to the primary stream for child threads" (`:4430-4661`); the idle
  stop-reason union is `end_turn | requires_action{event_ids} | retries_exhausted`
  (`:4471-4544`; *v1.63.1* adds `budget_reached`); `agent.thread_message_sent {content,
  to_session_thread_id, to_agent_name?}` — "emitted to the sender's output stream"
  (`:637-676`) and `agent.thread_message_received {content, from_session_thread_id,
  from_agent_name?}` — "written to the target thread's input stream" (`:478-517`), the
  `*_agent_name` "Absent when … the primary agent", content = `text | image | document`
  (*v1.63.1* + `redacted`); `agent.thread_context_compacted {id, processed_at, type}`
  (`:447-476`). `session_thread_id` is `api:"required"` on the five `session.thread_*`
  events and `api:"nullable"` on `agent.tool_use` / `agent.mcp_tool_use` /
  `agent.custom_tool_use` ("When set, this event was cross-posted from a subagent's
  thread … Empty on the thread's own events", `:982`, `:347`, `:126`) and on
  `user.tool_confirmation` / `user.custom_tool_result` / `user.tool_result` /
  `user.interrupt` responses; absent from `agent.message`, `agent.thinking`, results,
  `user.message`, `session.status_*`, `session.error`, spans.
- **Inbound routing** — only `UserInterruptEventParams` has a `session_thread_id` request
  field (`:6529-6540`): "If absent, interrupts every non-archived thread in a multiagent
  session (or the primary alone in a single-agent session). If present, interrupts only
  the named thread." The confirmation/result params have none, though their response
  types say "Echo this…"; the docs resolve it — "Post `user.tool_confirmation` (with
  `tool_use_id`) or `user.custom_tool_result` … the server routes the response to the
  correct thread automatically." So the routing key is the tool-use id.
- **ID prefix** — `sthr_`: three "Public `sthr_` ID" doc comments and seven occurrences
  of the fixture `sthr_011CZkZVWa6oIjw0rgXZpnBt` across SDK and CLI tests. (The docs' one example says
  `sth_01DEF…`; the SDK is authoritative — recording item.)
- **BYOC** — `BetaSelfHostedWork.Data` is exactly `{id, type:"session"}` at both tags
  (`betaenvironmentwork.go:490-501`), and `betasessiontoolrunner.go` has no thread
  concept: one session-level stream, serial dispatch, `user.tool_result` without a thread
  field. Its discovery mechanics (verified 2026-08-18): `streamLoop` (`:698-726`) opens
  the session-level SSE **first**, then runs `reconcile` (`:815-861`) — one full
  `ListAutoPaging` walk of the session's history, `order=asc`, `limit=1000`, collecting
  every `agent.tool_use`/`agent.custom_tool_use` and marking `answered` from the results —
  **once per (re)connect**; the steady state is the live stream alone, deduplicated by
  tool-use id (`seen`/`answered`). `lib/environments/worker.go:365-380` runs one runner
  per work item and holds the item until the session terminates or sits idle on
  `end_turn` for `MaxIdle` (60 s), then force-stops it. Neither file, nor the poller,
  nor the CLI's `pkg/cmd/worker.go` (v1.23.0) contains the word "thread" at v1.63.1.
  Contrast ours: `internal/worker` opens no stream at all — claim → one bounded
  newest-first list scan (`toolexec.go:197-257`, #76) → run → post → force-stop, one
  pass per item.
- **Not typed anywhere**: the delegation tools. The cookbook (plan-big §1) is the sole
  source: "the server automatically gives it `create_agent`, `send_to_agent`,
  `wait_for_agents`, and `list_agents`, and workers get `submit_result` and
  `send_to_parent` the same way. You never define any of those tools … Its `create_agent`
  tool takes a bare agent name and task string." The docs' advisor section names
  `list_agents` and `send_to_agent`. No schema, no return shape, no statement of whether
  the calls appear as `agent.tool_use` on the primary log.

### Documented semantics the design rests on

- The session-level stream **is** the primary thread: "a condensed view of all activity
  across all threads. You don't see the full activity from subagents, but you do see the
  start and end of their work, and blocking events such as tool permission requests."
  What is cross-posted is stated once, and narrowly: "If a subagent needs something from
  your client, such as permission to run an `always_ask` tool, or the result of a custom
  tool, the event is cross-posted to the primary thread with `session_thread_id`
  identifying the originating session thread" — the SDK's `agent.tool_use` field doc
  says the same ("to surface its **permission request** on the primary thread's stream",
  `:978-982`; the `agent.custom_tool_use` variant says "its custom tool use", `:123-125`).
  Nothing says an allow-policy built-in call is surfaced — on cloud the platform runs it
  and no client needs to see it — and nothing says what a `self_hosted` session shows
  (scope decision 2). A child's own `agent.message`/thinking/results never reach the
  session stream; previews are per-connection and per-thread; the plan-big cookbook
  (§4) adds that the session feed carries only the primary thread's
  `span.model_request_end` — a cookbook statement, not a docs-page one.
- "All agents share the same sandbox, filesystem, and vault credentials"; context, tools,
  MCP servers, skills, model and system prompt are per agent. "A maximum of 25 concurrent
  threads is supported"; "the coordinator can call multiple copies of each agent";
  "Threads are persistent: the coordinator can send a follow-up to an agent it called
  earlier, and that agent retains everything from its previous turns."
- "The session `status` is an aggregation of all agent activity; if at least one thread
  is `running`, then the overall session status is `running`." Three independent
  signals give the contrapositive (session idle ⇔ every thread idle, one
  `session.status_idle` per quiescence): the budget-pause sequence emits N per-thread
  idles then exactly one session idle; an advisor consultation emits a thread idle with
  no session idle because the primary is mid-turn; billed `active_seconds` is "the time
  during which the session had at least one thread running". `terminated` is not a
  rollup: "the primary thread's end … surfaces only as `session.status_terminated`"
  (webhooks); a child that finishes "goes `idle`, not `terminated`". Precedence when
  threads' idle reasons disagree is documented for one pair — "A pending ask outranks
  the cap: a session with one thread waiting on `requires_action` and another paused at
  `budget_reached` reports `requires_action` at the session level" (budgets); ranking
  `end_turn` below both is this plan's extension of that rule.
- "Archive only succeeds if the thread is `idle`. A thread parked on `requires_action`
  counts as idle"; archiving "frees up a thread against the 25-thread limit". Against a
  child blocked on `requires_action`, an interrupt "closes each pending tool call with an
  error tool result … and re-emits `session.thread_status_idle` with `stop_reason:
  end_turn` directly; the model is not sampled"; against an idle thread it is a no-op.
- `system.message` "lands on the primary thread only" (events-and-streaming). Nothing
  says the same of `user.message`; that it does is inferred from the wire — its params
  carry no thread field, so there is nowhere else it can be addressed (registered
  INFERRED, slice 3).

## Design decisions

1. **A thread is a row; the primary thread's id is derived from the session's.**
   `session_threads(id sthr_, session_id, parent_thread_id NULL for the primary, agent
   jsonb — the SessionThreadAgent snapshot with type:"agent", agent_name, status, usage
   jsonb, created_at, updated_at, archived_at, org/workspace/project)`. The primary
   thread's id is `sthr_` + the session id's token — deterministic, valid in the id
   alphabet, unique because session ids are, and opaque to clients (ids need not match
   the reference's derivation, which is unrecorded). That makes the primary row
   backfillable in plain SQL for every existing session (`agent` NULL — read through, below;
   status/usage/timestamps from the session row) and lets any brain
   name the primary thread without a lookup. **The primary row stores no agent snapshot
   of its own**: its `agent` column is NULL and every reader — the thread GET, decision
   14's MCP discovery — renders it from `sessions.resolved_agent` minus `multiagent`, so
   a session update, which rewrites `resolved_agent` (`sessions.go:924`), needs no second
   write and can never leave a stale duplicate; child rows hold their spawn-time snapshot
   (a session update never reaches a running child — INFERRED, recording item). A partial
   unique index `(session_id) WHERE parent_thread_id IS NULL` makes "one primary per
   session" a schema fact, not a convention. Rejected: minting the primary id (needs a
   Go-side backfill or a lazy insert on GET) and synthesizing the primary on read (needs
   the derivation anyway, and cannot hold the primary's own usage once children exist).
   Thread `stats` render the empty shape, the precedent DIVERGENCES already records for
   session `stats`.
2. **`events.thread_id` NULL means "the primary thread"; the payload carries the wire
   id.** `eventWire` renders from the payload, so a `session.thread_status_running` event
   carries its `session_thread_id` in the payload and no events row is rewritten. Child
   events are stored once with `thread_id = <child>`; the session-level list/stream is
   the view `thread_id IS NULL OR type ∈ {session.thread_created, session.thread_status_*,
   agent.thread_message_received-on-primary, cross-posted agent.*tool_use, and the
   inbound user.tool_confirmation / user.custom_tool_result answering a cross-posted
   call}` — store once, filter per stream. **The primary thread's own list/stream is
   that same view** (the docs say the session-level stream *is* the primary thread, so
   `GET /threads/{primary}/events` and `GET /events` never differ); a child's is
   `thread_id = $tid`. Rendering is per surface: `session_thread_id` is set on a child
   event seen through the session/primary view and empty on the child's own stream
   ("Empty on the thread's own events"), rendered from the row's `thread_id` and the
   view being served, not stored twice. Preview frames follow the same rule: the
   broker envelope gains the emitting thread's id, the session/primary stream previews
   only primary-thread turns, and a child's stream only its own — a child's
   `agent.message` deltas never reach a session-level subscriber, whose buffered event
   would be filtered out and leave an orphaned preview. One event, one `sevt_` id on
   both surfaces — the reconnect procedure the docs describe (seed seen ids from the
   list, skip duplicates) works unchanged. Rejected: dual-writing cross-posts (two rows,
   two ids, breaks "one event, one id" and the append-once invariant). Index
   `(session_id, thread_id, seq)` in the same migration.
3. **One session-wide `seq` under the session row lock, unchanged.** Threads claim
   concurrently and commit serially on the session row (`commitUnderLock`). This preserves
   the seq/`created_at` agreement the log guarantees, the SSE `AfterSeq` cursor, the events-list keyset
   cursor, and — decisively — correctness of cross-thread writes: a child's settlement
   appends to the parent's log and reads the parent's outstanding calls; a per-thread lock
   would make that a lost-update race, not merely a different design. Cost: one hot row
   per multiagent session, bounded by the 25-thread cap. Rejected: per-thread seq
   (breaks both cursors and the total order the reference's own primary stream implies).
4. **Session status is a fold over thread statuses; thread status is authoritative.**
   Written in the same transaction that moves a thread's status, under the same lock:
   `terminated` iff the primary is terminated; else `running` iff any non-archived thread
   is running; else `rescheduling` iff any thread is; else `idle`. The enqueue-to-claim
   window a status fold could misreport as quiescence (the one deepseek-harness closes
   with its `accepted` set) is closed by today's invariant carried per thread: a thread's
   status is set `running` in the **same transaction** that enqueues its `model_turn` —
   a spawn, a report that wakes an idle parent, a confirmation that resumes a thread.
   Exec items are session-keyed and never enter the fold: a `tool_exec` a BYOC worker
   holds while a session sits idle must not keep it `running` (the reference runner
   only releases that item after `session.status_idle{end_turn}` + `MaxIdle` — counting
   the item would deadlock a single-agent `self_hosted` session that idles today).
   Session-level idle `stop_reason` is a precedence pick over the idle threads' reasons —
   `requires_action ≻ retries_exhausted ≻ end_turn` — with `event_ids` the seq-ordered
   union of every thread's outstanding asks. Emission: on a single-agent session every
   existing `session.status_*` site keeps exactly its emission — including the
   interrupt's idle→idle re-emit and the reclaim's `rescheduled`+`running` pair on an
   already-running session, neither of which is a value change; on a multiagent session
   any thread's transition, the primary's included, emits a session event only when the
   folded value changes (so the coordinator's W1 park emits its `thread_status_idle` and
   no session event while children run), the two value-independent emissions above
   staying as they are, plus the payload-only re-idle when the ask set shrinks or grows.
   Order within one transition:
   **thread event first, session event second** (facts, then rollup — the order the
   reference's budget sequence shows). A single-thread session reduces to today's
   behavior exactly; that reduction is the regression gate.
5. **Turns are (session, thread); exec work stays per session.** `work_items.thread_id`
   (NULL = primary); the live-dedup index becomes `(session_id, thread_id, kind)`
   **`NULLS NOT DISTINCT`** (Postgres 15+; every fixture and deployment runs 16) — under
   the default NULLS DISTINCT two `(sesn, NULL, model_turn)` rows would never conflict
   and the primary's turn and every session-keyed exec item would lose the dedup that
   0003 exists for — with `Enqueue`'s `ON CONFLICT` arbiter changed to the same three
   columns in the same slice, so sibling threads run `model_turn`s concurrently; the
   brain's `claimLiveSession` checks the **thread's** status; replay, watermark
   (`MarkProcessedThrough`), `pendingInput` and every `toolflow` unanswered-query become
   thread-scoped (a session-wide watermark would stamp a sibling's queued input as
   processed — a correctness hazard, not a refinement). `tool_exec`/`web_exec`/
   `mcp_exec` remain session-keyed: one shared sandbox, one item covers every thread's
   backlog, execution across threads is serial (what the reference's own runner does),
   and the executor drains **per thread** — it wakes each thread's `model_turn` as that
   thread's calls become answered and re-scans before completing the item so a call
   that arrived under a live item is never stranded (the BYOC worker's counterpart is
   decision 13). **The exec drivers run the runnable set, not the unanswered set.**
   Today a turn holding an ask enqueues no exec item at all (`brain.go:651-676`) and the
   confirmation arm enqueues only after the last ask is answered, so "unanswered" and
   "runnable" coincide; with siblings they do not — thread B's allow-policy call would
   enqueue the session's `tool_exec` while thread A's `bash` still awaits its human, and
   a driver that runs everything unanswered would execute A's gated command in the
   shared sandbox. So the set every driver drains, the re-arm of decision 13 (iii)
   tests, and the worker's scan (which the reference runner already gets right by
   holding ask calls until their verdict) is: unanswered **and** (`evaluated_permission`
   = `allow`, or `ask` with an `allow` confirmation recorded for it) — allow-only, so a
   `deny` that has not yet been answered is never scheduled either. Rejected: a thread id on the
   work item's wire shape (the reference has none; `work.data` is `{id, type:"session"}`)
   and per-thread sandboxes (documented shared; the BYOC wire could not even name one).
6. **Delegation is a settlement feature, not a tool-execution feature.** The six tools
   are injected by thread role in `resolveTools` — coordinator four on the primary thread
   of a session whose snapshot has a non-empty roster, worker two on any child; a child
   never receives the coordinator four (depth 1); an injected name shadows an agent's
   own custom tool of that name (dropped with a `notes` entry) — and carry a fourth
   `toolClass` kind, *settlement-executed*: `turnEvents` never escalates them to a work
   item, and `commitTurn` resolves them inside the settlement transaction: `create_agent`
   validates the name against the roster snapshot and the 25-cap, inserts the thread row,
   appends `session.thread_created` + `agent.thread_message_sent` on the primary and
   `agent.thread_message_received` + `session.thread_status_running` on the child,
   enqueues the child's `model_turn`, and answers `{session_thread_id}`; `send_to_agent`
   (target by thread id, else by name when exactly one live thread runs that agent, else
   `is_error` naming the candidates) appends the sent/received pair and wakes a target
   idle on `end_turn`; a target parked on `requires_action` — idle by definition, but
   with a human's verdict outstanding — only receives the message (it is pending input
   by seq when its ask resolves and its turn resumes), and a running target reads it at
   its next settle, as decision 7 says; `list_agents` reads the rows; `submit_result`
   appends the child's `agent.thread_message_sent`, `session.thread_status_idle{end_turn}`
   and the parent's `agent.thread_message_received`; `send_to_parent` the same without
   ending the child's turn. Every call is answered by an `agent.tool_result` in the same
   commit, **and the commit schedules what follows**: a turn whose calls were all
   settlement-executed and none a wait or a `submit_result` has nothing left to wait
   for, so the same transaction enqueues the calling thread's next `model_turn` (the
   executor's own rule after its last answer — today's tool branch would otherwise
   `Complete` and enqueue nothing, leaving the thread `running` with no live item and
   no trigger); a `submit_result` **ends** the child's turn — the child idles and nothing
   re-enqueues it until a message arrives — and to keep that unambiguous a
   `submit_result` sharing its turn with exec-family calls is answered `is_error`
   ("report after your tool calls have returned") and ends nothing; a turn that mixed
   exec-family calls with the other delegation tools leaves the wake to that driver's
   drain, as today; a turn holding a `wait_for_agents` parks per decision 7 — **Design
   C: the delegation `tool_use`/`tool_result` are
   persisted on the calling thread's own log and the thread events are the client-facing
   projection.** Replay then needs no new arm
   for the model's own view, the request prefix stays byte-stable across turns, and the
   advisor's stated exemption ("composed by the platform rather than sent by the agent")
   does not extend to agent-sent calls. Its wire risk is confined to one output — the
   primary thread's event list — and degrades to a filter if a recording shows the
   reference hides them. `ValidateToolResults` marks the six names platform-owned so no
   client can forge a child's report.
7. **`wait_for_agents` answers immediately and parks the coordinator idle (W1).** The
   thread status enum has no "waiting" and the idle union nothing wait-shaped, so a waiting
   coordinator is either `running` with a held-open call or `idle`/`end_turn`. Held-open
   (W2) can wedge — a child that never reports leaves a `running` thread nothing can wake,
   and the platform has no delayed-work primitive (`work_items` has no `visible_at`;
   `Claim`'s only time predicate is lease expiry). W1 answers `{"message":"Wait started.
   Reports arrive as messages; do not conclude yet.","timed_out":false}` (Codex V2's
   payload-less shape), settles the primary `idle`/`end_turn` — the session stays
   `running` under decision 4 because children run, which is the only reading under
   which the documented aggregation sentence is load-bearing — **when there is something
   to wait for**: a wait issued with no child running or parked on `requires_action`
   (a child awaiting its human will still report) and no report unread (none spawned,
   all already reported, the rest idle on `end_turn` or archived) is answered in-commit with
   `{"message":"No agents are running and no reports are pending.","timed_out":true}`
   and the turn continues (next `model_turn` enqueued, no park) — Codex's wait returns
   at once on pending activity and times out otherwise, and a park nothing can wake is
   the wedge W1 exists to avoid. A report — `agent.thread_message_received` on the parent
   thread — is **pending input** for the parent, detected by seq, not by `processed_at`:
   an `agent.*` event is stamped at write (`events/log.go:189-194`) and the field is
   wire-required, so `pendingInput`'s `processed_at IS NULL` predicate can never see it;
   instead the child's settlement, under the session lock, enqueues an idle parent's turn
   directly (setting it `running` in the same transaction, decision 4), and a running
   parent's `settle` chains once iff a report's seq exceeds the head its turn replayed to
   (`settleEndTurn` already carries that head). The `Enqueue` dedup makes N reports cost
   at most one queued turn plus one chain. Replay renders a received message as a
   user-role text block (Claude Code's task-notification shape). A child's ending
   condition (interrupted, archived, errored) is delivered the same way, as text naming
   the outcome and a next action. Guard: the primary thread cannot be archived (400;
   archive the session instead). W2 is a later, local change (join predicate in the
   child's settlement + a sweeper) if a recording shows the reference's coordinator
   thread stays `running` across a wait.
8. **Caps and amplification.** 25 non-terminated threads per session, **the primary
   counted** — "a maximum of 25 concurrent threads" and the primary is a thread; whether
   the reference exempts it is unrecorded (INFERRED, recording item 9), and counting it
   is the conservative reading of a resource cap — so the `create_agent` that would make
   a 26th live thread (the 25th child while the primary lives) is the `is_error`. A
   finished child "goes `idle`, not `terminated`" (docs), and archiving it is what
   terminates it (decision 12), which is how archiving "frees up a thread against the
   25-thread limit". The roster bound (1–20, distinct,
   one `self`, depth 1) enforced at agent write. Wake amplification is bounded
   structurally by `Enqueue`'s dedup and `pendingInput`; a coordinator that answers every
   report with a fresh spawn is bounded only by the instantaneous cap — recorded as a
   known consequence, not solved here (a spawn budget is a policy no source documents).
9. **HITL and interrupts per thread.** A child's ask-gated `agent.tool_use` (and
   `agent.custom_tool_use`, `agent.mcp_tool_use`) is written on the child's log and
   surfaces on the session stream by decision 2's filter with `session_thread_id` set;
   the child idles `requires_action` and the session's fold follows decision 4. Inbound
   `user.tool_confirmation` / `user.*_result` route by the referenced tool-use row's
   `thread_id` (one column added to `ValidateToolResults`' projection); an explicit
   inbound `session_thread_id` is accepted and must match — a mismatch is a 400.
   `user.message`, `user.define_outcome` and `system.message` carry no thread id and
   always address the primary thread.
   `user.interrupt` without a thread id interrupts every non-archived thread and keeps
   today's `CancelSession` exactly — every live item of every kind, so the in-flight
   sandbox command is aborted through the executor's lease keeper as now; with one it
   interrupts that thread only (`CancelThread` on its `model_turn`), synthesizes its
   error results, re-idles it `end_turn`, and never stops the shared exec item — a
   sibling's calls ride on it. What it does to the interrupted thread's *in-flight*
   command: the exec drivers treat an answered call as cancelled — before starting one
   (the answered-check under the lock that plan 16's one-answer invariant already
   requires; a late result for it is dropped) and, for the executor, mid-run: its
   per-item lease keeper checks the running call's answered-ness on each beat and
   cancels the run context, so an interrupted `sleep 3600` costs one beat, not
   `toolset.MaxTimeout`, and the sibling calls queued behind it in the serial runner
   are not held hostage. Our BYOC worker does the same on its heartbeat; the reference
   runner cannot be told and runs the command to completion (known consequence).
10. **Roster resolution and snapshotting is a foundational slice of its own.** Agent
    create/update resolve the roster inside the write transaction: bare strings and
    versionless references pin the member's current version eagerly (the response
    type's `version` is a plain int64 with no `latest` alias — unlike skills); `self`
    resolves to the coordinator's own id and the version the write is about to produce;
    C-1…C-7 checked against the merged spec with `FOR SHARE` on the referenced agents.
    Session create builds `session.agent.multiagent.agents[]` as full
    `SessionThreadAgent` snapshots by fetching each member's pinned version — **except
    the `self` member, whose snapshot is the coordinator's own resolved spec for this
    session, overrides applied, minus `multiagent`** (docs: overrides "apply to the
    coordinator and its `self` copies"; a `self` copy built from the stored version would
    run the un-overridden system prompt) — computed at the API boundary and stored in
    `sessions.resolved_agent`, leaving `domain.AgentSpec.Multiagent` untyped (option
    (a); splitting the type is the bigger diff for no runtime gain). `multiagent` inside
    `agent_with_overrides` becomes an explicit 400 instead of today's silent drop.
11. **Skills: materialize the union of the roster's skills once per session.** The roster
    is snapshotted at session create, so the union is known before the sandbox exists;
    the shared filesystem is documented, and each thread's system prompt still injects
    only its own agent's Level-1 metadata. Rejected: per-thread skill directories under a
    shared workdir (the `.materialized` sentinel is session-scoped and the reference
    shares one filesystem anyway).
12. **Primary-thread events on every session, mirroring `running`/`idle`/`rescheduled`;
    never `terminated`.** The catalog asserts `_running` for every session; idle and
    rescheduled follow the SDK's "Emitted on the thread's own stream" doc on all four;
    the webhooks page carves the primary's end out. A session's history from before the
    slice holds none of these events (append-only; nothing backfills them) — stated in
    the registry entry, not discovered later. A child reaches `terminated` two ways, both
    documented: **archiving it** ("A thread was archived or encountered a terminal error"
    — multiagent; "either because the thread was archived or because it exhausted its
    retries" — webhooks) sets `archived_at`, moves its status to `terminated` and emits
    `session.thread_status_terminated`, cross-posted to the primary (SDK `:4430-4661`);
    and the **session's own end** — termination, archive or delete — does the same for
    every child still live. A terminal error / exhausted retries is left as a recording
    item, because the pages disagree with the idle union: the SDK types
    `retries_exhausted` as an *idle* stop reason (`:4471-4544`) while the webhooks page
    files it under `thread_terminated`; until recorded, a child that exhausts retries
    idles `retries_exhausted` (the typed shape) and its coordinator is told (decision
    7). No other path terminates a child.
13. **BYOC under reading (a): the worker stays thread-unaware; the platform puts what it
    must run on the view it already reads.** Four parts.
    (i) *The view rule.* For a session on a `self_hosted` environment, decision 2's
    session-level filter widens to every child thread's `agent.tool_use` — whatever its
    evaluated permission — and every result answering one (`user.tool_result` from a
    worker, `agent.tool_result` from an interrupt's synthesized errors), the uses carrying
    `session_thread_id` exactly as a cross-posted ask does on cloud. Cloud sessions keep
    the documented condensed view. Store-once/filter-per-stream makes this a predicate on
    the environment kind, not a second write. The `user.tool_confirmation` that releases
    a child's ask-gated call is in the session view on every session (decision 2): the
    reference runner parks an ask call until it sees the verdict on the session stream
    or list (`betasessiontoolrunner.go:750-751`, `:834-838`), and a verdict stored only
    on the child's log would leave the call suspended forever. The results are in the view for the two
    workers' sakes, and they read different ones: the reference runner's `reconcile`
    marks answered **only** from `user.tool_result` / `user.custom_tool_result`
    (`betasessiontoolrunner.go:842-845`), and a worker result it cannot see makes it
    re-dispatch the call on every reconnect, straight into the duplicate-result 400
    (DIVERGENCES `:143`); ours counts `agent.tool_result` too (`toolexec.go:157-166`).
    An interrupt's synthesized `agent.tool_result` is therefore invisible to the
    reference runner — it re-executes the interrupted call on reconnect — but that is
    today's behavior on every single-agent `self_hosted` session already (#429, filed
    from this review); the view rule neither causes nor cures it.
    (ii) *Nothing on the work item changes.* One `tool_exec` per session, `work.data`
    `{id, type:"session"}`; a result routes to its thread by the referenced tool-use row's
    `thread_id` (decision 9), and a `session_thread_id` a worker echoes is validated,
    never used to route.
    (iii) *A one-pass worker's window closes on the control plane.* Ours scans once at
    claim; if thread B commits tool calls while the worker runs thread A's, B's
    `Enqueue(tool_exec)` is a no-op against the live item, the worker force-stops on
    finishing its found set, and nothing serves B. Today that window is theoretical — a
    session's next turn cannot start before its last result lands, and the executor
    closes its own inside the settlement transaction (`executor.go:528-546`) — but with
    concurrent threads it is as wide as a tool run. So: when a `tool_exec` item reaches
    `stopped` through the work API and the session still has **runnable**
    platform-executed calls (decision 5's set — an "unanswered" test would re-arm for an
    ask-gated sibling call and loop stop → re-arm → nothing runnable → stop), the same
    transaction — **under the session row lock**, so
    it is serialized with every settlement and trigger that appends calls or enqueues,
    rather than resting on `ON CONFLICT DO NOTHING`'s wait-and-recheck — enqueues a
    fresh item of the kind the API's own trigger would pick, in its order
    (`events.go:395-421`: `mcp_exec` if an MCP call is unanswered, else `web_exec` if a
    web call is, else `tool_exec` — MCP first because only the platform's MCP driver can
    answer it and a BYOC worker must never be handed a log with one outstanding,
    `executor.go:501-526`). Benign for the reference runner, which stops only after
    `MaxIdle` of `end_turn` idleness, when nothing is unanswered; it also closes today's
    theoretical window. The lease-lapse path needs nothing — `Poll` re-offers the item.
    (iv) *The worker.* It already loads the session for its liveness gate; a snapshot
    with `agent.multiagent` set switches `unansweredToolUses` to a full newest-first walk
    under the same three-type filter — the #76 bound's two premises hold per thread, not
    per session, and no per-session stop condition exists short of the log's start (the
    reference pays this per attach; we pay it per claimed item, for coordinator sessions
    only) — and the driver re-scans after answering the found set until a scan comes
    back empty. Single-agent sessions keep the bound. Skills are decision 11's union,
    computed from the same snapshot; files are session resources and unchanged.
    Rejected: per-thread runners in the worker (thread endpoints the reference worker
    never calls); an unanswered-only discovery endpoint (new wire surface the reference
    worker would not use); a per-`session_thread_id` trailing-run bound (no stop
    condition — the group count is unknowable without listing threads); adopting the
    reference's long-lived tail (a rewrite of `internal/worker` to close a window a few
    control-plane lines close).
14. **MCP is per thread's agent.** Each roster member declares its own `mcp_servers`
    (`SessionThreadAgent`), but today discovery and dialing read the coordinator's
    `sessions.resolved_agent.mcp_servers` (`brain.go:252-264`, `executor/mcpwork.go:137`,
    `mcpexec.go`'s `declared[server]`) and the catalog is keyed `(session_id,
    server_name)` (`0023_mcp_catalogs.sql:43`) — a child declaring a server only it has
    would loop suspend→discover-nothing→resume, and two members naming different servers
    alike would collide on the key. So: the brain's `declaredMCPServers` reads the
    **thread's** agent (a child row's snapshot; the primary reads through to `resolved_agent`, decision 1); the `mcp_exec` item stays
    session-keyed and its driver resolves each `agent.mcp_tool_use` to its thread by the
    row's `thread_id`, discovering and dialing from that thread's declared list; the
    catalog key becomes `(session_id, thread_id, server_name)` in slice 3's migration
    (`thread_id` NULL for the primary — the same `NULLS NOT DISTINCT` care as the work
    index; a server two threads both declare is discovered once per thread, the price of
    per-agent declarations). Credentials (vault bindings) stay session-wide, as
    documented.
15. **Outcomes belong to the primary thread and grade on session quiescence.** Plan 21
    hooks grading into every `end_turn` settlement (`settleEndTurn`'s intercept,
    `grader.go:729`, `:770`) and every claimed `model_turn` (`brain.go:238-239`); left
    alone, a child's own
    `end_turn` — or the coordinator's W1 park — would start an evaluation cycle
    mid-delegation and harvest the shared sandbox while siblings still write to it, and
    a sibling's next claim would run the grader with the wrong agent. So the intercept
    fires only when a settlement moves the **session's folded status** to `idle` with
    `end_turn` (quiescence, decision 4), the grading turn runs on the primary thread
    with the coordinator's agent, and `user.define_outcome` addresses the primary
    (decision 9). A single-agent session's quiescence is its own `end_turn`, so plan
    21's behavior is unchanged there.

## Slices

Ordered so no landed slice leaves an incoherent state; each is one PR through the full
ritual. Lifecycle per CLAUDE.md: slice 0's PR — the plan's first — flips this plan to
`in-progress` and takes over STATE.md; **every slice lands its DIVERGENCES entries in the
PR that introduces the behavior**; the last slice archives the plan and closes #53.

0. **SDK bump v1.61.0→v1.63.1** (scope decision 1; the plan-05/11 ritual): pin, pairwise
   diffs, advance the three live version labels, re-read every `v1.61.0` evidence label
   in DIVERGENCES, CONFIRMED entries + issues for every excluded v1.62 item (advisor,
   budgets, `session.usage`, usage cost fields, `redacted`, `inference_geo`; note the
   CLI's `--budget` flag lands as a top-level `budget` key that `rejectUnknownKeys` 400s
   today, `wire.go:184` — `edge_test.go:79` pins the mechanism on session create — so
   the entry records a rejection, not a silent drop), HISTORY record.
1. **Roster resolution and snapshot** (design decision 10): `agents.go` create/update
   accept and resolve `multiagent`; `sessions.go` `resolveAgent` snapshots full member
   definitions, the `self` member from the session's overridden coordinator spec;
   overrides carrying `multiagent` 400; the "multiagent … rejected" clause of
   DIVERGENCES `:30` is corrected in place. Inert at runtime — no thread machinery
   exists yet, and a coordinator session behaves as a single-agent session.
2. **Thread resource and the primary thread** (decisions 1, 2, 12): migration
   `0025_session_threads.sql` (table with `session_id … REFERENCES sessions ON DELETE
   CASCADE` like every sibling table — sessions are hard-deleted — backfill of one
   primary row per existing session, `events (session_id, thread_id, seq)` index);
   `PrefixSessionThread` in `id.go` + CLAUDE.md's prefix line; `events.thread_id`
   written and read (`NewEvent.ThreadID`, `ListQuery.ThreadID`); the five routes
   (threads list defaults to `limit` 1000, its documented default, through the same
   per-route cap override the session-events list already uses — the shared paginator
   caps at 100; thread events list asc by seq with the existing seq cursor; the
   primary's list/stream serve the session view, decision 2; a thread stream = the
   session broker filtered per subscriber, `event_deltas[]` honored, preview frames
   carrying and filtered by thread; archiving an idle child — one parked on `requires_action` included, as documented, its pending calls first closed with error results the way an interrupt closes them — sets `archived_at`, moves it
   to `terminated` and emits `session.thread_status_terminated`; archiving the primary
   or a non-idle thread is refused with a clear error — the reference's status code for
   the latter is unrecorded); primary-thread status events beside the twelve `session.status_*`
   emission sites (thread first, then session); the 20 exact-sequence assertions
   updated, the 15 exact-key-set sites untouched. `ant beta:sessions:threads
   list|retrieve` and `beta:sessions:threads:events list|stream` work against a
   single-agent session.
3. **Thread execution substrate** (decisions 3, 4, 5, 9, 14, 15): migration
   `0026_work_thread.sql` (`work_items.thread_id`; the widened dedup index
   `(session_id, thread_id, kind) NULLS NOT DISTINCT` replacing
   `work_items_live_session_kind_idx`, `Enqueue`'s arbiter changed with it;
   `mcp_catalogs` re-keyed per thread); brain claims `(session, thread)` turns with
   thread-scoped replay/watermark/pendingInput; thread-scoped `toolflow` queries and the
   **runnable set** for every exec driver; the status fold and stop-reason pick with
   every existing emission preserved; the outcome intercept moved to session
   quiescence; MCP discovery/dial per thread's agent; the API's trigger arms
   thread-aware (a confirmation for thread A while B runs resumes A; the session never
   falsely idles); `requireNullThread` → accept-and-validate; `CancelThread` +
   thread-scoped interrupt with the drivers' answered-means-cancelled check (before a
   call and, on the executor's keeper beat, mid-call); executor per-thread drain and
   wake; result-append answered-check; the `tool_exec` stop re-arm (decision 13 iii —
   control-plane only, and it closes today's theoretical window on its own). Reachable
   only through tests until slice 4 (a test seam spawns a child row) — the substrate is
   coherent and the wire is unchanged for single-agent sessions.
4. **Coordinator delegation** (decisions 6, 7, 8, 11, 13; scope decision 2): tool
   injection by role, settlement-executed delegation, the thread-event projection, the
   wake path, child-failure delivery, the 25-cap, the primary-archive guard, skills union
   on both deployment kinds, the `self_hosted` session view rule and the worker's
   coordinator-session scan (decision 13 i, iv), the INFERRED entries for every inferred
   shape, and the end-to-end acceptance transcripts — cloud and `self_hosted`. This
   slice meets #53's acceptance.
5. **Close-out**: HISTORY acceptance + review-hardening records and the progress summary,
   ARCHITECTURE "Execution flow"/"Wire-compatibility model"/package rows and a new
   security-invariant paragraph (what a child thread shares: sandbox, vault bindings,
   gate), README status + "what runs" + roadmap line, plan → `archived`, STATE.md →
   "None.", #53 closed. (Docs move with each slice; this slice only carries what spans
   them.)

## Verification

- Slice 1: API contract tests for C-1…C-7 (each rejection its own case; `self` resolves
  to the coordinator's own next version; eager pinning survives a member update); the
  session snapshot renders full member definitions with `type:"agent"` and no nested
  `multiagent`; a session created with a `system` override snapshots its `self` member
  with that override and the other members without; overrides with `multiagent` 400.
- Slice 2: the migration backfill on a fixture with legacy sessions; `GET /threads` on a
  legacy and a new session both list one primary with `parent_thread_id: null` and the
  derived id; deleting a session deletes its thread rows; the events index; the
  primary's events list/stream equal the session's on a single-agent session **and**
  on a coordinator session with cross-posts (a child's ask shows on both, with
  `session_thread_id` set there and empty on the child's own stream); a child's
  preview deltas reach only the child's stream; the primary-thread event pairs at every
  status site with the fixed order; archive rules; the CLI transcript.
- Slice 3: the eight rollup tests (child idle does not idle the session; quiescence idles
  once; `requires_action ≻ end_turn`; `event_ids` union; confirmation for A while B runs;
  child termination leaves the session; the spawn's same-transaction `running` closes the
  enqueue-to-claim window; single-thread reduction — every existing status assertion,
  the interrupt's re-idle and the reclaim pair included, passes unchanged); an idle
  `self_hosted` session with a worker still holding its `tool_exec` idles as today;
  the HITL non-bypass: thread A holds an ask, thread B's allow-policy call enqueues the
  session's `tool_exec`, and the executor runs B's call and **not** A's until A's
  `allow` lands (and the same for the worker's scan and the re-arm predicate); two
  primary `model_turn` enqueues and two `tool_exec` enqueues dedupe under the NULL-thread
  key; wake amplification (three concurrent reports → at most one queued parent turn); a
  report landing mid-turn chains once (by seq, with `processed_at` stamped); a child's
  `end_turn` never starts an outcome evaluation and quiescence does; a
  child's own MCP server is discovered and dialed from the child's list; the
  thread-scoped watermark never stamps a sibling's input; a thread-scoped interrupt
  leaves the shared exec item, drops the late result, and cancels an in-flight call on
  the next keeper beat; a `tool_exec` stopped through the work API with
  platform calls still unanswered leaves a fresh queued item of the right kind
  (`mcp_exec` when an MCP call is among them), one stopped with everything answered
  leaves none, and a stop racing a settlement that appends a call under the session lock
  never leaves the call without a live item.
- Slice 4: a turn calling `create_agent`×3 + `wait_for_agents` produces in one commit
  three thread rows, the twelve projection events (four per spawn) plus the primary's
  `thread_status_idle` for the wait, four `agent.tool_result`s, three child
  `model_turn`s and no `tool_exec`; a turn of only `create_agent`/`send_to_agent` calls
  enqueues the caller's next turn in the same commit; a `wait_for_agents` with no child
  running answers `timed_out:true` and does not park; a child's `submit_result` wakes an
  idle parent exactly once; a client `user.tool_result` naming a delegation call is
  refused; archiving a reported child terminates it with the event; a child's
  request never contains `create_agent`; two consecutive coordinator turns produce
  identical request prefixes up to new content; the spawn that would make a 26th live thread (the 25th child) is an `is_error`; the
  primary cannot be archived. BYOC (decision 13): on a `self_hosted` session the
  session-level list and stream carry a child's allow-policy `agent.tool_use` with its
  `session_thread_id` and, once posted, the `user.tool_result` answering it, while the
  same log on a cloud session shows neither; the worker's scan against a fixture log of
  interleaved sibling calls (a child's use above an older sibling's unanswered use, a
  result between them — the shape the #76 bound strands) returns every unanswered call
  oldest-first, and the single-agent fixture still stops at the trailing turn; a call
  committed during the run is answered by the re-scan; a `user.tool_result` from the
  worker lands on the child's log and wakes that child's turn only; the worker's skills
  set-up materializes the roster union. A `RUN_EVALS` live eval runs a two-worker
  coordinator end to end; the `ant` CLI acceptance below.
- Coverage stays ≥ 90 % over the logic packages; every slice runs `make verify`, the
  verifier, and the dual review.

## New DIVERGENCES entries (inferences to record as they land)

- (slice 1) `multiagent` in `agent_with_overrides` is a 400 — the reference SDK cannot
  send it and the server's response to the stray key is unrecorded (INFERRED, replaces
  the silent drop). Bare-string and versionless roster entries pin eagerly at write time
  (INFERRED from the resolved type's plain `int64`; the roster doc says only "an agent
  ID string"). `self` is exempt from the depth-1 check (INFERRED — read literally the
  rule would forbid the documented feature); the `self` member's snapshot carries the
  session's overrides (documented).
- (slice 2) The primary thread's id is derived from the session id (ours; the reference's
  derivation is unrecorded and ids are opaque). Primary-thread `running`/`idle`/
  `rescheduled` events are emitted on every session, thread event before session event
  within a transition; no primary `terminated` (INFERRED — only `_running` is asserted
  for every session); a child terminates on archive and on the session's end
  (documented) and idles, not terminates, on exhausted retries (INFERRED — the typed
  idle union against the webhooks page's wording). A session's
  pre-slice history holds no primary-thread events. Thread `stats` render the empty
  shape. Thread list default `limit` 1000; thread events list ascending, no `types[]`.
  Cross-posts are one event id on both surfaces, and the primary thread's own list/stream
  is the session view (INFERRED from "the session-level stream is the primary thread");
  a client's `user.tool_confirmation` / `user.custom_tool_result` answering a
  cross-posted call is itself in that view (INFERRED). Previews are thread-scoped on
  every surface (documented for previews; the frame envelope carrying the thread is
  ours). The `session_thread_id` rejection entry at `:37` is rewritten, not deleted.
- (slice 3) Session status is a fold over thread statuses; the idle stop-reason
  precedence `requires_action ≻ retries_exhausted ≻ end_turn` (the ask-over-cap pair is
  documented, `end_turn`'s rank is INFERRED; `budget_reached`'s documented rank
  recorded, not implemented); session-level `event_ids` is the union across threads
  (INFERRED). `user.message` addresses the primary thread (INFERRED — documented only
  for `system.message`; the params carry no thread field). Inbound `session_thread_id`
  on confirmations/results is optional and validated; routing is by tool-use id
  (matches the docs). Exec drivers run only the runnable set — an ask-gated sibling call
  waits for its verdict (INFERRED; the reference runner behaves so client-side). Tool
  execution across sibling threads is serial (matches the reference runner's own
  comment; whether the reference server serializes is INFERRED). MCP servers are
  discovered per thread's agent (ours). Outcome grading runs on the primary at session
  quiescence (INFERRED — the outcome docs predate threads). A thread-scoped interrupt
  does not stop the shared work item; the drivers cancel an in-flight call once its use
  is answered (ours). A `tool_exec` stopped through the work
  API while runnable platform calls remain is re-armed as a fresh item with a fresh
  work id (ours; a poller sees a new item where the reference's long-lived worker never
  stops early — client-visible only to a one-pass worker like ours).
- (slice 4) The six delegation tools' schemas and result payloads (INFERRED, ours);
  delegation calls persist as `agent.tool_use`/`agent.tool_result` on the calling thread's
  own log (INFERRED — the reference's primary-thread event table omits them and its
  behavior is unobserved); `wait_for_agents` is answered immediately and the coordinator
  parks idle (INFERRED, W1); `create_agent` returns the thread id; `send_to_agent`
  addresses by thread id, else by unique name; a `wait_for_agents` with nothing to wait
  for answers `timed_out:true` and does not park (INFERRED); a child's ending condition
  is delivered as `agent.thread_message_received` text (INFERRED); the 25-cap counts
  non-terminated threads, the primary and idle children included (INFERRED); skills materialize as the roster union (ours);
  **the `self_hosted` view rule** — on a `self_hosted` session the session-level
  list/stream surfaces every child thread's `agent.tool_use` and the results answering
  them, beyond the documented "condensed view" (INFERRED: the docs are silent on
  self-hosted multiagent sessions and this is the one reading under which the
  reference's thread-unaware worker can serve child threads; the alternative — the
  reference refuses coordinators on `self_hosted` — turns this into an
  accepts-where-the-reference-refuses entry, never a rewrite); the BYOC worker is one
  pass per work item where the reference's is a stream tail held until `MaxIdle`
  (client-side behavior, recorded beside the #383 entry's precedent, not a wire
  mismatch). All INFERRED items cross-link #78.

## Recording checklist (a real coordinator session, `ant beta:sessions:events stream` + a full events list + `GET /threads`, plus the agent create/update and session create responses that produced it)

In priority order — each settles a decision above:

1. Does `agent.tool_use{name:"create_agent"}` appear in the primary thread's event list?
   (Design C vs a filtered projection.)
2. Does `session.thread_status_idle` appear on the primary between a spawn and the first
   report? (W1 vs W2.) And what a `wait_for_agents` issued with no child running returns.
3. The six tool schemas as the model sees them, and whether `create_agent` returns the
   thread id; how `send_to_agent` addresses one of several copies of an agent.
4. What the coordinator receives when a child errors, is interrupted or archived; whether
   `submit_result` and `send_to_parent` differ (does `thread_status_terminated` follow one?).
5. Ordering of `session.thread_status_running` vs `session.status_running` in one
   transition; whether a single-agent session emits primary `_idle`/`_rescheduled`.
6. Whether a cross-posted event shares its id on both surfaces; whether all child
   `agent.tool_use` are cross-posted or only ask-gated ones.
7. Whether inbound `session_thread_id` on a `user.tool_confirmation` is accepted, and
   `sth_` vs `sthr_` on a real thread id.
8. Whether `multiagent` is supported on `self_hosted` environments at all (`ant
   beta:sessions create` from a coordinator on one), and if so what the session-level
   list/stream shows a worker there — reading (a): every child thread's built-in
   `agent.tool_use` and their `user.tool_result`s, or only the documented blocking
   events (decision 13 i); whether `ant beta:worker` actually serves a child's `bash`
   call; and whether the reference serializes tool calls across sibling threads.
9. What archiving the primary thread does; whether the 25-cap counts the primary and idle-but-unarchived
   threads; whether `retries_exhausted` idles or terminates a child; whether children
   emit `thread_status_terminated` when the session ends.
10. Whether `GET /threads/{primary}/events` equals `GET /events` on a coordinator session
    (cross-posts included), and how `session_thread_id` renders on a child's own stream
    versus the session view; whether a client's `user.tool_confirmation` for a child's
    call appears in the session-level list.
11. Whether a `user.message` sent while children run lands on the primary; whether a
    `self` copy runs the session's overrides; whether an ask-gated call on one child
    blocks a sibling's allow-policy call from executing (the runnable set); whether an
    outcome evaluation can start while children run.
12. From the create/update responses: what `agent_with_overrides` carrying `multiagent`
    returns; whether a bare-string roster entry's `version` in the response moves when
    the member is updated later (eager pin) or not; whether `{type:"self"}` is accepted
    on an agent that itself has `multiagent` set (the depth-1 exemption); whether a
    session update (`agent.mcp_servers`) reaches a child thread already spawned.
13. From a session with one child idle `end_turn` and another `requires_action`, then
    one `retries_exhausted`: the session-level `stop_reason` each time (the rank of
    `end_turn` below `retries_exhausted`) and whether its `event_ids` is the union across
    threads; whether the primary emits `session.thread_status_terminated` when the
    session terminates.

Every entry marked INFERRED above maps to one of these items; an entry with no item is a
plan defect. Entries marked *ours* (the re-arm, the skills union, the primary id
derivation, the frame envelope) are platform choices no recording can confirm or refute,
and have none by design.

## Known consequences, not fixed here

- One hot session row per multiagent session (decision 3) — bounded by the cap.
- Serial tool execution across sibling threads sharing one sandbox: a 25-thread fan-out
  runs its `bash` calls one at a time.
- A coordinator that re-spawns on every report is bounded only by the instantaneous cap.
- Sessions created before slice 2 permanently lack primary-thread events.
- The reference's `pause_turn`-style held-open wait (W2) is not implemented; the
  coordinator's `session.thread_status_idle{end_turn}` while children run is a visible
  difference if the reference holds the thread `running`.
- A `self_hosted` session's session-level view is wider than the documented condensed
  view: a client of such a session sees child tool calls it would not see on cloud, and
  the worker's per-item scan on a coordinator session is O(the session's tool history)
  again — the #76 bound is kept only where its premises hold (single-agent sessions).
- If a recording shows the reference refuses coordinators on `self_hosted`, our
  acceptance stands as a registered divergence rather than being withdrawn.
- The reference runner re-executes an interrupted call on reconnect because the
  interrupt's synthesized `agent.tool_result` is not a type it reads (#429) — a
  `self_hosted` behavior that predates this plan and reaches child threads unchanged;
  likewise it cannot be told to abort an in-flight command a thread-scoped interrupt
  abandoned, so on the reference worker that command runs to completion (our drivers
  cancel it on the next beat).
- An MCP server two roster agents both declare is discovered once per thread.
- A cross-posted child ask on a cloud session is visible in the session view with its
  confirmation; the rest of the child's log is not — a client that wants it reads the
  child's stream, as documented.

## End-to-end acceptance (recorded into docs/HISTORY.md by slice 4)

With the real `ant` CLI against the local server: `ant beta:agents create` a worker and a
coordinator whose `--multiagent` roster names it (`{"type":"coordinator","agents":[…]}`);
`ant beta:sessions create` from the coordinator on a cloud environment; `ant
beta:sessions:events send` a task; `ant beta:sessions:events stream` shows
`session.thread_created` → `agent.thread_message_sent` → cross-posted
`session.thread_status_running` → `agent.thread_message_received` → `session.status_idle`;
`ant beta:sessions:threads list` returns the primary (`parent_thread_id: null`) and the
child; `ant beta:sessions:threads:events stream --thread-id <child> --event-delta
agent.message` previews the child's text; `ant beta:sessions:events send` a
`user.interrupt` with `--event.session-thread-id <child>` interrupts only that thread;
`ant beta:sessions:threads archive` on the idle child succeeds — the child reads
`terminated` with `archived_at` set and the stream shows its
`session.thread_status_terminated` — and on the primary 400s.
Then the same coordinator on a `self_hosted` environment — this platform's behavior
under scope decision 2, whatever a later recording shows the reference does (a recorded
refusal changes the registry entry, not this transcript): `ant beta:worker` (the
reference's own thread-unaware runner) polls the `tool_exec`, sees the child's `bash`
call on the session-level stream with its `session_thread_id`, runs it, posts a plain
`user.tool_result`, and the child's thread stream shows the result landing on the child's
log; a second run with our own `cmd/worker` does the same.
