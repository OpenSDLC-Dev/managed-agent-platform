package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 0055 backfills each session's active_seconds from its stored
// session-level status events (plan 68): every running interval an idle,
// rescheduled or terminated closes, and the interval still open as
// running_since. A running inside an open interval restarts it, as the live
// rule does. Thread status events do not count; the session's own fold
// already counts concurrent threads once.
func TestSessionActiveSecondsBackfill(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.FreshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.MigrateThrough(ctx, pool, "0054_session_nested_skills.sql"); err != nil {
		t.Fatalf("migrate through 0054: %v", err)
	}
	const (
		done    = "sesn_0123456789abcdefghjkmnpqr1"
		running = "sesn_0123456789abcdefghjkmnpqr2"
		fresh   = "sesn_0123456789abcdefghjkmnpqr3"
	)
	fixture := []string{
		`INSERT INTO agents (id, name, version, spec) VALUES ('agent_legacy', 'legacy', 1, '{"model":{"id":"m"}}')`,
		`INSERT INTO agent_versions (agent_id, version, name, spec) VALUES ('agent_legacy', 1, 'legacy', '{"model":{"id":"m"}}')`,
		`INSERT INTO environments (id, name, kind, config) VALUES ('env_legacy', 'legacy', 'cloud', '{"type":"cloud"}')`,
	}
	for _, id := range []string{done, running, fresh} {
		fixture = append(fixture, `INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id, status)
		 VALUES ('`+id+`', 'agent_legacy', 1, '{"type":"agent","id":"agent_legacy","version":1,"name":"legacy","model":{"id":"m"}}', 'env_legacy', 'idle')`)
	}
	for _, q := range fixture {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("legacy fixture: %v\n%s", err, q)
		}
	}
	for _, ev := range []struct {
		session, typ, at string
		seq              int
	}{
		{done, "session.status_running", "2026-10-17T01:00:00Z", 1},
		{done, "session.thread_status_running", "2026-10-17T01:00:00Z", 2},
		{done, "session.status_idle", "2026-10-17T01:00:02.5Z", 3},
		{done, "session.status_running", "2026-10-17T01:00:10Z", 4},
		{done, "session.status_rescheduled", "2026-10-17T01:00:11Z", 5},
		{done, "session.status_running", "2026-10-17T01:00:12Z", 6},
		{done, "session.status_running", "2026-10-17T01:00:12.25Z", 7},
		{done, "session.status_idle", "2026-10-17T01:00:13.5Z", 8},
		{done, "session.thread_status_idle", "2026-10-17T01:00:20Z", 9},
		{done, "session.status_idle", "2026-10-17T01:00:30Z", 10},
		// Two hosts' clocks: an idle stamped before the running it closes.
		{done, "session.status_running", "2026-10-17T01:00:40Z", 11},
		{done, "session.status_idle", "2026-10-17T01:00:39Z", 12},
		{running, "session.status_running", "2026-10-17T02:00:00Z", 1},
		{running, "session.status_idle", "2026-10-17T02:00:01Z", 2},
		{running, "session.status_running", "2026-10-17T02:00:05Z", 3},
		{running, "session.thread_status_idle", "2026-10-17T02:00:06Z", 4},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO events (id, session_id, seq, type, payload, processed_at) VALUES ($1, $2, $3, $4, '{}', $5)`,
			fmt.Sprintf("sevt_legacy_%s_%d", ev.session[len(ev.session)-1:], ev.seq), ev.session, ev.seq, ev.typ, ev.at); err != nil {
			t.Fatalf("legacy event %+v: %v", ev, err)
		}
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate the rest: %v", err)
	}

	for _, want := range []struct {
		id     string
		active float64
		since  *time.Time
	}{
		// 2.5, then 1 to the reschedule, then 1.25 from the running that
		// restarted the interval at 12.25; the skewed pair adds nothing.
		{done, 2.5 + 1 + 1.25, nil},
		{running, 1, ptr(time.Date(2026, 10, 17, 2, 0, 5, 0, time.UTC))},
		{fresh, 0, nil},
	} {
		var (
			active float64
			since  *time.Time
		)
		if err := pool.QueryRow(ctx, `SELECT active_seconds, running_since FROM sessions WHERE id = $1`, want.id).
			Scan(&active, &since); err != nil {
			t.Fatalf("%s: %v", want.id, err)
		}
		if active != want.active || (since == nil) != (want.since == nil) || (since != nil && !since.Equal(*want.since)) {
			t.Errorf("%s: active_seconds = %v running_since = %v, want %v and %v", want.id, active, since, want.active, want.since)
		}
	}
	// The scratch table the scan fills is the migration's own, dropped with it.
	var leftover bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('session_clock_0055') IS NOT NULL`).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover {
		t.Error("migration 0055 left its scratch table session_clock_0055 behind")
	}
}

func ptr[T any](v T) *T { return &v }
