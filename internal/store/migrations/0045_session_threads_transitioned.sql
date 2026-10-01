-- A session thread's stats and usage render null until its first status
-- transition (#674), as the reference renders a primary that has never run.
-- Most threads show from their own row that they have moved: a child is born
-- running, a status other than idle is itself a move, and a thread with
-- nonzero usage has run a model request. An idle primary is the one case
-- the row cannot tell (never run, or run and idle again), so this flag records
-- it: events.TransitionThread sets it on the thread's first real status
-- change, and nothing clears it. The renderer reads all four signals
-- (internal/api's threadRow.moved). One flag serves both fields: the spec
-- keeps usage null until the first idle, but every recording shows the two
-- null or present together (docs/DIVERGENCES.md, the session threads entry).
--
-- Every row that exists now is flagged, so nothing that rendered objects
-- before this migration renders null after it: a thread that ran is right to
-- keep them, and a primary that never ran keeps them too, the conservative
-- side the owner chose over guessing from row state (#674). ADD COLUMN with a
-- constant default stores it once in the catalog instead of rewriting every
-- row; SET DEFAULT then gives later inserts false, while existing rows keep
-- the stored true.
--
-- A rolling upgrade can leave one kind of thread unflagged for good: an idle
-- primary that a replica on an earlier build, which writes no flag, both
-- started and stopped after this has run, with no model request settling
-- nonzero usage on it. It renders null until its next transition, which a
-- one-shot session never makes. Every other thread an earlier replica writes
-- or moves is caught by the derived signals.
--
-- The ALTER takes session_threads ACCESS EXCLUSIVE while replicas on the
-- earlier build are serving, and a pending request queues every reader of the
-- table behind it, so it waits at most 2s for the lock, as 0043 does for the
-- same reason; Migrate retries the give-up (55P03). The timeout is reset at
-- the end, so no later migration in the same transaction inherits it.
SET LOCAL lock_timeout = '2s';

ALTER TABLE session_threads ADD COLUMN transitioned boolean NOT NULL DEFAULT true;
ALTER TABLE session_threads ALTER COLUMN transitioned SET DEFAULT false;

SET LOCAL lock_timeout TO DEFAULT;
