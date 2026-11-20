---
status: archived
issue: "#1004"
---

# Bind pagination cursors to their list (plan 71)

The 2026-10-24 tool-name page-token recording found three requests that
the reference refused with 400 and this platform accepted with 200. In each
case a different list had issued the cursor. The 2026-10-31 cursor-parent
binding recording extends the evidence to 42 cross-endpoint requests, 18
working continuations, and both directions of six cross-parent pairs.

## Decisions

1. Tag each opaque cursor with the list that issued it and keep its position,
   direction and sort key. Validate the list before using the position in a
   query. The public request and response fields stay the same. A new format
   version makes the source mandatory. The owner confirmed that the platform
   has no existing deployments to support, so the old unbound format is
   rejected. A client holding an old token restarts at the first page.
2. Match the recorded error text for the cross-endpoint pairs. For an
   unrecorded pair, use the target list's existing foreign-token refusal.
   Keep malformed input and invalid limits distinct from a valid cursor
   issued for another list.
3. Scope position-based cursors to a list family, without binding the parent
   in vault credentials, agent versions, skill versions, memories, or
   memory versions. Those queries still select only rows of the requested
   parent. Bind event cursors to their thread using the cursor event ID in
   the recorded refusal. A session's event list belongs to its primary
   thread. The thread-event endpoint uses its own list tag.

## Acceptance

Create and paginate resources through HTTP against PostgreSQL. Replay
server-issued tokens on the original list and across the recorded endpoints.
Check status and error text, including both directions of the session-event
case. For the other five nested families, check that an accepted token lists
only rows of the requested parent. Existing pagination controls cover
backward session pages, event direction, and memory path depth.

Evidence is recorded in [the original probes](https://github.com/OpenSDLC-Dev/managed-agents-wire-recordings/tree/main/2026-10-24-tool-name-page-tokens)
and [the cross-list and cross-parent matrix](https://github.com/OpenSDLC-Dev/managed-agents-wire-recordings/tree/main/2026-10-31-cursor-parent-binding).
