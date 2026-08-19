-- Session threads (plan 35, decisions 1 and 2): every session has one primary
-- thread and, once a coordinator runs, child threads spawned from its roster.
--
-- A thread is a row. The primary's id is derived from the session's (sthr_ +
-- the session id's token) so it is backfillable here and any brain can name
-- it without a lookup; its agent column stays NULL — readers render it from
-- sessions.resolved_agent (minus multiagent), so a session update never
-- leaves a stale duplicate. Child rows hold their spawn-time snapshot.
-- session_id cascades like every sibling table: sessions are hard-deleted.
CREATE TABLE session_threads (
    id               text PRIMARY KEY,
    session_id       text NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    parent_thread_id text REFERENCES session_threads(id),  -- NULL for the primary
    org_id           text NOT NULL DEFAULT 'default',
    workspace_id     text NOT NULL DEFAULT 'default',
    project_id       text NOT NULL DEFAULT 'default',
    agent            jsonb,                                 -- SessionThreadAgent snapshot; NULL on the primary
    agent_name       text NOT NULL,
    status           text NOT NULL CHECK (status IN ('idle', 'running', 'rescheduling', 'terminated')),
    usage            jsonb NOT NULL DEFAULT '{}',
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    archived_at      timestamptz
);
-- One primary per session is a schema fact, not a convention.
CREATE UNIQUE INDEX session_threads_primary_idx ON session_threads (session_id) WHERE parent_thread_id IS NULL;
-- The threads list is keyed by creation order.
CREATE INDEX session_threads_session_idx ON session_threads (session_id, created_at, id);

-- Backfill: one primary row per existing session, mirroring the session row.
INSERT INTO session_threads (id, session_id, org_id, workspace_id, project_id, agent_name,
                             status, usage, created_at, updated_at, archived_at)
SELECT 'sthr_' || split_part(id, '_', 2), id, org_id, workspace_id, project_id,
       COALESCE(resolved_agent->>'name', ''), status, usage, created_at, updated_at, archived_at
  FROM sessions;

-- Events are stored once and filtered per surface (decision 2): thread_id is
-- the emitting child thread (NULL = the primary), and cross_posted marks a
-- child's row the session-level view — which is the primary thread's own —
-- also surfaces (a child's status events, an ask-gated call and its answer).
ALTER TABLE events ADD COLUMN cross_posted boolean NOT NULL DEFAULT false;
CREATE INDEX events_thread_seq_idx ON events (session_id, thread_id, seq);
