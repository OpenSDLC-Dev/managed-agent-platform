package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 0044 gives a memory store archived before #685 the updated_at the
// archive route has written since: its archived_at. An archived store refuses
// every update, so nothing can have moved the column after the archive. A
// store never archived keeps its own.
func TestArchivedMemoryStoresBackfillUpdatedAt(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.FreshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.MigrateThrough(ctx, pool, "0043_work_session_tokens_unkeyed.sql"); err != nil {
		t.Fatalf("migrate through 0043: %v", err)
	}
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	renamed := created.Add(time.Hour)
	archived := created.Add(2 * time.Hour)
	for _, q := range []struct {
		q    string
		args []any
	}{
		// Renamed, then archived by a build before #685, which left updated_at
		// at the rename.
		{`INSERT INTO memory_stores (id, name, created_at, updated_at, archived_at)
		  VALUES ('memstore_stale', 's', $1, $2, $3)`, []any{created, renamed, archived}},
		// Renamed and never archived: the rename's stamp is its own.
		{`INSERT INTO memory_stores (id, name, created_at, updated_at)
		  VALUES ('memstore_live', 'l', $1, $2)`, []any{created, renamed}},
	} {
		if _, err := pool.Exec(ctx, q.q, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate the rest: %v", err)
	}
	for _, tc := range []struct {
		id   string
		want time.Time
	}{
		{"memstore_stale", archived},
		{"memstore_live", renamed},
	} {
		var updatedAt time.Time
		if err := pool.QueryRow(ctx,
			`SELECT updated_at FROM memory_stores WHERE id = $1`, tc.id).Scan(&updatedAt); err != nil {
			t.Fatalf("read %s: %v", tc.id, err)
		}
		if !updatedAt.Equal(tc.want) {
			t.Errorf("%s updated_at = %v after migrating, want %v", tc.id, updatedAt, tc.want)
		}
	}
}
