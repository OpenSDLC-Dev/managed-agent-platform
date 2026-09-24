-- A sessions token no longer carries a foreign key to its session (#643). The
-- key's check took the session row inside the poll's claim, after the claim
-- had taken the work item — the reverse of a session delete or interrupt — and
-- the two could deadlock; internal/api's claimWork says how. What the key did
-- is done without it: a token authenticates only through a join to its live
-- item and its unarchived session (internal/worktoken's Authenticate), so a
-- row whose session is gone is inert, and the session delete removes the
-- session's tokens itself (internal/api's deleteSession). The index on
-- session_id stays; that delete reads by it.
--
-- Dropping the key takes both tables ACCESS EXCLUSIVE. sessions is taken
-- first, the order a delete and a claim take them, so that during a rolling
-- upgrade a replica still on the previous build waits for this rather than
-- deadlocking with it — all but a token check, which takes the two in the
-- other order within one statement. If one does meet it, the migration rolls
-- back and a later start applies it.
LOCK TABLE sessions, work_session_tokens IN ACCESS EXCLUSIVE MODE;
ALTER TABLE work_session_tokens DROP CONSTRAINT work_session_tokens_session_id_fkey;
