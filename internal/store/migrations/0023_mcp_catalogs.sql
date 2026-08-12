-- The MCP tool catalog and the work kind that fills it
-- (docs/plan/29_mcp-toolset.md). Both land together because neither is usable
-- alone: the catalog is written by exactly one producer, the executor's MCP
-- driver, and that driver is reachable only as an mcp_exec item.

-- mcp_exec: MCP discovery — and, from a later slice, tool execution — runs in
-- the platform executor's own process for cloud AND self_hosted sessions alike,
-- so like web_exec the kind list is the only schema change Claim needs — and
-- Poll never serves it. The CHECK is dropped and re-added under its
-- auto-generated name, the 0015_web_exec_kind pattern.
ALTER TABLE work_items DROP CONSTRAINT work_items_kind_check;
ALTER TABLE work_items ADD CONSTRAINT work_items_kind_check
    CHECK (kind IN ('model_turn', 'tool_exec', 'web_exec', 'outputs_harvest', 'mcp_exec'));

-- One row per (session, MCP server): the tools that server reported, or the
-- failure that stopped it reporting. Written only by the executor's discovery
-- pass; the brain reads it at request assembly from a later slice, and nothing
-- reads it today.
--
-- The row is a per-session snapshot rather than a shared cache keyed by URL:
-- two sessions may reach the same server with different credentials and be
-- shown different tools, and a session's catalog dies with it.
--
-- url is the endpoint discovery actually used, so an agent whose mcp_servers
-- were patched mid-session (one of only two mid-session-mutable agent fields)
-- can be told which rows no longer describe the agent. Invalidation is by
-- deletion in the patch's own transaction, never by a TTL sweep.
--
-- status carries the retry semantics: 'ready' is a usable listing, 'failed'
-- is re-attempted on the next turn (the reference retries on the
-- session.status_idle → session.status_running transition), which is why a
-- failure is a row and not the absence of one — the absence means "never
-- attempted", and the two settle differently. error is the human-readable
-- reason, null on 'ready'.
CREATE TABLE mcp_catalogs (
    session_id  text        NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    server_name text        NOT NULL,
    url         text        NOT NULL,
    tools       jsonb       NOT NULL DEFAULT '[]'::jsonb,
    status      text        NOT NULL CHECK (status IN ('ready', 'failed')),
    error       text,
    fetched_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, server_name)
);
