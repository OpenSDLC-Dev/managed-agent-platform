---
status: in-progress
issue: "#375"
---

# External tool waits and ordered resumption

Custom results, confirmations and self-hosted results participate in the same
thread-local tool flow. Receipt is durable and prevents a duplicate answer;
processing advances in the order the model emitted calls. A result received
ahead of an earlier call remains unprocessed until that call settles.

## Decisions

- Emit requires_action for unresolved custom calls, confirmation gates and
  self-hosted sandbox results. Premint their event IDs; do not include platform
  Web/MCP or inline delegation calls as worker-result waits.
- Derive flow from the existing event log under the session row lock. API input,
  selective processing timestamps, thread/session transitions and queue effects
  commit together. No new public fields, tables, or migration.
- Cloud execution cannot pass an unresolved earlier call on the same thread.
  A self-hosted worker may execute authorized calls early; the platform still
  processes its results in model order. Approval alone leaves its result wait.
- A thread runs while executing platform tools and idles while waiting for
  external input. Session status folds over live threads. Model work resumes
  once all of that thread's calls have settled, including queued results.
- Processing timestamps are not the model's consumption cursor: preserve the
  replay-watermark check for inputs arriving during a model request.
- Interrupt/archive preserve received answers and cancel the remaining work;
  late results cannot restart a closed thread. External waits protect a cloud
  sandbox from idle reclamation. Harvest must not race runnable tool work.
- Publish completed platform results durably as today. Do not invent a
  publication barrier or timer: the reference can publish a cloud result before
  the last custom reply, and the cause of its observed delay is unknown.
- Retain null for unprocessed timestamps and existing event-list ordering;
  record these separately from the state-machine convergence. Duplicate POST
  response semantics (#58) and Work API lifetime (#78) remain separate.

## Evidence and acceptance

Reference recordings: mixed tools at recordings commit 146c57d (PR #17), plus
2026-09-19-custom-order-followup/setup.json: ask-first allow [17], deny [39],
worker-first [32], alpha/bash/beta [42,46,47,57], reversed custom replies
[76,77,83,84,87]. The follow-up is sealed at commit 16d24be5acdb0ee4729b8051b5363ce17991ed06 (PR #18).
These observations justify the local ordered processor, not a claim about the
reference service's internal implementation.

Exercise real brain, HTTP API, queue and SSE: twelve cookbook-style custom
decisions; partial and reverse replies; both generated orders for approvals and
worker results; the three-call cloud case; child routing and sibling isolation;
concurrent final replies, restart, interruption and late results. Keep ordinary
cloud tools and Console consumers working. Run make verify and applicable CI,
registry check, independent verification and both required reviews.

Ship a running/idle-compatible worker first, then coordinate the controlplane,
brain and executor cutover. Preserve legacy running-result resumption without
bulk rewriting historical logs; acceptance uses fresh sessions.
