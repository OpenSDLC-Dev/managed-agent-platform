-- 0046's copy-delete guard on files and its tombstone trigger on
-- deleted_sessions go (#856), and with them the map.copy_delete setting,
-- which nothing reads any more. Both served binaries built before 0046 alone,
-- and each cost every later build: a path that deletes copies had to set the
-- flag or its DELETE silently skipped them, and every session delete paid a
-- trigger call. Binaries built before 0046 are not supported against this
-- schema, so rolling back below 0046 is unsupported (the owner's decision on
-- #856: v0.4.0, the last release without it, has no deployment to keep).
--
-- This build deletes a copy with a plain DELETE: DELETE /v1/files/{id}, the
-- session delete, which takes the session's copies with its outputs in one
-- id-ordered statement, and the expiry sweep. The outputs harvest leaves
-- copies by its own predicate (source_file_id IS NULL), and a dream's close
-- deletes by dream_id, which no copy carries. store's
-- TestEveryFilesDeleteOwesItsKeyAndDecidesAboutCopies holds each DELETE FROM
-- files to owing the key its rows name and to either excluding copies or
-- saying why it may meet one. A binary built between 0046 and this one loses
-- nothing here: its set_config reaches nothing that reads it, and its session
-- delete takes its session's files itself.
--
-- What stays is the reference count on pending_object_deletes, with its
-- advisory lock and its refusal of a files/ key outside READ COMMITTED. Those
-- keep this build's own removers from each other: two transactions deleting
-- the last two rows that name one object still owe it once between them.
--
-- Dropping a trigger takes its table ACCESS EXCLUSIVE. A waiting request for
-- that queues every reader of the table behind it, so, as 0043, 0045 and 0046
-- do, this waits at most 2s for each lock, and migrate.go retries the give-up
-- (55P03) and a deadlock (40P01). Neither drop rewrites or reads a row.
--
-- Run alone, on a database at 0046, this takes both locks first, before any
-- work, in the order a session delete takes the tables: the tombstone, then
-- the files. A session delete holding deleted_sessions and asking for files
-- therefore never waits for this while this waits for it.
--
-- Upgrading from a release before 0046, every pending migration (0041 to 0047
-- from v0.4.0) runs in one transaction, and this comes after 0046's work, its
-- index builds included. By then files is held ACCESS EXCLUSIVE already, so
-- its LOCK here takes nothing new, and deleted_sessions SHARE ROW EXCLUSIVE,
-- which keeps out every writer of it; the ACCESS EXCLUSIVE lock this asks for
-- on deleted_sessions waits for its readers alone, but it is asked for last,
-- with every lock the earlier migrations took still held. A give-up or a
-- deadlock there rolls the whole run back, and migrate.go retries it from the
-- start.
SET LOCAL lock_timeout = '2s';

LOCK TABLE deleted_sessions IN ACCESS EXCLUSIVE MODE;
LOCK TABLE files IN ACCESS EXCLUSIVE MODE;

DROP TRIGGER files_follow_session ON deleted_sessions;
DROP FUNCTION files_follow_session();

DROP TRIGGER files_copy_delete_guard ON files;
DROP FUNCTION files_copy_delete_guard();

SET LOCAL lock_timeout TO DEFAULT;
