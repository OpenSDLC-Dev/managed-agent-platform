package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 0048 adds the given-URL index (#836) and fills nothing: a
// session's existing events stay as they were, with no index row, for its
// next lookup to catch up. Its index goes with the session by cascade.
func TestMigration0048IndexesNothingAndFollowsTheSession(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.FreshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.MigrateThrough(ctx, pool, "0047_drop_file_copy_guard.sql"); err != nil {
		t.Fatalf("migrate through 0047: %v", err)
	}
	seedSessionChain(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, session_id, seq, type, payload)
		VALUES ('sevt_1', 'sesn_1', 1, 'user.message', '{"content":[{"type":"text","text":"https://example.com/"}]}')`); err != nil {
		t.Fatal(err)
	}

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate the rest: %v", err)
	}
	var indexes, entries int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM session_given_url_indexes),
		(SELECT count(*) FROM session_given_urls)`).Scan(&indexes, &entries); err != nil {
		t.Fatal(err)
	}
	if indexes != 0 || entries != 0 {
		t.Errorf("after 0048: %d index rows and %d entries, want none: the migration backfills nothing", indexes, entries)
	}

	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO session_given_url_indexes (session_id, indexed_through)
		VALUES ('sesn_1', 1) RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO session_given_urls (index_id, url_key, seq, ord, rank, plain, spell_key, spelling)
		VALUES ($1, 1, 1, 0, 0, true, '\x01', 'https://example.com/')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sessions WHERE id = 'sesn_1'`); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM session_given_url_indexes),
		(SELECT count(*) FROM session_given_urls)`).Scan(&indexes, &entries); err != nil {
		t.Fatal(err)
	}
	if indexes != 0 || entries != 0 {
		t.Errorf("after the session's delete: %d index rows and %d entries, want none", indexes, entries)
	}
}

// 0048's foreign key takes sessions SHARE ROW EXCLUSIVE, which a writer of
// sessions holds against it. The migration waits for it at most its 2s
// lock_timeout (55P03), which, with one attempt allowed, fails the run and
// leaves the schema at 0047; a run once the writer commits applies it.
func TestMigration0048GivesUpOnABusySessionsTable(t *testing.T) {
	defer store.SetMigrateRetryForTest(1, 10*time.Millisecond)()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.FreshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.MigrateThrough(ctx, pool, "0047_drop_file_copy_guard.sql"); err != nil {
		t.Fatalf("migrate through 0047: %v", err)
	}
	seedSessionChain(t, pool)
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, `UPDATE sessions SET updated_at = now() WHERE id = 'sesn_1'`); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err = store.Migrate(ctx, pool)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("Migrate with a writer holding sessions = %v, want its lock_timeout (55P03)", err)
	}
	if waited := time.Since(start); waited > 10*time.Second {
		t.Errorf("Migrate gave up after %v, want about its 2s lock_timeout", waited)
	}
	var applied bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0048_session_given_urls.sql')`).Scan(&applied); err != nil || applied {
		t.Errorf("0048 applied = %v (%v) after giving up, want false", applied, err)
	}

	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after the writer committed = %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_given_urls`).Scan(new(int)); err != nil {
		t.Errorf("session_given_urls after the retry: %v", err)
	}
}
