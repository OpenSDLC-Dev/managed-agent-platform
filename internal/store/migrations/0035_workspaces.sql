-- 0035_workspaces: the workspace registry (plan 42 slice 1, #56).
--
-- Every scoped table has carried workspace_id 'default' since 0001, with no
-- table saying what that value names. This adds the one that does — and it
-- REGISTERS the default workspace rather than creating it: the row below
-- describes rows that already exist, so no existing row changes and nothing
-- needs a backfill. The workspace is the isolation unit; org_id rides along
-- because the pair is what a credential resolves and what the response headers
-- carry, and project_id is not here at all, having no registry of its own.
--
-- UNIQUE (org_id, id) beside the primary key is deliberate redundancy: id is
-- already globally unique, and the pair is what every scoped foreign lookup
-- will name, so the composite reference has an index to land on.
--
-- The reservation comments at 0001_init.sql:6-10, 0013_key_rotation_one_live.sql:43-46
-- and 0022_principals.sql:39-42 — "single-tenant defaults", "a scope it does not
-- implement", "reserved scoping columns" — are historical text from here on.
-- They are not corrected in place because a merged migration is immutable:
-- editing one silently diverges every database that already ran it.

CREATE TABLE workspaces (
    id         text PRIMARY KEY,
    org_id     text NOT NULL DEFAULT 'default',
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- Archiving is a tombstone, not a delete (plan 42 §6.9): an archived
    -- workspace stops resolving credentials and keeps its rows.
    archived_at timestamptz,
    UNIQUE (org_id, id)
);

INSERT INTO workspaces (id, org_id, name) VALUES ('default', 'default', 'Default Workspace');
