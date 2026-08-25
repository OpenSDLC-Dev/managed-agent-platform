-- Per-item sessions tokens (plan 36 decision 15, #52): the bearer a BYOC
-- worker presents for a work item whose session attaches a memory store. The
-- reference's poll response carries it inside the item's `secret`, and the
-- reference worker then calls the item's heartbeat and stop, its session's
-- routes and the memory routes with it. Only the hash is stored (the
-- environment_keys precedent). A row is inserted in the same transaction as
-- the poll's claim, so an item is never leased without the credential its
-- worker needs. Validity is a set of join conditions rather than a column: a
-- token authenticates while work_items.id still equals work_id — every
-- re-hand-out rewrites the id (#62), which is why work_id is not a foreign
-- key: the id it names is meant to stop existing — the lease is unexpired,
-- the item is not stopped, and the session is unarchived. A superseded row
-- is dead by those conditions and never deleted; a session delete cascades.
CREATE TABLE work_session_tokens (
    id          text PRIMARY KEY,
    work_id     text NOT NULL,
    session_id  text NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    token_hash  text NOT NULL UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Supports the session_id cascade — the session_gate_tokens precedent.
CREATE INDEX work_session_tokens_session_idx ON work_session_tokens (session_id);
