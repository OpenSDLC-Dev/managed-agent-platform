-- 0008_files: the Files API registry (docs/plan/08_files.md, slice 1).
--
-- File bytes live in object storage (internal/blob, key layout files/{file_id});
-- this table holds the wire-visible metadata so a list or metadata request never
-- reads an object, and (later) session-resource materialization resolves a mount
-- from a row. Files are immutable — no version table, no update; the lifecycle is
-- upload and hard delete (the reference has no file archival, unlike sessions).

CREATE TABLE files (
    id           text PRIMARY KEY, -- file_…
    org_id       text NOT NULL DEFAULT 'default',
    workspace_id text NOT NULL DEFAULT 'default',
    project_id   text NOT NULL DEFAULT 'default',
    filename     text NOT NULL,
    mime_type    text NOT NULL,
    size_bytes   bigint NOT NULL,
    -- false for every user upload; the seam for session- or tool-produced
    -- downloadable outputs (post-v1, docs/plan/08_files.md non-goals). The
    -- download endpoint gates on this column.
    downloadable boolean NOT NULL DEFAULT false,
    -- The scoping resource a file was created in the context of (e.g. a session):
    -- NULL for a plain upload. scope_id backs the list's ?scope_id= filter; nothing
    -- sets these yet (no session-generated files exist in v1).
    scope_type   text,
    scope_id     text,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- The list's ?scope_id= filter. Partial: only scoped files carry a scope_id, so
-- the plain-upload rows (the common case in v1) stay out of the index.
CREATE INDEX files_scope_id_idx ON files (scope_id) WHERE scope_id IS NOT NULL;
