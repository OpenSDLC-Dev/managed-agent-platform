-- The time a session spent running, for usage.active_seconds and the
-- session.usage event (docs/plan/68_session-usage-event.md decisions 4 and 5).
-- active_seconds sums the session's closed running intervals; running_since is
-- the open one's start, null while the session is not running. From here on
-- events.Log.AppendInTx keeps both, from the session-level status events it
-- appends: running opens an interval when none is open, and idle, rescheduled
-- or terminated closes it. The session's status is the fold over its threads,
-- so its own events already count concurrent threads once.
--
-- ADD COLUMN with a constant default changes only the catalog. The backfill
-- below then replays the same rule over every session's stored status events,
-- so a session's history keeps its time and a session running now keeps its
-- open interval. It is one scan of events, and the sessions lock the ALTER
-- takes is held until the run commits, so on a large log the upgrade blocks
-- appends for that long.
--
-- A rolling upgrade can miscount a session that changes status while replicas
-- on the earlier build are serving, since they write status events without
-- moving these columns: an interval they open is lost, and one they close
-- stays open until a later idle closes it, counting the gap between.
ALTER TABLE sessions
    ADD COLUMN active_seconds double precision NOT NULL DEFAULT 0,
    ADD COLUMN running_since timestamptz;

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
    -- interval (a reclaim) is not an edge.
    SELECT session_id, seq, processed_at, running
      FROM status
     WHERE running <> was_running
), span AS (
    SELECT session_id, running, processed_at AS opened,
           LEAD(processed_at) OVER (PARTITION BY session_id ORDER BY seq) AS closed
      FROM edge
)
UPDATE sessions
   SET active_seconds = clock.active, running_since = clock.since
  FROM (SELECT session_id,
               COALESCE(SUM(GREATEST(EXTRACT(EPOCH FROM closed - opened), 0))
                   FILTER (WHERE running AND closed IS NOT NULL), 0)::double precision AS active,
               MAX(opened) FILTER (WHERE running AND closed IS NULL) AS since
          FROM span
         GROUP BY session_id) clock
 WHERE sessions.id = clock.session_id;
