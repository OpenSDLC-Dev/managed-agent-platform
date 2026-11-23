WITH bounds AS (
    SELECT now() AS as_of, (now() AT TIME ZONE 'UTC')::date - 29 AS first_day
), scoped AS (
    SELECT id, name, archived_at FROM agents
    WHERE org_id = $1 AND workspace_id = $2 AND NOT internal AND ($3 = '' OR id = $3)
), recent AS (
    SELECT s.id, s.agent_id, (s.created_at AT TIME ZONE 'UTC')::date AS day,
        (s.created_at AT TIME ZONE 'UTC')::date >= b.first_day AS current_period,
        s.active_seconds + CASE WHEN s.status = 'running'
            THEN GREATEST(0, extract(epoch FROM b.as_of - s.running_since)) ELSE 0 END AS active,
        COALESCE((
            SELECT e.type = 'session.status_idle' AND e.payload #>> '{stop_reason,type}' = 'retries_exhausted'
            FROM events e WHERE e.session_id = s.id
                AND e.type IN ('session.status_idle', 'session.status_running', 'session.status_rescheduled')
            ORDER BY e.seq DESC LIMIT 1
        ), false) AS failed
    FROM sessions s JOIN scoped a ON a.id = s.agent_id CROSS JOIN bounds b
    WHERE s.org_id = $1 AND s.workspace_id = $2
        AND s.created_at >= ((b.first_day - 30)::timestamp AT TIME ZONE 'UTC') AND s.created_at <= b.as_of
), periods AS (
    SELECT count(*) FILTER (WHERE current_period) AS sessions,
        count(*) FILTER (WHERE NOT current_period) AS previous,
        count(DISTINCT agent_id) FILTER (WHERE current_period) AS agents,
        count(*) FILTER (WHERE current_period AND failed) AS errors,
        count(*) FILTER (WHERE NOT current_period AND failed) AS previous_errors
    FROM recent
), rates AS (
    SELECT *, coalesce(100.0 * errors / nullif(sessions, 0), 0) AS error_rate,
        100.0 * previous_errors / nullif(previous, 0) AS previous_rate FROM periods
), activity AS (
    SELECT agent_id, count(*) AS sessions, 100.0 * count(*) FILTER (WHERE failed) / count(*) AS error_rate,
        percentile_cont(0.5) WITHIN GROUP (ORDER BY active) AS median_seconds
    FROM recent WHERE current_period GROUP BY agent_id
), agent_rows AS (
    SELECT a.*, coalesce(v.sessions, 0) AS sessions, coalesce(v.error_rate, 0) AS error_rate,
        v.median_seconds, (SELECT max(greatest(s.created_at, (
            SELECT e.created_at FROM events e WHERE e.session_id = s.id
              AND e.type IN ('session.status_idle','session.status_running','session.status_rescheduled','session.status_terminated')
            ORDER BY e.seq DESC LIMIT 1))) FROM sessions s
            WHERE s.agent_id = a.id AND s.org_id = $1 AND s.workspace_id = $2) AS last_active
    FROM scoped a LEFT JOIN activity v ON v.agent_id = a.id
), busiest AS (
    SELECT *, row_number() OVER (ORDER BY sessions DESC, name, id) AS position
    FROM agent_rows WHERE sessions > 0
), chart_groups AS (
    SELECT position, id, name FROM busiest WHERE position <= 5
    UNION ALL SELECT 6, NULL, 'Other' WHERE EXISTS (SELECT FROM busiest WHERE position > 5)
), daily_counts AS (
    SELECT least(b.position, 6) AS position, r.day, count(*) AS n
    FROM recent r JOIN busiest b ON b.id = r.agent_id WHERE r.current_period
    GROUP BY least(b.position, 6), r.day
), days AS (
    SELECT first_day + n AS day FROM bounds CROSS JOIN generate_series(0,29) AS n
), series AS (
    SELECT g.position, g.id, g.name, jsonb_agg(coalesce(c.n, 0) ORDER BY d.day) AS counts
    FROM chart_groups g CROSS JOIN days d LEFT JOIN daily_counts c ON c.day = d.day AND c.position = g.position
    GROUP BY g.position, g.id, g.name
), ranked AS (
    SELECT *, row_number() OVER (ORDER BY /* table order */) AS position FROM agent_rows
)
SELECT jsonb_build_object(
    'summary', jsonb_build_object('sessions', sessions, 'agents', agents, 'error_rate', error_rate,
        'session_change', 100.0 * (sessions - previous) / nullif(previous, 0),
        'error_change', 100.0 * (error_rate - previous_rate) / nullif(previous_rate, 0)),
    'days', (SELECT jsonb_agg(day ORDER BY day) FROM days),
    'series', coalesce((SELECT jsonb_agg(to_jsonb(series) - 'position' ORDER BY position) FROM series), '[]'),
    'active_time', coalesce((SELECT jsonb_agg(to_jsonb(b) - 'position' ORDER BY position) FROM busiest b WHERE position <= 8), '[]'),
    'agents', coalesce((SELECT jsonb_agg(to_jsonb(r) - 'position' ORDER BY position) FROM ranked r WHERE position <= 10), '[]'),
    'total', (SELECT count(*) FROM scoped)
) FROM rates
