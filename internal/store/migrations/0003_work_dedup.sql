-- One live work item per (session, kind): enqueue is idempotent while a
-- queued/starting/active item exists, so event-append triggers can fire
-- repeatedly without double-scheduling a session's turn.
CREATE UNIQUE INDEX work_items_live_session_kind_idx
    ON work_items (session_id, kind)
    WHERE state IN ('queued', 'starting', 'active');
