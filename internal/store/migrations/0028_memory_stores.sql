-- Memory stores (docs/plan/36_memory-stores.md slice 1, decisions 1-3, #52):
-- the /v1/memory_stores management surface. A store is a named container for
-- agent memories; the memories and the immutable versions behind them are
-- slice 2's own tables, which reference this one.

CREATE TABLE memory_stores (
    id           text PRIMARY KEY,
    -- Reserved multi-tenant scope columns, single-tenant defaults (CLAUDE.md
    -- principle 5), as on every resource table.
    org_id       text NOT NULL DEFAULT 'default',
    workspace_id text NOT NULL DEFAULT 'default',
    project_id   text NOT NULL DEFAULT 'default',
    name         text NOT NULL,
    -- "Empty string when unset" on the wire, never null.
    description  text NOT NULL DEFAULT '',
    metadata     jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- Audit only: which API key / principal created the store. Never part of
    -- the wire schema, never used for isolation — sessions.created_by's rule
    -- (0001), and plain text for the reason 0022 gives.
    created_by   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- Advances when name, description or metadata changes; memory writes
    -- inside the store do not advance it.
    updated_at   timestamptz NOT NULL DEFAULT now(),
    -- Set once and never cleared: archiving is one-way.
    archived_at  timestamptz
);

-- The list is newest-first, keyset-paged on (created_at, id).
CREATE INDEX memory_stores_created_idx ON memory_stores (created_at DESC, id DESC);
