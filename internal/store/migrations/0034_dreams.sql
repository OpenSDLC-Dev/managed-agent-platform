-- Dreams (docs/plan/41_dreams.md slice 1, §6): the /v1/dreams management
-- surface. A dream is an asynchronous memory-consolidation job that reads one
-- memory store plus a set of session transcripts and writes consolidated
-- memories into an output store — a clone by default, the input store itself
-- under output_behavior.type = update_existing.
--
-- Slice 1 serves the routes over this table with no runner behind it: a created
-- dream stays 'pending' until slice 2 lands. Several columns therefore have no
-- reader yet, and ship now because a migration is immutable once merged —
-- adding them later would mean a second file for a schema already designed:
--
--   stage                   the pipeline position slice 2's tick resumes from.
--   attempts                slice 2's start claims, bounded by dreamStartAttempts.
--   closed_at               "nothing left to do", stamped by slice 2's finishing
--                           arm; the sweep and the hold index below read it.
--   session_id              the pipeline session slice 2 creates.
--   target_memory_store_id  the update_existing target, set from slice 4 on;
--                           until then no row sets it, so the hold index below
--                           can constrain nothing (§5.3).
--   agents.internal /       slice 2's hidden runner-owned agent and environment.
--   environments.internal
--   files.dream_id          slice 2's transcript files (§4.5).

CREATE TABLE dreams (
    id                      text PRIMARY KEY,
    -- Reserved multi-tenant scope columns, single-tenant defaults (CLAUDE.md
    -- principle 5), as on every resource table.
    org_id                  text NOT NULL DEFAULT 'default',
    workspace_id            text NOT NULL DEFAULT 'default',
    project_id              text NOT NULL DEFAULT 'default',
    status                  text NOT NULL CHECK (status IN ('pending','running','completed','failed','canceled')),
    -- The pipeline position the tick resumes from: 0 before the start arm,
    -- 1..4 while running, the last stage reached at a terminal state.
    stage                   smallint NOT NULL DEFAULT 0,
    -- Start claims so far; dreamStartAttempts bounds them, and updated_at
    -- within dreamStartLease of a claim is the soft lease (§4.1, §4.2).
    attempts                smallint NOT NULL DEFAULT 0,
    inputs                  jsonb NOT NULL,          -- the request's inputs[], verbatim
    input_memory_store_id   text NOT NULL,           -- denormalized for the tick's checks
    input_session_ids       text[] NOT NULL,
    model                   jsonb NOT NULL,          -- {id, speed?}
    instructions            text,
    output_behavior         jsonb NOT NULL,          -- {type} or {type, memory_store_id}
    -- Set for update_existing; the hold below is keyed on it.
    target_memory_store_id  text,
    outputs                 jsonb NOT NULL DEFAULT '[]'::jsonb,
    session_id              text REFERENCES sessions(id) ON DELETE SET NULL,
    -- "Nothing left to do": the session archived (or never existed) and the
    -- file rows gone. Stamped by the arm that finishes the dream (§4.1); null
    -- while the dream is live or closing. The sweep and the hold index read
    -- it, and neither depends on the nullable session_id above.
    closed_at               timestamptz,
    usage                   jsonb NOT NULL DEFAULT '{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}'::jsonb,
    error                   jsonb,                   -- {type, message} or null
    created_by              text,                    -- audit only, as on every resource
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    ended_at                timestamptz,
    archived_at             timestamptz,
    CONSTRAINT dreams_terminal_ended CHECK ((status IN ('completed','failed','canceled')) = (ended_at IS NOT NULL)),
    CONSTRAINT dreams_error_shape    CHECK (error IS NULL OR status = 'failed'),
    CONSTRAINT dreams_closed_terminal CHECK (closed_at IS NULL OR status IN ('completed','failed','canceled'))
);
CREATE INDEX dreams_created_idx ON dreams (created_at DESC, id DESC);
-- The session-mutation gate's lookup (§4.4): which dream owns a session.
CREATE INDEX dreams_session_idx ON dreams (session_id) WHERE session_id IS NOT NULL;
-- The update_existing hold (§5.3): one live in-place dream per target store —
-- pending, running, or any terminal not yet closed.
CREATE UNIQUE INDEX dreams_target_hold_idx ON dreams (target_memory_store_id)
    WHERE target_memory_store_id IS NOT NULL AND closed_at IS NULL;

ALTER TABLE agents       ADD COLUMN internal boolean NOT NULL DEFAULT false;
ALTER TABLE environments ADD COLUMN internal boolean NOT NULL DEFAULT false;
-- A transcript file's owner (§4.5): never rendered on the wire; null for every
-- uploaded file. Not a foreign key — the closing arm reads it after the dream
-- row is terminal, and a dream is never deleted.
ALTER TABLE files        ADD COLUMN dream_id text;
CREATE INDEX files_dream_idx ON files (dream_id) WHERE dream_id IS NOT NULL;
