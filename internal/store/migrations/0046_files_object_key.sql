-- A files row can name another row's object (#578). Until now every reader
-- derived a file's key from its own id (blob.FilesKey: files/{id}), which made
-- "one object per row" a law of the schema. A session's file mount now mints a
-- session-scoped copy, as the reference does: a row of its own, with a fresh
-- file_id the session's resources[] echo, aliasing the upload's object instead
-- of duplicating its bytes.
--
-- object_key is the key a copy's bytes are at, and NULL on every row that owns
-- its object: an upload, a harvested output, a dream transcript, whose key is
-- still its own id's. Every reader of a file's bytes reads
-- coalesce(object_key, 'files/' || id) (store.FileObjectKeySQL). NULL is the
-- owner's value so that this migration rewrites no row and a previous build's
-- INSERT, which names neither column, writes an owner correctly.
-- source_file_id is the row a copy was minted from. The two are set together,
-- on a copy, or not at all, and a copy's key is under files/, being its
-- source's key, which is 'files/' || an id at the end of every chain
-- (internal/api's mountFileCopy): the reference count below counts files/ keys
-- alone. The CHECKs are NOT VALID because both columns are new and NULL on
-- every existing row, which they already accept, so validating would scan the
-- table to learn nothing. Neither is a foreign key: a copy outlives its source.
--
-- source_file_id is what tells a copy from the rows its session produced. The
-- outputs harvest replaces only its own snapshot rows, and the grader lists
-- only those. The (scope, filename) uniqueness is the harvest's per-path key
-- and leaves copies out: two resources mounting one upload get two copies with
-- one filename, and a copy may share its name with an output. And the guard
-- below acts on copies alone.
--
-- A copy is deleted only by a transaction that has said it means to, with
-- set_config('map.copy_delete', 'on', true) (store.AllowFileCopyDeletes). Any
-- other DELETE skips the row, as if its WHERE had not matched. That is for the
-- previous build, which knows nothing of copies and deletes every
-- session-scoped row in two places: its executor's harvest, which would take a
-- live session's copies and leave resources[] naming files that are gone, and
-- its session delete. This build sets the flag in the three transactions that
-- remove copies: DELETE /v1/files/{id}, the session delete and the expiry
-- sweep. store's TestEveryFilesDeleteDecidesAboutCopies fails on a DELETE FROM
-- files it has not been told about, so a new remover is a decision, not a
-- silent skip.
--
-- A session's files follow its tombstone. A previous build's session delete
-- writes the deleted_sessions row too, in the same transaction, under the
-- session's row lock and before it touches files, so the trigger on it
-- deletes every session-scoped row: the copies the guard would keep from that
-- build's DELETE and the outputs it would take. One statement locks them in id
-- order, the order a create holds the rows it mounts FOR SHARE in
-- (internal/api's lockFileRows), and their keys are enqueued once each, in
-- the count's lock order below. That build's own DELETE then finds nothing
-- left to lock. Had the outputs been left to it, they would go in scan order
-- after the copies the trigger took, and a create holding one of each would
-- close a cycle with it (internal/api's
-- TestAPreviousBuildSessionDeleteLocksInTheCreatesOrder). This build's delete
-- has set the flag by then and takes the same rows the same way itself, so the
-- trigger leaves them to it: the trigger serves the previous build alone, and
-- dropping it with the guard (#856) changes nothing this build does. It rides
-- the tombstone rather than the sessions row so this migration need not lock
-- the busiest table.
--
-- Reference counting, at the one door every object delete goes through. Each
-- remover enqueues the key of every row it deletes, in the same transaction
-- (plan 50, #703), and the trigger on pending_object_deletes drops a key some
-- files row still names. So deleting an upload whose copies live leaves their
-- object alone, and deleting the last row that names it enqueues it for the
-- drain. It sits in the database so a previous build's remover is counted too:
-- its unconditional enqueue would otherwise reach a drain that deletes
-- whatever is queued.
--
-- The lock is for two transactions that each delete one of the last two rows
-- naming a key. Each would see the other's row still there and skip the key,
-- and the object would never be deleted. So a check that finds a row takes a
-- transaction-scoped advisory lock on the key and looks again. The lock is
-- held to commit, so the second checker waits out the first one and then sees
-- its delete. Looking again sees that commit only under READ COMMITTED, where
-- each statement takes a new snapshot, so the trigger refuses a files/ key
-- under any other isolation level outright rather than skipping a key it has
-- stopped being able to count. Every remover in this build names READ
-- COMMITTED when it begins (store.BeginObjectDelete) rather than inheriting a
-- database default that may be stricter; the refusal is for whatever does
-- not. Only a files/ key can be named by a files row, an owner by its id and a
-- copy by its object_key (files_copy_names_a_files_key), so any other key, a
-- skill archive's or a session checkpoint's, is not counted and passes under
-- every level, as before 0046. A files/ key no other row names, which is
-- almost every one, takes no lock. Class 578 keeps these locks apart from the
-- single-key advisory locks the migrator and the executor take; the one-key
-- and two-key forms are separate lock spaces.
-- store.PendingObjectDeleteInsertSQL inserts each key once, in hashtext order,
-- the lock's own id, so two removers take a shared pair in one order; two keys
-- that collide are one lock.
--
-- A copy is minted with its source row held FOR SHARE (internal/api's
-- mountFileCopy), so it cannot name a key whose last row is being deleted: the
-- delete waits for the mint to commit, and this check then sees the copy.
--
-- Rolling upgrade: no order is needed between the binaries, so one helm
-- upgrade or compose up rolls them together. No previous-build statement
-- deletes a copy, or an object some row still names. A previous-build replica
-- degrades while it overlaps this build, without losing anything:
--   * executor: a copy has no object at its own id's key, where that build
--     reads, so the mount is skipped and the turn runs without it.
--   * brain: a grading cycle lists a session's copies among its deliverables,
--     by name, MIME type and size; their contents are not inlined, because the
--     read at the copy's own key finds nothing and that build lists only.
--   * control plane: a create, a resources add or a deployment fire mounts
--     the upload itself, as before #578. DELETE /v1/files/{copy} answers 404,
--     the guard leaving that build's DELETE nothing to report. A rubric naming
--     a copy answers 500, and so does a worker's GET /v1/files/{copy}/content,
--     with a "file missing from object storage" ERROR log: both read the
--     copy's own key. The management route's answer is this build's, the 400
--     file_not_downloadable every upload gets. Its unfiltered GET /v1/files
--     lists session-scoped rows, copies included.
--   * its expiry sweep skips expired copies until this build's sweeps them,
--     and ends a pass at its first short batch (n < filePurgeBatch): every
--     copy among the oldest expired rows shortens each batch, so its purge of
--     later uploads slows, and stops once a batch's worth of them is oldest.
--   * a transaction on either side can meet a deadlock (40P01) and fail: that
--     build's harvest and dream close lock the rows they delete in scan order
--     rather than by id, and every remover of that build enqueues its keys
--     unsorted, so the count's advisory locks come in no fixed order. The
--     victim can be this build's request, POST /v1/sessions answering 500, as
--     readily as that build's, its session delete included: the trigger above
--     takes the session's rows in id order but locks their keys in hashtext
--     order, an order that build's other removers do not keep. A session
--     whose copies name two expired uploads, deleted while that build's expiry
--     sweep takes the uploads and enqueues their keys the other way round, is
--     such a pair, and Postgres decides which of the two fails.
--   * on a database whose default_transaction_isolation is stricter than READ
--     COMMITTED, that build begins every transaction at the default, so each
--     of its transactions that enqueues a files/ key fails at the count's
--     refusal (25000): DELETE /v1/files/{id} of a file it finds, the delete of
--     a session holding any file (the trigger enqueues their keys), an expiry
--     sweep that takes a row, a harvest replacing earlier outputs, and a dream
--     close that deletes transcripts. Its skill and skill-version deletes, and
--     the delete of a session holding no file, enqueue no files/ key and
--     succeed.
--
-- Rolling back after 0046 leaves the schema and its three triggers in place,
-- so nothing is lost. Every degradation above but the last comes of copies
-- and lasts until each session holding copies is deleted, the tombstone
-- trigger then taking its copies and owing their objects: the previous build
-- never mounts a copy, answers 404 to DELETE /v1/files/{copy}, and never
-- sweeps an expired copy, so an upload a copy shares stays stored and its
-- sweep stalls behind such copies. The last has nothing to do with copies:
-- the deletes it names fail for as long as the previous build runs on this
-- schema, on a database defaulting stricter than READ COMMITTED. Rolling
-- forward resumes every path. #856 tracks dropping the guard and the
-- tombstone trigger once no pre-0046 binary can run.
--
-- Every table lock this needs is taken first, before any work, so a give-up
-- wastes no work and nothing waits for a second lock while holding the first
-- for long. The order is the one a session delete takes the tables in, the
-- tombstone, the files and then the queue, and until the trigger this adds no
-- transaction reads files after writing the queue, so no live transaction
-- holds one of these while waiting for one taken before it. files is held ACCESS EXCLUSIVE from there to the
-- commit, which covers three index builds; on a million rows that measured
-- under 0.4s, and every reader and writer of files queues behind it.
-- pending_object_deletes and deleted_sessions are held SHARE ROW EXCLUSIVE,
-- for their triggers, which for as long stops every enqueue, the drain and
-- session deletes, but not the reaper's reads. As 0043 and 0045 do, this waits
-- at most 2s for each lock, and migrate.go retries the give-up (55P03) and a
-- deadlock (40P01).
SET LOCAL lock_timeout = '2s';

LOCK TABLE deleted_sessions IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE files IN ACCESS EXCLUSIVE MODE;
LOCK TABLE pending_object_deletes IN SHARE ROW EXCLUSIVE MODE;

ALTER TABLE files
    ADD COLUMN object_key text,
    ADD COLUMN source_file_id text,
    ADD CONSTRAINT files_copy_pair_agrees
        CHECK ((object_key IS NULL) = (source_file_id IS NULL)) NOT VALID,
    ADD CONSTRAINT files_copy_names_a_files_key
        CHECK (starts_with(object_key, 'files/')) NOT VALID;

-- The reference count's lookup of copies; owners are found by id.
CREATE INDEX files_object_key_idx ON files (object_key) WHERE object_key IS NOT NULL;

DROP INDEX files_scope_filename_idx;
CREATE UNIQUE INDEX files_scope_filename_idx ON files (scope_id, filename)
    WHERE scope_id IS NOT NULL AND source_file_id IS NULL;

-- GET /v1/files without scope_id lists unscoped rows only, newest first. Every
-- session-scoped row (each mount's copy, each harvested output) would
-- otherwise be walked past in files_created_at_id_idx to find them, and copies
-- grow with every session that mounts a file. files_created_at_id_idx stays:
-- the previous build's unfiltered list has no other ordered path, and a
-- ?scope_id= list of a session holding much of the table plans through it.
CREATE INDEX files_unscoped_created_at_id_idx ON files (created_at DESC, id DESC)
    WHERE scope_id IS NULL;

CREATE FUNCTION files_copy_delete_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.source_file_id IS NOT NULL
       AND current_setting('map.copy_delete', true) IS DISTINCT FROM 'on' THEN
        RETURN NULL;
    END IF;
    RETURN OLD;
END $$;

CREATE TRIGGER files_copy_delete_guard BEFORE DELETE ON files
    FOR EACH ROW EXECUTE FUNCTION files_copy_delete_guard();

CREATE FUNCTION files_follow_session() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    keys text[];
BEGIN
    IF current_setting('map.copy_delete', true) = 'on' THEN
        RETURN NULL;
    END IF;
    PERFORM set_config('map.copy_delete', 'on', true);
    WITH gone AS (
        DELETE FROM files
         WHERE id IN (SELECT id FROM files
                       WHERE scope_type = 'session' AND scope_id = NEW.id
                       ORDER BY id
                       FOR UPDATE)
        RETURNING coalesce(object_key, 'files/' || id) AS k)
    SELECT array_agg(DISTINCT k) INTO keys FROM gone;
    PERFORM set_config('map.copy_delete', '', true);
    IF keys IS NOT NULL THEN
        INSERT INTO pending_object_deletes (object_key)
        SELECT k FROM unnest(keys) AS k ORDER BY hashtext(k)
        ON CONFLICT (object_key) DO NOTHING;
    END IF;
    RETURN NULL;
END $$;

CREATE TRIGGER files_follow_session AFTER INSERT ON deleted_sessions
    FOR EACH ROW EXECUTE FUNCTION files_follow_session();

-- Whether some files row names the object at k, a files/ key: a copy by its
-- object_key, an owner by its id.
CREATE FUNCTION files_name_object(k text) RETURNS boolean
LANGUAGE sql AS $$
    SELECT EXISTS (SELECT 1 FROM files WHERE object_key = k)
        OR EXISTS (SELECT 1 FROM files WHERE id = substr(k, 7) AND object_key IS NULL)
$$;

CREATE FUNCTION pending_object_deletes_skip_referenced() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT starts_with(NEW.object_key, 'files/') THEN
        RETURN NEW;
    END IF;
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'pending_object_deletes: a files/ key is enqueued under READ COMMITTED only, not %',
            current_setting('transaction_isolation')
            USING ERRCODE = 'invalid_transaction_state';
    END IF;
    IF files_name_object(NEW.object_key) THEN
        PERFORM pg_advisory_xact_lock(578, hashtext(NEW.object_key));
        IF files_name_object(NEW.object_key) THEN
            RETURN NULL;
        END IF;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER pending_object_deletes_skip_referenced BEFORE INSERT ON pending_object_deletes
    FOR EACH ROW EXECUTE FUNCTION pending_object_deletes_skip_referenced();

SET LOCAL lock_timeout TO DEFAULT;
