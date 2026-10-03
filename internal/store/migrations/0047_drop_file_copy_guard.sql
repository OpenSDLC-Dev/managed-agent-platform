-- 0046's copy-delete guard on files and its tombstone trigger on
-- deleted_sessions go (#856), and with them the map.copy_delete setting,
-- which nothing reads any more. Both served binaries built before 0046 alone:
-- the guard kept their DELETEs off a session's file copies, and the trigger
-- took a session's files for their session delete, whose own DELETE the guard
-- kept off the copies. Each cost every later build: a path that deletes
-- copies had to set the flag or its DELETE silently skipped them, and every
-- session delete paid a trigger call. The owner's decision on #856 is that a
-- rollback below 0046 is not supported, so they go in the release that first
-- ships 0046 (v0.4.0 is the last without it).
--
-- This build deletes a copy with a plain DELETE: DELETE /v1/files/{id}, the
-- session delete, which takes the session's copies with its outputs in one
-- id-ordered statement, and the expiry sweep. The outputs harvest still
-- leaves copies by its own predicate (source_file_id IS NULL), and a dream's
-- close deletes transcripts, which are never copies. A binary built between
-- 0046 and this one loses nothing here either: its set_config reaches nothing
-- that reads it, and its session delete takes its session's files itself.
--
-- What stays is the reference count on pending_object_deletes, with its
-- advisory lock and its refusal of a files/ key outside READ COMMITTED. Those
-- keep this build's own removers from each other: two transactions deleting
-- the last two rows that name one object still owe it once between them.
--
-- Rolling upgrade from a release before 0046: the first binary of this
-- release to start applies 0046 and this together, and the older replicas
-- then run on this schema until they are replaced. Only this release's
-- control plane mints copies, and nothing keeps an older replica's deletes off
-- them any more:
--   * an older executor's harvest replaces a session's snapshot by deleting
--     every session-scoped row, the copies with the outputs. The session's
--     resources[] then name a file that is gone: every later provision skips
--     that mount, as it skips any deleted file's, and nothing brings it back.
--   * an older control plane's session delete takes the session's copies
--     with its outputs, its DELETE matching every session-scoped row. It
--     leaves no row behind, so there is nothing for a sweep to find. Its
--     DELETE /v1/files/{copy} deletes the copy, and its expiry sweep takes
--     expired copies.
--   * every older remover that deletes a copy owes the key its id derives,
--     files/{copy id}, where nothing is stored, and never the object the copy
--     names; the drain deletes the missing key as a no-op. So when that copy
--     was the last row naming its object (the upload was deleted or swept
--     first), the object stays in storage, owed by nothing. No sweep finds
--     it: the queue is the only record of an owed object.
--   * an older session delete locks the session's rows in scan order, which
--     the tombstone trigger had kept it from doing. A create holding two of
--     those rows FOR SHARE in id order (internal/api's lockFileRows) can meet
--     it in a deadlock (40P01), and either side can fail: this build's POST
--     /v1/sessions answers 500, or the older delete does.
--   * 0046's other notes on an older replica still hold: its executor skips a
--     copy's mount, its grader lists copies, its control plane mounts the
--     upload itself, answers 500 to a rubric naming a copy and to a worker
--     reading one's content, and lists copies unfiltered, its other removers
--     can deadlock against this build's, and on a database whose
--     default_transaction_isolation is stricter than READ COMMITTED each of
--     its transactions that enqueues a files/ key fails.
-- So finish the rollout before creating sessions that mount files, rolling
-- the brain and the executor before the control plane: an older executor is
-- the one replica that takes a live session's copies.
--
-- Rolling back below 0046 is unsupported, and so is anything that runs a
-- release before 0046 on this schema once it has migrated, a helm upgrade
-- --atomic that rolls the release back after the migration committed among
-- them: the migration stays, and an older binary does everything above for
-- as long as it runs. Rolling forward again does not restore what it deleted.
--
-- Dropping a trigger takes its table ACCESS EXCLUSIVE, so both tables are
-- locked first, before any work, in the order a session delete takes them:
-- the tombstone, then the files. A session delete holding deleted_sessions
-- and asking for files therefore never waits for this while this waits for
-- it. Neither drop rewrites or reads a row, so the locks are held for
-- moments, but a waiting ACCESS EXCLUSIVE request queues every reader of its
-- table behind it. As 0043, 0045 and 0046 do, this waits at most 2s for each
-- lock, and migrate.go retries the give-up (55P03) and a deadlock (40P01).
-- From a release before 0046 this runs in 0046's transaction, which already
-- holds files ACCESS EXCLUSIVE and deleted_sessions SHARE ROW EXCLUSIVE, so
-- only the stronger lock on deleted_sessions is still to take.
SET LOCAL lock_timeout = '2s';

LOCK TABLE deleted_sessions IN ACCESS EXCLUSIVE MODE;
LOCK TABLE files IN ACCESS EXCLUSIVE MODE;

DROP TRIGGER files_follow_session ON deleted_sessions;
DROP FUNCTION files_follow_session();

DROP TRIGGER files_copy_delete_guard ON files;
DROP FUNCTION files_copy_delete_guard();

SET LOCAL lock_timeout TO DEFAULT;
