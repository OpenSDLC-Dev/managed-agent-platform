-- Object keys a delete still owes the object store (plan 50, #645 + #320).
--
-- The request path no longer deletes objects: deleteSession enqueues here in
-- the transaction that removes the rows, and a control-plane sweeper drains it.
-- Unlike deleted_sessions beside it, this table is meant to empty — a row lives
-- exactly as long as its object does, and is deleted when the store confirms
-- the object is gone. So its size is the current backlog, never the history.
--
-- The key is the primary key because the same object must never be owed twice,
-- and because an enqueue that races another (a retried request, two paths that
-- both know the key) has an obvious right answer: it is already owed.
--
-- attempts and last_error are for the operator rather than the sweeper. A key
-- the store permanently refuses is retried forever on a capped backoff and is
-- never dropped, because this row is the only record that the object was not
-- removed; discarding it would restore the very leak this table closes while
-- also hiding it. That makes the table the answer to "what has this deployment
-- failed to delete", which no log line can be.
CREATE TABLE pending_object_deletes (
    object_key      text        PRIMARY KEY,
    enqueued_at     timestamptz NOT NULL DEFAULT now(),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    attempts        integer     NOT NULL DEFAULT 0,
    last_error      text
);

-- The sweeper's only query: the due rows, oldest first. Partial on nothing —
-- every row is a candidate sooner or later — but ordered so a backlog drains
-- in the order it was incurred rather than starving its own tail.
CREATE INDEX pending_object_deletes_due_idx
    ON pending_object_deletes (next_attempt_at, enqueued_at);
