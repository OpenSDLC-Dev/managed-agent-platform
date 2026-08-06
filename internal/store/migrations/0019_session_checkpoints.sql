-- The restore-consumption marker (plan 24 D6). Restore fires on this marker,
-- never on "the container is fresh": a fresh container also happens on the
-- routine cattle path of a container dying mid-turn, and rewinding that to
-- reap-time state would contradict the committed event log. The TTL reap
-- writes `ready` after the checkpoint upload, inside its lock hold, before
-- the destroy; provision restores when the marker is `ready` and flips it
-- `consumed` only after the extraction completes — a crash mid-restore
-- leaves `ready` standing, detectably replaceable rather than silently
-- adoptable. A consumed marker is kept, not deleted, so the blob's
-- provenance stays queryable; the next checkpoint overwrites both.
CREATE TABLE session_checkpoints (
    session_id text        PRIMARY KEY,
    blob_key   text        NOT NULL,
    state      text        NOT NULL CHECK (state IN ('ready', 'consumed')),
    taken_at   timestamptz NOT NULL DEFAULT now()
);
