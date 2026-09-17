-- A primary thread row never carries archived_at (#720). #713 stopped the
-- session archive stamping the session's archived_at onto its primary, and
-- 0040 cleared the stamps already there; this is the guard 0040 could not add,
-- because beside #713 it would have failed every session archive a replica
-- still running a build from before #713 performed, for the length of the
-- rollout.
--
-- No release wrote the stamp: v0.3.0 predates session threads (#436, 0025),
-- and v0.4.0 carries #713. Only a deployment tracking main at a commit between
-- #436 and #713 runs replicas that do. Rolled straight to a build carrying
-- this, each session archive they perform fails on the CHECK — the dream
-- runner's closing arm included — until the last of them is replaced, and this
-- migration can fail too, as below. Rolling out a build with #713 and without
-- this first avoids the failed archives and the first of those failures.
--
-- The clear runs again, with 0040's WHERE, because such a replica could stamp
-- a primary after 0040 ran and ADD CONSTRAINT validates every existing row: one
-- such stamp would fail this migration, and Migrate applies every pending
-- migration in one transaction when the controlplane, brain or executor
-- starts. updated_at is left alone, for 0040's reason.
--
-- The migration can still fail while it runs. A replica from before #713 can
-- stamp a primary between the clear and the ALTER's lock, which validation
-- then meets. And any replica deleting a session whose stamped primary the
-- clear has just updated can deadlock with the ALTER's lock, which needs only a
-- stamp such a replica left behind. The transaction rolls back, and a later
-- start applies the migration once no such write lands in that window.
-- Taking ACCESS EXCLUSIVE before the clear, with Migrate pinned to READ
-- COMMITTED, would close both, at the cost of holding every reader of
-- session_threads through the clear's scan as well as the validation's on every
-- deployment, to spare restarts only on deployments no release produces.
--
-- Validated rather than NOT VALID, for 0036's reason: this migrator cannot run
-- the follow-up VALIDATE CONSTRAINT in a transaction of its own, so NOT VALID
-- would abandon validation, not defer it. Both terms are IS tests, so the
-- expression is never NULL and no row passes it by default.
--
-- DROP ... IF EXISTS so a re-run of the file rebuilds the same constraint
-- instead of failing with duplicate_object and taking every other pending
-- migration down with it.
UPDATE session_threads
   SET archived_at = NULL
 WHERE parent_thread_id IS NULL AND archived_at IS NOT NULL;

ALTER TABLE session_threads DROP CONSTRAINT IF EXISTS session_threads_primary_unarchived;
ALTER TABLE session_threads ADD CONSTRAINT session_threads_primary_unarchived
    CHECK (parent_thread_id IS NOT NULL OR archived_at IS NULL);
