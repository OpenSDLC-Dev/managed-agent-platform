---
status: draft
issue: "#53"
---

# Multi-agent session threads — the coordinator topology (plan 35)

This plan closes #53: a session's primary thread orchestrates work by spawning **session
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

Scope decisions **proposed** on 2026-08-17 (each needs the user's confirmation; the
default stands until then):

1. **Bump the SDK pin to v1.63.1 as slice 0.** All managed-agents drift from the pinned
   v1.61.0 landed in one additive release (v1.62.0: advisor, budgets, `session.usage`,
   cost/server-tool usage fields, `redacted` block, `inference_geo`); v1.63.x is bug fixes.
   The v1.61.0 thread surface is a forward-compatible subset (routes, params, events and
   the SSE allowlist are byte-identical, and the thread agent snapshot already carries
   `type:"agent"`), so a bump obligates nothing — but the official docs this plan cites
   describe the v1.62 service, and a plan citing a schema the repo refuses to read is the
   doc/code lag the verifier exists to catch. Alternative if declined: keep v1.61.0 and
   carry the exclusion list of decision 3 verbatim, reading post-pin surface as evidence,
   never as contract (docs/REFERENCE_PROJECTS.md's caveat).
2. **Coordinator sessions are cloud-only in this plan; `self_hosted` refuses them.** A
   coordinator agent on a `self_hosted` environment is rejected at session create with a
   clear error. The reference's own environment worker tails only the session-level
   stream and dispatches serially, and only client-actionable child events are
   cross-posted there — so a child's allow-policy `bash` call would never reach a BYOC
   worker at all; our worker's bounded newest-first scan (`internal/worker/toolexec.go:197`)
   additionally strands sibling threads' calls silently. Whether the reference offers
   multiagent on `self_hosted` is itself unrecorded. A follow-up issue owns BYOC threads.
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
   thread and that `GET /threads` lists it. So threads are not purely additive: existing
   single-agent sessions gain a listable primary thread and, going forward, primary-thread
   status events beside the session ones. Design decision 2 keeps the retro cost to one
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

Out of scope, besides decision 3: per-thread skills/files materialization on BYOC
(moot under decision 2), a fork/inherit-context primitive (the reference has none — every
spawn is fresh), thread-count or wake budgets beyond the documented 25-thread cap
(argued under design decision 8), and `rescheduling` semantics (nothing writes the value
today; the fold ranks it, no code produces it).

## Ground truth (verified 2026-08-17)

Resolved per CLAUDE.md's order: public docs (platform.claude.com/docs/en/managed-agents:
`multiagent-orchestration`, `events-and-streaming` § "Preview session thread events",
`agent-setup`, `sessions`, `budgets`, `webhooks`, `tools`, `reference` — the event
catalog — and the cookbooks `CMA_plan_big_execute_small`, `CMA_coordinate_specialist_team`,
`CMA_watch_subagents_live`, all fetched 2026-08-17) → `anthropic-sdk-go` read at the
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
  (`betasession.go:924-932`): a bare agent-id string ("pins the latest version"),
  `{type:"agent",id,version?}` (`betasession.go:178-197`), `{type:"self"}`
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
- **ID prefix** — `sthr_`: seven "Public `sthr_` ID" doc comments and the fixture
  `sthr_011CZkZVWa6oIjw0rgXZpnBt` in SDK and CLI tests. (The docs' one example says
  `sth_01DEF…`; the SDK is authoritative — recording item.)
- **BYOC** — `BetaSelfHostedWork.Data` is exactly `{id, type:"session"}` at both tags
  (`betaenvironmentwork.go:490-501`), and `betasessiontoolrunner.go` has no thread
  concept: one session-level stream, serial dispatch, `user.tool_result` without a thread
  field.
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
  Cross-posted child `agent.tool_use` carry `session_thread_id`; a child's own
  `agent.message`/thinking/results never reach the session stream; previews are
  per-connection and per-thread; the session feed carries only the primary's
  `span.model_request_end`.
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
  threads' idle reasons disagree: `requires_action` outranks a budget pause, which
  outranks `end_turn` (budgets) — "the actionable reason wins".
- "Archive only succeeds if the thread is `idle`. A thread parked on `requires_action`
  counts as idle"; archiving "frees up a thread against the 25-thread limit". Against a
  child blocked on `requires_action`, an interrupt "closes each pending tool call with an
  error tool result … and re-emits `session.thread_status_idle` with `stop_reason:
  end_turn` directly; the model is not sampled"; against an idle thread it is a no-op.
