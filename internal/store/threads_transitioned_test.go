package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 0045 flags every thread that existed before it as transitioned
// (#674), so the stats and usage it rendered as objects stay objects — a
// primary that never ran included, the conservative side the owner chose —
// while a thread written after it starts unflagged, for its first status
// transition to flag.
func TestThreadsBeforeTheFlagAreFlaggedTransitioned(t *testing.T) {
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
		var flagged bool
		if err := pool.QueryRow(ctx,
			`SELECT transitioned FROM session_threads WHERE id = $1`, id).Scan(&flagged); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if !flagged {
			t.Errorf("%s is unflagged after migrating, want transitioned", id)
		}
	}

	// The backfill is not the default: a row written afterwards starts
	// unflagged.
	if _, err := pool.Exec(ctx,
		`INSERT INTO session_threads (id, session_id, parent_thread_id, agent, agent_name, status)
		 VALUES ('sthr_new', 'sesn_1', 'sthr_primary', '{}', 'n', 'idle')`); err != nil {
		t.Fatalf("insert after migrating: %v", err)
	}
	var flagged bool
	if err := pool.QueryRow(ctx,
		`SELECT transitioned FROM session_threads WHERE id = 'sthr_new'`).Scan(&flagged); err != nil {
		t.Fatal(err)
	}
	if flagged {
		t.Error("a thread inserted after 0045 is born flagged, want transitioned false")
	}
}
