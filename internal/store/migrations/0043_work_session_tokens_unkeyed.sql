-- A sessions token no longer carries a foreign key to its session (#643): the
-- key's check locked the session row inside the poll's claim, after the work
-- item, and deadlocked against a session delete or interrupt (internal/api's
-- claimWork has the lock rule). A token whose session is gone cannot
-- authenticate anyway: internal/worktoken's Authenticate joins the session.
--
-- The cascade's job passes to the trigger below: deleting a session row
-- deletes its tokens on every path that deletes one — the API's delete, a
-- replica still on the previous build, a hand-written DELETE. The index on
-- session_id stays; the trigger's delete reads by it. The trigger has to run
-- after the cascade into work_items, because that cascade is what waits out a
-- claim holding one of the session's items; only then has the claim's token
-- committed, and the token delete, a statement with a snapshot of its own,
-- sees it. Postgres fires a row's AFTER triggers in name order, byte-wise,
-- and the cascades are internal triggers named RI_ConstraintTrigger_a_<oid>;
-- a lower-case name sorts after every one of them. A name sorting first
-- leaves the claim's token behind, which the item_first delete cases of
-- TestPollDoesNotDeadlockAgainstTheSessionLock catch.
--
-- Dropping the key takes work_session_tokens and then sessions ACCESS
-- EXCLUSIVE, measured in that order. During a rolling upgrade a replica on
-- the previous build takes the two tables in either order (its claim and its
-- delete take sessions first; a token check takes work_session_tokens first),
-- so waiting here can close a cycle, and no LOCK TABLE order avoids every
-- one; the ALTER's own order at least holds only the token table while it
-- waits for the busy one. A pending lock on sessions also queues every
-- session read behind it, so this migration waits at most 2s for each lock.
-- The bound sits above deadlock_timeout (a second by default) on purpose:
-- only once a waiter has waited that long does Postgres run its deadlock
-- check, and that check is what cancels an autovacuum holding the table. A
-- bound below it waits the autovacuum out instead, through every retry, and
-- so does a deadlock_timeout raised to 2s or more, or an autovacuum preventing
-- wraparound, which no waiter cancels. Holding neither table when it starts,
-- the migration asks for sessions the instant it has the token table, so in
-- a cycle with an old replica it is the first to wait: its check runs first
-- and it is the side that gives up (40P01), not live traffic. A holder
-- outside any cycle makes it give up at the bound (55P03). Migrate retries
-- both (migrate.go). The timeout is reset at the end, so no later migration
-- in the same transaction inherits it.
SET LOCAL lock_timeout = '2s';

ALTER TABLE work_session_tokens DROP CONSTRAINT work_session_tokens_session_id_fkey;

CREATE FUNCTION work_session_tokens_follow_session() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM work_session_tokens WHERE session_id = OLD.id;
    RETURN NULL;
END $$;

CREATE TRIGGER work_session_tokens_follow_session AFTER DELETE ON sessions
    FOR EACH ROW EXECUTE FUNCTION work_session_tokens_follow_session();

SET LOCAL lock_timeout TO DEFAULT;
