-- A files row names the object that holds its bytes (#578). Until now every
-- reader derived the key from the row's own id (blob.FilesKey: files/{id}),
-- which made "one object per row" a law of the schema. A session's file mount
-- now mints a session-scoped copy, as the reference does: a row of its own,
-- with a fresh file_id the session's resources[] echo. The copy aliases the
-- upload's object instead of duplicating its bytes, so two rows can name one
-- object and the key has to be data.
--
-- object_key is that key, and every reader of a file's bytes reads it. Each
-- existing row is backfilled to the key its id derived, which is where its
-- bytes are, so a row written before this keeps working unchanged. A row
-- inserted without one gets the same derivation from the trigger below. That
-- is for a replica still on the previous build, which writes no object_key,
-- during a rolling upgrade. This build always writes the key itself.
--
-- source_file_id is the row a copy was minted from, and NULL for every row
-- that owns its object: an upload, a harvested output, a dream transcript. Two
-- things read it. The outputs harvest replaces only its own snapshot rows, and
-- the grader lists only those. The (scope, filename) uniqueness below is the
-- harvest's per-path key, and a copy is not under it: two resources mounting
-- one upload get two copies with one filename, and a copy may share its name
-- with an output. It is not a foreign key, because a copy outlives its source.
--
-- Rolling upgrade. Only the control plane mints copies, so roll the brain and
-- executor fleets before it. Two previous-build readers mistake a copy for
-- something else: an executor's harvest deletes every session-scoped row,
-- copies included, and a brain grades copies as deliverables. The rest of the
-- previous build degrades without losing anything. It reads a copy's bytes at
-- its own id's key, where no object exists, so that mount is skipped and the
-- run goes on. It cannot delete a shared object, because the trigger on
-- pending_object_deletes drops a key a live row still names, whoever enqueues
-- it. It can leak one: when it deletes the last row naming a shared object, it
-- enqueues that row's own id key instead, and the object is left behind.
--
-- The ALTERs take files ACCESS EXCLUSIVE, held until the migration commits,
-- and the backfill rewrites every row inside that. So, as 0043 and 0045 do,
-- this waits at most 2s for the lock. Migrate retries the give-up (55P03).
SET LOCAL lock_timeout = '2s';

ALTER TABLE files ADD COLUMN object_key text, ADD COLUMN source_file_id text;
UPDATE files SET object_key = 'files/' || id;
ALTER TABLE files ALTER COLUMN object_key SET NOT NULL;

-- A BEFORE trigger runs ahead of the NOT NULL check, so a previous build's
-- INSERT, which names no object_key, passes it carrying the key that build is
-- about to put its bytes at.
CREATE FUNCTION files_object_key_default() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.object_key IS NULL THEN
        NEW.object_key := 'files/' || NEW.id;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER files_object_key_default BEFORE INSERT ON files
    FOR EACH ROW EXECUTE FUNCTION files_object_key_default();

-- The reference count's lookup, below.
CREATE INDEX files_object_key_idx ON files (object_key);

DROP INDEX files_scope_filename_idx;
CREATE UNIQUE INDEX files_scope_filename_idx ON files (scope_id, filename)
    WHERE scope_id IS NOT NULL AND source_file_id IS NULL;

-- Reference counting, at the one door every object delete goes through. Each
-- remover enqueues the key of every row it deletes, in the same transaction
-- (plan 50, #703), and this drops a key some files row still names. So deleting
-- an upload whose copies live leaves their object alone, and deleting the last
-- row that names it enqueues it for the drain. It sits in the database rather
-- than in store.EnqueueObjectDeletes so a previous build's remover is counted
-- too. Otherwise its unconditional enqueue would reach a drain that deletes
-- whatever is queued.
--
-- The lock is for two transactions that each delete one of the last two rows
-- naming a key. Under READ COMMITTED each would see the other's row still
-- there and skip the key, and the object would never be deleted. So a check
-- that finds a row takes a transaction-scoped advisory lock on the key and
-- looks again. The lock is held to commit, so the second checker waits out the
-- first one and then sees its delete. A key no other row names, which is almost
-- every key, takes no lock. Class 578 keeps these locks apart from the
-- single-key advisory locks the migrator and the executor take; the one-key and
-- two-key forms are separate lock spaces. store.EnqueueObjectDeletes sorts its
-- keys so two removers take a shared pair of keys in one order.
--
-- A copy is minted with its source row held FOR SHARE (internal/api's
-- mountFileCopy), so it cannot name a key whose last row is being deleted: the
-- delete waits for the mint to commit, and this check then sees the copy.
CREATE FUNCTION pending_object_deletes_skip_referenced() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM files WHERE object_key = NEW.object_key) THEN
        PERFORM pg_advisory_xact_lock(578, hashtext(NEW.object_key));
        IF EXISTS (SELECT 1 FROM files WHERE object_key = NEW.object_key) THEN
            RETURN NULL;
        END IF;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER pending_object_deletes_skip_referenced BEFORE INSERT ON pending_object_deletes
    FOR EACH ROW EXECUTE FUNCTION pending_object_deletes_skip_referenced();

SET LOCAL lock_timeout TO DEFAULT;