- `user.message` and `system.message` land on the primary thread only.

## Design decisions

1. **A thread is a row; the primary thread's id is derived from the session's.**
   `session_threads(id sthr_, session_id, parent_thread_id NULL for the primary, agent
   jsonb — the SessionThreadAgent snapshot with type:"agent", agent_name, status, usage
   jsonb, created_at, updated_at, archived_at, org/workspace/project)`. The primary
   thread's id is `sthr_` + the session id's token — deterministic, valid in the id
   alphabet, unique because session ids are, and opaque to clients (ids need not match
   the reference's derivation, which is unrecorded). That makes the primary row
   backfillable in plain SQL for every existing session (`agent` = `resolved_agent`
   minus `multiagent`; status/usage/timestamps from the session row) and lets any brain
   name the primary thread without a lookup. Rejected: minting the primary id (needs a
   Go-side backfill or a lazy insert on GET) and synthesizing the primary on read (needs
   the derivation anyway, and cannot hold the primary's own usage once children exist).
   Thread `stats` render the empty shape, the precedent DIVERGENCES already records for
   session `stats`.
2. **`events.thread_id` NULL means "the primary thread"; the payload carries the wire
   id.** `eventWire` renders from the payload, so a `session.thread_status_running` event
   carries its `session_thread_id` in the payload and no events row is rewritten. Child
   events are stored once with `thread_id = <child>`; the session-level list/stream is
   the view `thread_id IS NULL OR type ∈ {session.thread_created, session.thread_status_*,
   agent.thread_message_received-on-primary, cross-posted agent.*tool_use}` — store once,
   filter per stream. A thread's own list/stream is `thread_id = $tid` (primary:
   `IS NULL`). One event, one `sevt_` id on both surfaces — the reconnect procedure the
   docs describe (seed seen ids from the list, skip duplicates) works unchanged.
   Rejected: dual-writing cross-posts (two rows, two ids, breaks "one event, one id" and
   the append-once invariant). Index `(session_id, thread_id, seq)` in the same migration.
3. **One session-wide `seq` under the session row lock, unchanged.** Threads claim
   concurrently and commit serially on the session row (`commitUnderLock`). This preserves
   I1 (seq/created_at agreement), the SSE `AfterSeq` cursor, the events-list keyset
   cursor, and — decisively — correctness of cross-thread writes: a child's settlement
   appends to the parent's log and reads the parent's outstanding calls; a per-thread lock
   would make that a lost-update race, not merely a different design. Cost: one hot row
   per multiagent session, bounded by the 25-thread cap. Rejected: per-thread seq
   (breaks both cursors and the total order the reference's own primary stream implies).
4. **Session status is a fold over thread statuses; thread status is authoritative.**
   Written in the same transaction that moves a thread's status, under the same lock:
   `terminated` iff the primary is terminated; else `running` iff any non-archived thread
   is running **or holds a live work item** (the disjunct closes the enqueue-to-claim
   window a status-only fold would report as quiescence — the window deepseek-harness
   closes with its `accepted` set); else `rescheduling` iff any thread is; else `idle`.
   Session-level idle `stop_reason` is a precedence pick over the idle threads' reasons —
   `requires_action ≻ retries_exhausted ≻ end_turn` — with `event_ids` the seq-ordered
   union of every thread's outstanding asks. Emit `session.status_*` only on a value
   change, plus the existing payload-only re-idle when the ask set shrinks or grows.
   Order within one transition: **thread event first, session event second** (facts,
   then rollup — the order the reference's budget sequence shows). A single-thread session
   reduces to today's behavior exactly; that reduction is the regression gate.
5. **Turns are (session, thread); exec work stays per session.** `work_items.thread_id`
   (NULL = primary); the live-dedup index becomes `(session_id, thread_id, kind)` so
   sibling threads run `model_turn`s concurrently; the brain's `claimLiveSession` checks
   the **thread's** status; replay, watermark (`MarkProcessedThrough`), `pendingInput` and
   every `toolflow` unanswered-query become thread-scoped (a session-wide watermark would
   stamp a sibling's queued input as processed — a correctness hazard, not a
   refinement). `tool_exec`/`web_exec`/`mcp_exec` remain session-keyed: one shared
   sandbox, one item covers every thread's backlog, execution across threads is serial
   (what the reference's own runner does), and the executor drains **per thread** — it
   wakes each thread's `model_turn` as that thread's calls become answered and re-scans
   before completing the item so a call that arrived under a live item is never
   stranded. Rejected: a thread id on the work item's wire shape (the reference has none;
   `work.data` is `{id, type:"session"}`) and per-thread sandboxes (documented shared;
   the BYOC wire could not even name one).
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
   `is_error` naming the candidates) appends the sent/received pair and wakes an idle
   target; `list_agents` reads the rows; `submit_result` appends the child's
   `agent.thread_message_sent`, `session.thread_status_idle{end_turn}` and the parent's
   `agent.thread_message_received`; `send_to_parent` the same without ending the child's
   turn. Every call is answered by an `agent.tool_result` in the same commit — **Design C:
   the delegation `tool_use`/`tool_result` are persisted on the calling thread's own log
   and the thread events are the client-facing projection.** Replay then needs no new arm
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
   which the documented aggregation sentence is load-bearing — and treats
   `agent.thread_message_received` as **pending input** on the parent thread: idle parent
   → enqueue its turn; mid-turn parent → the running turn's `settle` chains once; the
   `Enqueue` dedup makes N reports cost at most one queued turn plus one chain. Replay
   renders a received message as a user-role text block (Claude Code's task-notification
   shape). A child's terminal condition (interrupted, archived, errored, retries
   exhausted) is delivered the same way, as text naming the outcome and a next action.
   Guard: the primary thread cannot be archived (400; archive the session instead). W2 is
   a later, local change (join predicate in the child's settlement + a sweeper) if a
   recording shows the reference's coordinator thread stays `running` across a wait.
8. **Caps and amplification.** 25 non-archived, non-terminated threads per session,
   enforced by `create_agent` (`is_error` on the 26th); the roster bound (1–20, distinct,
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
   `user.interrupt` without a thread id interrupts every non-archived thread and cancels
   the session's `model_turn` items; with one it interrupts that thread only (`CancelThread`
   on its `model_turn`), synthesizes its error results, re-idles it `end_turn`, and never
   stops the shared exec item — the executor's result append checks answered-ness under
   the lock first (plan 16's one-answer invariant), so a late sandbox result for an
   already-interrupted call is dropped.
10. **Roster resolution and snapshotting is a foundational slice of its own.** Agent
    create/update resolve the roster inside the write transaction: bare strings and
    versionless references pin the member's current version eagerly (the response
    type's `version` is a plain int64 with no `latest` alias — unlike skills); `self`
    resolves to the coordinator's own id and the version the write is about to produce;
    C-1…C-7 checked against the merged spec with `FOR SHARE` on the referenced agents.
    Session create builds `session.agent.multiagent.agents[]` as full
    `SessionThreadAgent` snapshots by fetching each member's pinned version — computed at
    the API boundary and stored in `sessions.resolved_agent`, leaving
    `domain.AgentSpec.Multiagent` untyped (option (a); splitting the type is the bigger
    diff for no runtime gain). `multiagent` inside `agent_with_overrides` becomes an
    explicit 400 instead of today's silent drop.
11. **Skills: materialize the union of the roster's skills once per session.** The roster
    is snapshotted at session create, so the union is known before the sandbox exists;
    the shared filesystem is documented, and each thread's system prompt still injects
    only its own agent's Level-1 metadata. Rejected: per-thread skill directories under a
    shared workdir (the `.materialized` sentinel is session-scoped and the reference
    shares one filesystem anyway).
12. **Primary-thread events on every session, mirroring `running`/`idle`/`rescheduled`;
    never `terminated`.** The catalog asserts `_running` for every session; idle and
    rescheduled follow the SDK's "Emitted on the thread's own stream" doc on all four;
    the webhooks page carves the primary's end out. Sessions written before this plan
    can never gain these events (append-only) — stated in the registry entry, not
    discovered later.

## Slices

Ordered so no landed slice leaves an incoherent state; each is one PR through the full
ritual. Lifecycle per CLAUDE.md: slice 1's PR flips this plan to `in-progress` and takes
over STATE.md; **every slice lands its DIVERGENCES entries in the PR that introduces the
behavior**; the last slice archives the plan and closes #53.

0. **SDK bump v1.61.0→v1.63.1** (only under scope decision 1; the plan-05/11 ritual): pin,
   pairwise diffs, advance the three live version labels, re-read DIVERGENCES' 17
   `v1.61.0` evidence labels, CONFIRMED entries + issues for every excluded v1.62 item
   (advisor, budgets, `session.usage`, usage cost fields, `redacted`, `inference_geo`;
   note the CLI already sends `--budget` and we drop it silently), HISTORY record.
1. **Roster resolution and snapshot** (design decision 10): `agents.go` create/update
   accept and resolve `multiagent`; `sessions.go` `resolveAgent` snapshots full member
   definitions; overrides carrying `multiagent` 400; the "multiagent … rejected" clause of
   DIVERGENCES `:30` is corrected in place. Inert at runtime — no thread machinery
   exists yet, and a coordinator session behaves as a single-agent session.
2. **Thread resource and the primary thread** (decisions 1, 2, 12): migration
   `0025_session_threads.sql` (table, backfill of one primary row per existing session,
   `events (session_id, thread_id, seq)` index); `PrefixSessionThread` in `id.go` +
   CLAUDE.md's prefix line; `events.thread_id` written and read (`NewEvent.ThreadID`,
   `ListQuery.ThreadID`); the five routes (threads list defaults to `limit` 1000, its
   documented default; thread events list asc by seq with the existing seq cursor;
   thread stream = the session broker filtered per subscriber, `event_deltas[]`
   honored; archiving the primary or a non-idle thread is refused with a clear error —
   the reference's status code for the latter is unrecorded); primary-thread status
   events beside the ten `session.status_*` sites (thread
   first, then session); the 20 exact-sequence assertions updated, the 15 exact-key-set
   sites untouched. `ant beta:sessions:threads list|retrieve` and
   `beta:sessions:threads:events list|stream` work against a single-agent session.
3. **Thread execution substrate** (decisions 3, 4, 5, 9): migration `0026_work_thread.sql`
   (`work_items.thread_id`, the widened `model_turn` dedup index replacing
   `work_items_live_session_kind_idx`); brain claims `(session, thread)` turns with
   thread-scoped replay/watermark/pendingInput; thread-scoped `toolflow` queries; the
   status fold and stop-reason pick; the API's trigger arms thread-aware (a confirmation
   for thread A while B runs resumes A; the session never falsely idles); `requireNullThread`
   → accept-and-validate; `CancelThread` + thread-scoped interrupt; executor per-thread
   drain and wake; result-append answered-check. Reachable only through tests until
   slice 4 (a test seam spawns a child row) — the substrate is coherent and the wire is
   unchanged for single-agent sessions.
4. **Coordinator delegation** (decisions 6, 7, 8, 11; scope decision 2): tool injection
   by role, settlement-executed delegation, the thread-event projection, the wake path,
   child-failure delivery, the 25-cap, the primary-archive guard, skills union,
   `self_hosted` refusal at session create, the INFERRED entries for every inferred
   shape, and the end-to-end acceptance transcript. This slice meets #53's acceptance.
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
  `multiagent`; overrides with `multiagent` 400.
- Slice 2: the migration backfill on a fixture with legacy sessions; `GET /threads` on a
  legacy and a new session both list one primary with `parent_thread_id: null` and the
  derived id; the events index; thread events list/stream equal the session's for the
  primary; the primary-thread event pairs at every status site with the fixed order;
  archive rules; the CLI transcript.
- Slice 3: the eight rollup tests (child idle does not idle the session; quiescence idles
  once; `requires_action ≻ end_turn`; `event_ids` union; confirmation for A while B runs;
  child termination leaves the session; the live-work window; single-thread reduction —
  every existing status assertion passes unchanged); wake amplification (three concurrent
  reports → at most one queued parent turn); mid-turn report chains; the thread-scoped
  watermark never stamps a sibling's input; thread-scoped interrupt leaves the shared exec
  item and drops the late result.
- Slice 4: a turn calling `create_agent`×3 + `wait_for_agents` produces in one commit
  three thread rows, the nine projection events, four `agent.tool_result`s, three child
  `model_turn`s and no `tool_exec`; a child's `submit_result` wakes an idle parent
  exactly once; a client `user.tool_result` naming a delegation call is refused; a child's
  request never contains `create_agent`; two consecutive coordinator turns produce
  identical request prefixes up to new content; the 26th spawn is an `is_error`; the
  primary cannot be archived; a `self_hosted` coordinator session is refused; a
  `RUN_EVALS` live eval runs a two-worker coordinator end to end; the `ant` CLI
  acceptance below.
- Coverage stays ≥ 90 % over the logic packages; every slice runs `make verify`, the
  verifier, and the dual review.

## New DIVERGENCES entries (inferences to record as they land)

- (slice 1) `multiagent` in `agent_with_overrides` is a 400 — the reference SDK cannot
  send it and the server's response to the stray key is unrecorded (CONFIRMED, replaces
  the silent drop). Bare-string and versionless roster entries pin eagerly at write time
  (INFERRED from the resolved type's plain `int64`). `self` is exempt from the depth-1
  check (INFERRED — read literally the rule would forbid the documented feature).
- (slice 2) The primary thread's id is derived from the session id (ours; the reference's
  derivation is unrecorded and ids are opaque). Primary-thread `running`/`idle`/
  `rescheduled` events are emitted on every session, thread event before session event
  within a transition; no primary `terminated` (INFERRED — only `_running` is asserted
  for every session). Sessions predating the slice carry no primary-thread events, ever.
  Thread `stats` render the empty shape. Thread list default `limit` 1000; thread events
  list ascending, no `types[]`. Cross-posts are one event id on both surfaces (INFERRED).
  The `session_thread_id` rejection entry at `:37` is rewritten, not deleted.
- (slice 3) Session status is a fold over thread statuses with the live-work disjunct;
  the idle stop-reason precedence `requires_action ≻ retries_exhausted ≻ end_turn`
  (`budget_reached`'s documented rank recorded, not implemented); session-level
  `event_ids` is the union across threads (INFERRED). Inbound `session_thread_id` on
  confirmations/results is optional and validated; routing is by tool-use id (matches the
  docs). Tool execution across sibling threads is serial (matches the reference runner's
  own comment; whether the reference server serializes is INFERRED). A thread-scoped
  interrupt does not stop the shared work item.
- (slice 4) The six delegation tools' schemas and result payloads (INFERRED, ours);
  delegation calls persist as `agent.tool_use`/`agent.tool_result` on the calling thread's
  own log (INFERRED — the reference's primary-thread event table omits them and its
  behavior is unobserved); `wait_for_agents` is answered immediately and the coordinator
  parks idle (INFERRED, W1); `create_agent` returns the thread id; `send_to_agent`
  addresses by thread id, else by unique name; a child's terminal condition is delivered
  as `agent.thread_message_received` text (INFERRED); the 25-cap counts non-archived,
  non-terminated threads (INFERRED); skills materialize as the roster union (ours);
  coordinator sessions are refused on `self_hosted` (CONFIRMED, tracked by a new issue).
  All INFERRED items cross-link #78.

## Recording checklist (a real coordinator session, `ant beta:sessions:events stream` + a full events list + `GET /threads`)

In priority order — each settles a decision above:

1. Does `agent.tool_use{name:"create_agent"}` appear in the primary thread's event list?
   (Design C vs a filtered projection.)
2. Does `session.thread_status_idle` appear on the primary between a spawn and the first
   report? (W1 vs W2.)
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
8. Whether `multiagent` is supported on `self_hosted` environments (`ant beta:worker poll`
   against a coordinator session), and whether the reference serializes tool calls across
   sibling threads.
9. What archiving the primary thread does; whether the 25-cap counts idle-but-unarchived
   threads; whether `retries_exhausted` idles or terminates a child.

## Known consequences, not fixed here

- One hot session row per multiagent session (decision 3) — bounded by the cap.
- Serial tool execution across sibling threads sharing one sandbox: a 25-thread fan-out
  runs its `bash` calls one at a time.
- A coordinator that re-spawns on every report is bounded only by the instantaneous cap.
- Sessions created before slice 2 permanently lack primary-thread events.
- The reference's `pause_turn`-style held-open wait (W2) is not implemented; the
  coordinator's `session.thread_status_idle{end_turn}` while children run is a visible
  difference if the reference holds the thread `running`.
- BYOC coordinator sessions are refused; the worker's bounded scan and per-thread skills
  stay unaddressed until that issue is picked up.

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
`ant beta:sessions:threads archive` on the idle child succeeds and on the primary 400s.
