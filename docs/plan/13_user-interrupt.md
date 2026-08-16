---
status: archived
issue: https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/68
---

# `user.interrupt` — the escape hatch for a turn nothing will finish

> Archived on landing: the decisions below and the code that implements them are one PR.
> The narrative is in CHANGELOG.md, the rejected alternatives in
> [docs/history/2026-07.md](../history/2026-07.md) § "`user.interrupt` semantics (plan 13) — wire
> resolution and rejected alternatives", the new inference in
> [docs/DIVERGENCES.md](../DIVERGENCES.md). **"The gap" and "What has to be resolved" below
> describe the state of the repository *before* that PR** — read them as the argument for
> the design, not as a description of the result. "Design", "Acceptance" and "Known
> consequences" describe what landed.

## Why this needs a plan file

Not for its size — it is one PR — but because it is the first change that lets one API
request take work away from a live claimant, and because the wire-visible outcome of an
interrupt was unresolved. Both are decisions a reviewer should be able to read rather than
reconstruct from a diff.

## The gap

`user.interrupt` is accepted, validated and appended (`internal/events/inbound.go`), and
then nothing acts on it: the control plane's state machine (`internal/api/events.go`) has
no case for it, and the brain's replay skips it as "state, not conversation".

So a session suspended on a tool whose result never arrives stays `running` forever — a
`self_hosted` session whose BYOC worker never comes back, a custom (client-executed) tool
the client never answers. The mirror case is a session stranded `idle` with a tool use
unanswered (a pre-#181 log): a `user.message` is refused by the resume gate rather than
replaying a request the model protocol rejects, and no later tool result revives it (that
trigger needs a running session). One escape hatch has to cover both, plus the third dead
end that behaves the same way: an idle session on `requires_action`, waiting for a
confirmation nobody will send.

## What has to be resolved

### 1. What the wire says an interrupt does — resolved from public docs

The typed schema cannot answer it. `BetaManagedAgentsUserInterruptEvent` carries only
`{id, type, processed_at, session_thread_id}`, and `session.status_idle`'s `stop_reason`
union is exactly `end_turn | requires_action | retries_exhausted` — no interruption
variant. The `ant` CLI never sends one.

The public docs (authority #1 in CLAUDE.md's resolution order) do answer it, on
*Session event stream*: an interrupt "stops the agent mid-execution", and

> The interrupted turn ends with a `session.status_idle` event whose `stop_reason` is
> `end_turn`, the same value as a turn that finishes on its own; there is no stop reason
> specific to interruption.

Two more facts come with it. The documented way to steer a running agent is **one send**
carrying `[{user.interrupt}, {user.message}]` — the interrupt stops the turn and the
message redirects it. And an interrupt is the documented prerequisite for updating,
archiving or deleting a `running` session, so it must genuinely leave the session not
running.

What the docs do *not* say is what happens to the tool calls the interrupted turn left
outstanding. That stays an inference, recorded in docs/DIVERGENCES.md.

### 2. What answers a dangling `tool_use`

It has to be answered by something, or the interrupt trades one dead end for another: the
log is append-only and every later turn replays it, so an unanswered `tool_use` makes
every future request one the model protocol rejects.

The answer follows the precedent already in the tree — a denied `user.tool_confirmation`
synthesizes an `is_error:true` result carrying the deny message — with the rule that
`internal/events/toolflow.go` already states for denials: **the result's family must match
the call's**. `agent.tool_use` → `agent.tool_result`, `agent.custom_tool_use` →
`user.custom_tool_result`, `agent.mcp_tool_use` → `agent.mcp_tool_result`, each keyed by
its own reference field. A result of the wrong family is not the answer a client watching
that tool is waiting for.

Answering a gated call without a confirmation is a state the API previously refused to
create, so two checks have to learn about it: `UnconfirmedAskEvents` must stop counting an
answered ask as blocking (or the gate outlives the call and wedges the resume it was meant
to release), and `ValidateToolConfirmations` must refuse a confirmation for an answered
call (or a later denial writes a second result for it).

### 3. What stops a claimant that is still working the interrupted turn

This is the part with teeth. A brain mid-stream, or an executor mid-tool, will try to
commit against the turn the interrupt just ended — duplicate tool results, or fresh tool
intents committed onto a session that is now idle, which is #181 re-opened.

The design uses the ownership proof the queue already has. Every claimant re-asserts its
lease inside the transaction that commits its work (`Complete`, `Requeue`, `Assert`), so
cancelling the session's work items **under the session row lock the interrupt already
holds** makes the claimant's whole settlement roll back. Nothing half-committed, no new
code path in the brain or the executor, and the two orderings both resolve: interrupt
first and the claimant commits nothing; claimant first and the interrupt answers whatever
it left outstanding.

The alternative — leaving the items alone and teaching the brain to notice an interrupt at
settlement time — is rejected in docs/history/2026-07.md.

## Design

`POST /events`, in the transaction that already holds the session row lock, when the batch
carries a `user.interrupt`:

1. List the tool calls still outstanding, treating the batch's own results as answers.
2. If the session is `running`, or anything is outstanding, **settle**: append one
   family-matched `is_error:true` result per outstanding call, append
   `session.status_idle{stop_reason:{type:"end_turn"}}`, move the column to `idle` (only
   from `running` — a stranded or gate-blocked session is already there), and cancel every
   live work item.
3. If the batch also carries a `user.message`, append `session.status_running`, move the
   column back, and enqueue the redirect turn — the documented one-send steer.
4. Otherwise nothing: an idle session with nothing outstanding has no turn to end, and a
   `session.status_idle` there would announce a transition that did not happen.

Steps 2 and 3 apply only to the two statuses v1 ever writes, `idle` and `running`.
`terminated` has ended and the redirect must not revive it — the `user.message` trigger
guards the same thing by requiring `idle` — and `rescheduling` is a state nothing writes,
so settling it would invent semantics.

## Known consequences, not fixed here

**Stopping is not instant.** Cancelling the item is what makes the claimant's work
uncommittable, immediately; what actually tears the work down is the lease keeper noticing
its next `Extend` fail, which it attempts at TTL/3 (≈40s on the 2-minute default). So an
interrupted model stream can keep generating tokens for up to that long, and an
interrupted tool keeps running in its sandbox with whatever side effects it has left. The
outcome is unaffected either way — nothing the claimant *settles* can commit — and
cancelling faster means a wake-up channel from the control plane to a claimant, which the
pull protocol deliberately does not have. The cancel binds settlements, not every append:
`agent.thinking` and `span.model_request_start` are written mid-stream without a lease
proof, so an interrupted turn can still land a stray thinking event after its
`session.status_idle`. Replay reconstructs nothing from either, and both are already
registered as the brain's crash-window residue in docs/DIVERGENCES.md.


Cancelling a live `model_turn` makes the brain's settlement fail its lease proof, which the
brain reports the way it reports any lost claim: a red `model_turn` span and a
`turn failed` log. That signal is deliberate and tested (`internal/brain/telemetry_test.go`)
for the reclaim case, and the queue carries no reason code that would let the brain tell a
cancelled item from a reclaimed one. Distinguishing them is queue surface beyond this
issue. The same rollback also means an interrupted turn's `span.model_request_start` is
left with no matching `span.model_request_end` — a divergence, recorded.
