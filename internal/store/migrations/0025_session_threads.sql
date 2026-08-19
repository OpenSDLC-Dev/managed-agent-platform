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
    parent_thread_id text,                                 -- NULL for the primary; FK below
    org_id           text NOT NULL DEFAULT 'default',
    workspace_id     text NOT NULL DEFAULT 'default',
    project_id       text NOT NULL DEFAULT 'default',
    agent            jsonb,                                 -- SessionThreadAgent snapshot; NULL on the primary
    agent_name       text NOT NULL,
    status           text NOT NULL CHECK (status IN ('idle', 'running', 'rescheduling', 'terminated')),
    usage            jsonb NOT NULL DEFAULT '{}',
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    archived_at      timestamptz,
    -- parent_thread_id is the one discriminator readers use; the agent column
    -- follows it — a child row always carries its snapshot, the primary never
    -- does — so no reader can render one kind as the other.
    CHECK ((parent_thread_id IS NULL) = (agent IS NULL)),
    -- A parent is a thread of the same session: the FK is composite so a child
    -- cannot hang off another session's thread. The UNIQUE it references is
    -- implied by the primary key and exists only to be referenced.
    UNIQUE (session_id, id),
    FOREIGN KEY (session_id, parent_thread_id) REFERENCES session_threads (session_id, id)
);
-- One primary per session is a schema fact, not a convention.
CREATE UNIQUE INDEX session_threads_primary_idx ON session_threads (session_id) WHERE parent_thread_id IS NULL;
-- The threads list is keyed by creation order.
CREATE INDEX session_threads_session_idx ON session_threads (session_id, created_at, id);
-- The self-reference is checked on every row delete (a session's cascade
-- removes its primary and children in one statement); without this index
-- each check is a scan of the whole table — the work_items precedent (0001).
CREATE INDEX session_threads_parent_idx ON session_threads (parent_thread_id);

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
-- A child thread's own view reads `thread_id = $tid` in seq order; the session
-- view's predicate (thread_id IS NULL OR cross_posted) walks the existing
-- (session_id, seq) key. Partial, because no row written before this
-- migration has a thread_id: the build is then a scan that writes nothing,
-- where a full index over a large events table would hold CREATE INDEX's
-- SHARE lock — blocking every append — for the rest of the migrator's single
-- transaction (the 0013 note).
CREATE INDEX events_thread_seq_idx ON events (session_id, thread_id, seq) WHERE thread_id IS NOT NULL;
