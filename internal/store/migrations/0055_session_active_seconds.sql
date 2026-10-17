-- The time a session spent running, for usage.active_seconds and the
-- session.usage event (docs/plan/68_session-usage-event.md decisions 4 and 5).
-- active_seconds sums the session's closed running intervals; running_since is
-- the open one's start, null while the session is not running. From here on
-- events.Log.AppendInTx keeps both, from the session-level status events it
-- appends: running opens an interval, restarting one already open, and idle,
-- rescheduled or terminated closes it. The session's status is the fold over
-- its threads, so its own events already count concurrent threads once.
--
-- The backfill replays the same rule over every session's stored status
-- events, so a session's history keeps its time and a session running now
-- keeps its open interval. A status event this scan does not see — one a
-- binary from before this migration writes, or one committed while the scan
-- runs — is reconciled by the session's next status event AppendInTx writes,
-- which reads the log's last status event first (internal/events/usage.go).
--
-- The scan of events runs first, into a scratch table dropped at the end,
-- before this migration locks sessions: reading events blocks no append. An
-- ordinary unlogged table, not a temporary one, so the migration needs no
-- privilege beyond the CREATE every migration here needs. The ALTER then
-- takes an ACCESS EXCLUSIVE lock on sessions, held until the migration
-- commits, so every read and write of a session waits while the sessions with
-- running time are updated — one pass over sessions, not over events. That
-- holds when 0055 is the only migration pending. The migrator runs every
-- pending migration in one transaction, so on a database further behind, a
-- lock an earlier one took is held through the scan too: 0053's ALTER locks
-- sessions, and 0054's foreign key to it blocks appends.
CREATE UNLOGGED TABLE session_clock_0055 AS
WITH status AS (
    SELECT session_id, type, processed_at,
           LEAD(type) OVER w AS next_type,
           LEAD(processed_at) OVER w AS next_at
      FROM events
     WHERE type IN ('session.status_running', 'session.status_idle',
                    'session.status_rescheduled', 'session.status_terminated')
       AND processed_at IS NOT NULL
    WINDOW w AS (PARTITION BY session_id ORDER BY seq)
)
-- Each running opens an interval the next status event ends: a close counts
-- it, a running restarts it, and none leaves it open.
SELECT session_id,
       COALESCE(SUM(GREATEST(EXTRACT(EPOCH FROM next_at - processed_at), 0))
           FILTER (WHERE type = 'session.status_running'
                     AND next_type <> 'session.status_running'), 0)::double precision AS active,
       MAX(processed_at) FILTER (WHERE type = 'session.status_running' AND next_type IS NULL) AS since
  FROM status
 GROUP BY session_id;

ALTER TABLE sessions
    ADD COLUMN active_seconds double precision NOT NULL DEFAULT 0,
    ADD COLUMN running_since timestamptz;

UPDATE sessions
   SET active_seconds = clock.active, running_since = clock.since
  FROM session_clock_0055 clock
 WHERE sessions.id = clock.session_id
   AND (clock.active <> 0 OR clock.since IS NOT NULL);

DROP TABLE session_clock_0055;
