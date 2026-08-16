---
status: archived
issue: "#222"
---

# One answer per tool call

Archived: completed — implemented by the PR that landed this file (a
single-PR plan: the same PR starts and finishes the work, so the file lands
archived; the delivery record is the CHANGELOG entry and docs/HISTORY.md).

## Problem

The event log can come to carry **two answers for one `tool_use_id`**, and the
log is append-only, so the session is poisoned permanently: every later replay
assembles both results into the provider request, and a Messages endpoint
rejects a duplicate `tool_result` — the session errors on every subsequent
turn ([#222](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/222)).

Reference-shape validation already exists and closes most of the surface:
`events.ValidateToolResults` (called inside the POST /events transaction,
`internal/api/events.go`) rejects a result whose reference is unknown, already
answered, of the wrong family, duplicated within the batch, or awaiting
confirmation. What it cannot close is the **race the issue names**: the
executor's web pass scans the unanswered set, runs the tools, and commits the
`agent.tool_result` events in a later transaction (`internal/executor/webwork.go`
`processWeb` → `runWebTools` → `AppendWith`). A client `user.tool_result` for
the same call posted **during that window** references a call that is still
unanswered — validation passes, the client's answer commits first, and the
executor's settlement then appends the second answer unchecked.

## Reachability

Narrowed by triage (recorded on the issue, verified against source); the issue
body's "both paths affected equally" is wrong:

- **Sandbox pass (`tool_exec`) is structurally exempt.** `queue.Claim` serves
  `tool_exec` to the platform executor only for `cloud` environments
  (`internal/queue/queue.go` — `AND (w.kind <> 'tool_exec' OR e.kind =
  'cloud')`), and on `cloud` the control plane rejects client
  `user.tool_result` outright (`internal/events/inbound.go`). One writer per
  sandbox call, always.
- **Interrupt vs. a live run is already safe.** `user.interrupt` answers the
  outstanding set and cancels live work items in one commit; the executor's
  settlement calls `queue.Complete` inside its append transaction, and
  `Complete` demands `state = 'active'` with the caller's own lease — the
  cancelled item fails the proof (`ErrLeaseLost`) and the whole settlement
  rolls back. The same proof covers a reclaiming second executor.
- **The permission gate never overlaps a live run.** A turn with any
  `always_ask` intent enqueues no work (`internal/brain/brain.go`, the
  `askIDs` branch); work appears only when the confirmation clearing the last
  gate enqueues it, and a denial synthesizes its result in that same API
  transaction.
- **The one reachable double-write:** a **self_hosted** session's web calls.
  `web_exec` is claimed by the platform executor for cloud and self_hosted
  alike (web tools are platform-executed — plan 15), while self_hosted also
  accepts client `user.tool_result`. The client and the web driver are two
  legitimate writers for the same `agent.tool_use`, and nothing marks the
  call as platform-owned.

## Decision: mark web calls platform-owned at the validation layer

The fork recorded on the issue was a commit-time answered-set re-diff in the
executor's settlement vs. an API-layer rejection scoped to web tool names.
**Chosen: the API-layer rejection**, as one new arm in the existing
`ValidateToolResults`: a `user.tool_result` whose referenced `agent.tool_use`
names a web tool is rejected with the same wire 400 as every other reference
violation. Scoped by `toolset.IsWebTool` — the one predicate the executor's
scan, the worker's, and the queue-kind decisions already consult — and
injected by the API as a function value, keeping `internal/events` free of a
toolset (and transitively sandbox) dependency. It must never name the sandbox
six: a self_hosted worker answering those via `user.tool_result` **is** the
BYOC pull protocol.

Why not the re-diff: with the web arm in place, the executor's
scan-to-commit window has no second writer left — clients are rejected at
the door, interrupts and rival executors already fail the lease proof — so a
commit-time re-diff would guard an unreachable state ("no error handling for
impossible states"). It also has the wrong semantics for a platform-owned
call: it silently *drops* one of the answers instead of telling the poster,
letting a client-fabricated result stand for a call the platform was asked to
execute, and hiding exactly the client bug the 400 makes loud.

Consequences accepted deliberately:

- A late worker result for an interrupt-answered call stays a 400 (the
  pre-existing "already has a result" arm): accepting it would poison the
  session, and the worker's own settlement had already failed with the
  cancelled item.
- A self_hosted operator can no longer "answer through" a stuck web call by
  hand; the documented escape for a wedged session remains `user.interrupt`,
  which answers everything and cancels the work item.

## Wire inference

The reference's behavior when a client answers a web call is **unobserved** —
public docs checked 2026-08-01 (managed-agents self-hosted-sandboxes, tools,
events-and-streaming: no validation rules documented), and the SDK types
carry no error shape for it. The reference's own worker would never send one,
and the mechanism is its per-name tool registration, not any enqueue
ordering: the SDK's session tool runner dispatches every `agent.tool_use` it
observes but posts no result for a name it has no tool registered for — the
official toolset registers only the six sandbox tools — logging "tool not
owned by this runner; leaving the tool_use_id pending for its owner", exactly
what the plan-15 acceptance run recorded (docs/HISTORY.md). The rejection
lands as an INFERRED entry in docs/DIVERGENCES.md, alongside the
already-recorded inference that reference validation rejects bad result
references at all.

## Tasks

1. `internal/events/toolflow.go`: `ValidateToolResults` gains a
   `platformOwned func(string) bool` parameter (nil = no ownership check) and
   reads the referenced call's `name`; a matching `agent.tool_use` reference
   is rejected after the answered check and before the confirmation check.
   TDD in toolflow_test.
2. `internal/api/events.go`: pass `toolset.IsWebTool`. TDD: on a self_hosted
   session, a client `user.tool_result` for an unanswered `web_fetch` call is
   a 400 naming the tool; a sandbox call's result stays accepted; after the
   platform's own answer lands, a retry reads "already has a result".
3. Docs in the same PR: CHANGELOG entry; DIVERGENCES INFERRED entry;
   ARCHITECTURE rows (`toolflow.go` validation arm, webwork one-writer note);
   HISTORY progress summary (this plan archives at birth); STATE.md task tick.
