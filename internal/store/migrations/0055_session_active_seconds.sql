-- The time a session spent running, for usage.active_seconds and the
-- session.usage event (docs/plan/68_session-usage-event.md decisions 4 and 5).
-- active_seconds sums the session's closed running intervals; running_since is
-- the open one's start, null while the session is not running. From here on
-- events.Log.AppendInTx keeps both, from the session-level status events it
-- appends: running opens an interval, and idle, rescheduled or terminated
-- closes it. The session's status is the fold over its threads, so its own
-- events already count concurrent threads once.
--
-- The backfill replays the rule over every session's stored status events, so
-- a session's history keeps its time and a session running now keeps its open
-- interval. One difference: here a running inside an open interval keeps it
-- open, because the stored log holds every close and the session ran
-- throughout. The live rule restarts there instead (internal/events/usage.go),
-- because a close it did not see was written by a replica on the earlier
-- build.
--
-- The scan of events runs first, into a temporary table, before anything
-- locks sessions: reading events blocks no append. The ALTER then takes an
-- ACCESS EXCLUSIVE lock on sessions, held until the migration commits, so
-- every read and write of a session waits while each session row is updated —
-- one pass over sessions, not over events.
--
-- A rolling upgrade can miscount a session that changes status while replicas
-- on the earlier build are serving, or while the scan runs, since those status
-- events move neither these columns nor the scan's result: an interval they
-- open is lost, and one they close stays open until this build's next status
-- event — a running restarts it, dropping that turn's time; an idle closes it,
-- counting the gap between.
CREATE TEMPORARY TABLE session_clock ON COMMIT DROP AS
WITH status AS (
    SELECT session_id, seq, processed_at,
           type = 'session.status_running' AS running,
           LAG(type = 'session.status_running', 1, false)
               OVER (PARTITION BY session_id ORDER BY seq) AS was_running
      FROM events
     WHERE type IN ('session.status_running', 'session.status_idle',
                    'session.status_rescheduled', 'session.status_terminated')
       AND processed_at IS NOT NULL
), edge AS (
    -- Where the session starts or stops running; a running inside an open
    -- interval is not an edge.
    SELECT session_id, seq, processed_at, running
      FROM status
     WHERE running <> was_running
), span AS (
    SELECT session_id, running, processed_at AS opened,
           LEAD(processed_at) OVER (PARTITION BY session_id ORDER BY seq) AS closed
      FROM edge
)
SELECT session_id,
       COALESCE(SUM(GREATEST(EXTRACT(EPOCH FROM closed - opened), 0))
           FILTER (WHERE running AND closed IS NOT NULL), 0)::double precision AS active,
       MAX(opened) FILTER (WHERE running AND closed IS NULL) AS since
  FROM span
 GROUP BY session_id;

ALTER TABLE sessions
    ADD COLUMN active_seconds double precision NOT NULL DEFAULT 0,
    ADD COLUMN running_since timestamptz;

UPDATE sessions
   SET active_seconds = clock.active, running_since = clock.since
  FROM session_clock clock
 WHERE sessions.id = clock.session_id;
