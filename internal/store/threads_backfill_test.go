package store_test

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 0025 backfills one primary thread per session that existed before
// it (plan 35 decision 1): the derived sthr_ id, the session's status, usage
// and timestamps, agent_name from the resolved agent, agent NULL.
func TestSessionThreadsBackfillsLegacySessions(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.FreshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.MigrateThrough(ctx, pool, "0024_api_keys_lifecycle.sql"); err != nil {
		t.Fatalf("migrate through 0024: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO agents (id, name, version, spec) VALUES ('agent_legacy', 'legacy', 1, '{"model":{"id":"m"}}')`,
		`INSERT INTO agent_versions (agent_id, version, name, spec) VALUES ('agent_legacy', 1, 'legacy', '{"model":{"id":"m"}}')`,
		`INSERT INTO environments (id, name, kind, config) VALUES ('env_legacy', 'legacy', 'cloud', '{"type":"cloud"}')`,
		`INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id, status, usage, archived_at)
		 VALUES ('sesn_0123456789abcdefghjkmnpqrs', 'agent_legacy', 1, '{"type":"agent","id":"agent_legacy","version":1,"name":"legacy","model":{"id":"m"}}',
		         'env_legacy', 'idle', '{"input_tokens":7}', now())`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("legacy fixture: %v", err)
		}
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate the rest: %v", err)
	}
	var (
		id, name, status string
		parent, agent    *string
		usage            []byte
		archivedAt       *string
	)
	if err := pool.QueryRow(ctx,
		`SELECT id, agent_name, status, parent_thread_id, agent::text, usage::text, archived_at::text
		   FROM session_threads WHERE session_id = 'sesn_0123456789abcdefghjkmnpqrs'`).
		Scan(&id, &name, &status, &parent, &agent, &usage, &archivedAt); err != nil {
		t.Fatalf("backfilled primary thread: %v", err)
	}
	if id != "sthr_0123456789abcdefghjkmnpqrs" || name != "legacy" || status != "idle" ||
		parent != nil || agent != nil || string(usage) != `{"input_tokens": 7}` || archivedAt == nil {
		t.Errorf("backfilled row = %s %s %s parent=%v agent=%v usage=%s archived=%v",
			id, name, status, parent, agent, usage, archivedAt)
	}
	var crossPosted bool
	if err := pool.QueryRow(ctx,
		`SELECT column_default::boolean FROM information_schema.columns
		  WHERE table_name = 'events' AND column_name = 'cross_posted'`).Scan(&crossPosted); err != nil || crossPosted {
		t.Errorf("events.cross_posted default: %v %v, want false", crossPosted, err)
	}
}
