-- Finish the wind-downs the pre-#25 state machine could never finish.
--
-- Until #25 a graceful stop moved a queued, starting or active item to
-- 'stopping', and nothing ever completed 'stopping' -> 'stopped': the worker read
-- the control plane's own stop as a lost lease and deliberately stopped nothing,
-- and Poll excludes 'stopping' from reclaim. Every such row is still sitting
-- non-terminal with a null stopped_at and an unreleased lease, and the new code
-- cannot rescue all of them by itself — a never-polled queued item carries no
-- lease at all, so Poll's lapsed-lease finalizer can never match it.
--
-- Finalize the lot. Every 'stopping' row predating this migration was written by
-- that state machine, and no worker running against it will ever finish one, so
-- there is nothing here still to wait for. An upgraded worker genuinely still
-- winding one down loses nothing: it finds its own force-stop already done, which
-- is the 409 it ignores.
--
-- No LOCK TABLE, unlike 0013: this builds no constraint, so a not-yet-upgraded
-- replica writing a fresh 'stopping' row during a rolling upgrade cannot fail the
-- migration — it is caught instead by Poll's finalizer, whose null-lease arm
-- exists for exactly that window. Locking the queue for the length of the repair
-- would cost more than it buys. Under the new state machine 'stopping' is entered
-- only from 'active' and is always finished (by its worker, or by Poll once the
-- lease lapses), so this repair is one-time and needs no companion constraint.
UPDATE work_items
   SET state            = 'stopped',
       stopped_at       = COALESCE(stopped_at, now()),
       lease_expires_at = NULL,
       updated_at       = now()
 WHERE state = 'stopping';
