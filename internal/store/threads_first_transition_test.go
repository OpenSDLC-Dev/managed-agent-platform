package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 0045 marks every thread that existed before it as already moved
// (#674), so the stats and usage it rendered as objects stay objects — a
// primary that never ran included, the conservative side the owner chose —
// while a thread written after it starts unmarked, for its first status
// transition to mark.
func TestThreadsBeforeTheMarkerAreMarkedMoved(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.FreshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.MigrateThrough(ctx, pool, "0044_memory_stores_archived_updated_at.sql"); err != nil {
		t.Fatalf("migrate through 0044: %v", err)
	}
	seedSessionChain(t, pool)
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, q := range []string{
		// A primary born idle that never ran, and a child that ran and idled.
		`INSERT INTO session_threads (id, session_id, agent_name, status, created_at, updated_at)
		 VALUES ('sthr_primary', 'sesn_1', 'a', 'idle', $1, $1)`,
		`INSERT INTO session_threads (id, session_id, parent_thread_id, agent, agent_name, status, created_at, updated_at)
		 VALUES ('sthr_child', 'sesn_1', 'sthr_primary', '{}', 'c', 'idle', $1, $1)`,
	} {
		if _, err := pool.Exec(ctx, q, created); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate the rest: %v", err)
	}
	for _, id := range []string{"sthr_primary", "sthr_child"} {
		var at *time.Time
		if err := pool.QueryRow(ctx,
			`SELECT first_transition_at FROM session_threads WHERE id = $1`, id).Scan(&at); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if at == nil || at.Before(created) {
			t.Errorf("%s first_transition_at = %v after migrating, want a mark no earlier than its creation %v", id, at, created)
		}
	}

	// The backfill is not a default: a row written afterwards starts unmarked.
	if _, err := pool.Exec(ctx,
		`INSERT INTO session_threads (id, session_id, parent_thread_id, agent, agent_name, status)
		 VALUES ('sthr_new', 'sesn_1', 'sthr_primary', '{}', 'n', 'idle')`); err != nil {
		t.Fatalf("insert after migrating: %v", err)
	}
	var marked bool
	if err := pool.QueryRow(ctx,
		`SELECT first_transition_at IS NOT NULL FROM session_threads WHERE id = 'sthr_new'`).Scan(&marked); err != nil {
		t.Fatal(err)
	}
	if marked {
		t.Error("a thread inserted after 0045 is born marked, want first_transition_at NULL")
	}
}
