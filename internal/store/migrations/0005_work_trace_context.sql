-- Trace context for cross-process observability. When a tool_exec work item is
-- enqueued under an active OTel span (a brain turn, or a session-events request),
-- the W3C trace context (traceparent/tracestate) is captured here so the BYOC
-- worker that later runs the item can parent its tool-execution spans on the turn
-- that produced the work — one trace spans the control plane and the worker (the
-- cloud executor will read it likewise once it is traced). A model_turn drives
-- the brain's own span and never carries this, so it stays null there too.
-- It is control-plane-internal: never part of the wire work object's metadata (it
-- rides a poll response header instead), so it does not touch the client-facing
-- metadata namespace. Null when the item was enqueued with no active span.
ALTER TABLE work_items
    ADD COLUMN trace_context jsonb;
