-- Thread execution substrate (plan 35 decisions 4, 5 and 14): turns are
-- (session, thread), the session's status folds over its threads', and MCP
-- catalogs are per thread's agent.

-- A model_turn belongs to one thread (NULL = the primary); the exec kinds —
-- tool_exec, web_exec, outputs_harvest, mcp_exec — stay session-keyed: one
-- shared sandbox, one item covers every thread's backlog. The live-dedup index
-- gains the thread column so sibling threads run model_turns concurrently
-- while each thread still has at most one live turn. NULLS NOT DISTINCT is
-- what keeps 0003's dedup for the primary's turn and every exec item — under
-- the default two (sesn, NULL, model_turn) rows would never conflict.
ALTER TABLE work_items ADD COLUMN thread_id text REFERENCES session_threads(id) ON DELETE CASCADE;
DROP INDEX work_items_live_session_kind_idx;
CREATE UNIQUE INDEX work_items_live_session_thread_kind_idx
    ON work_items (session_id, thread_id, kind) NULLS NOT DISTINCT
    WHERE state IN ('queued', 'starting', 'active');
-- The thread FK is checked on every thread-row delete (the session cascade):
-- indexed, the session_threads_parent_idx precedent.
CREATE INDEX work_items_thread_idx ON work_items (thread_id) WHERE thread_id IS NOT NULL;

-- Each thread declares its own MCP servers (its agent's mcp_servers), so a
-- listing is per thread: the key widens the same way, NULL for the primary.
-- A server two threads both declare is discovered once per thread.
ALTER TABLE mcp_catalogs ADD COLUMN thread_id text REFERENCES session_threads(id) ON DELETE CASCADE;
ALTER TABLE mcp_catalogs DROP CONSTRAINT mcp_catalogs_pkey;
CREATE UNIQUE INDEX mcp_catalogs_thread_server_idx
    ON mcp_catalogs (session_id, thread_id, server_name) NULLS NOT DISTINCT;
CREATE INDEX mcp_catalogs_thread_idx ON mcp_catalogs (thread_id) WHERE thread_id IS NOT NULL;

-- The thread's idle stop reason, beside its status: the session's idle
-- stop_reason is a precedence pick over its idle threads' (decision 4), read
-- from here in the transaction that moves a thread. NULL unless idle. A
-- primary row 0025 backfilled idle learns the reason its session last
-- advertised, so the first fold over it does not read an idle thread with no
-- reason at all.
ALTER TABLE session_threads ADD COLUMN stop_reason jsonb;
UPDATE session_threads t
   SET stop_reason = (SELECT e.payload->'stop_reason' FROM events e
                       WHERE e.session_id = t.session_id AND e.type = 'session.status_idle'
                       ORDER BY e.seq DESC LIMIT 1)
 WHERE t.parent_thread_id IS NULL AND t.status = 'idle';
