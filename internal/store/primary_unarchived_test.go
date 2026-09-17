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

// legacyMirror is the write #713 removed from the session archive
// (internal/api/sessions.go before 8238d4dc): the session's archived_at and
// updated_at copied onto its primary. A replica still running a build from
// before #713 keeps making it.
const legacyMirror = `UPDATE session_threads SET archived_at = $2, updated_at = $3
  WHERE session_id = $1 AND parent_thread_id IS NULL AND archived_at IS NULL`

// childEnding is the child thread's own archive write (internal/api/threads.go),
// which the thread archive and the session's end both make.
const childEnding = `UPDATE session_threads SET archived_at = now(), updated_at = now() WHERE id = 'sthr_child'`

// seedThreads gives sesn_1 an unarchived primary thread and one child, the two
// row shapes 0041 tells apart by parent_thread_id.
func seedThreads(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	seedSessionChain(t, pool)
	for _, q := range []string{
		`INSERT INTO session_threads (id, session_id, agent_name, status) VALUES ('sthr_primary', 'sesn_1', 'a', 'idle')`,
		`INSERT INTO session_threads (id, session_id, parent_thread_id, agent, agent_name, status)
		 VALUES ('sthr_child', 'sesn_1', 'sthr_primary', '{}', 'c', 'idle')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed threads: %v", err)
		}
	}
}

// threadStamps reads a thread's archived_at and updated_at as instants, so no
// assertion depends on the session's TimeZone.
func threadStamps(t *testing.T, pool *pgxpool.Pool, id string) (archivedAt *time.Time, updatedAt time.Time) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT archived_at, updated_at FROM session_threads WHERE id = $1`, id).Scan(&archivedAt, &updatedAt); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return archivedAt, updatedAt
}

// rowVersion reads a thread row's xmin, which moves whenever the row is
// rewritten, even to the values it already held.
func rowVersion(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var xmin string
	if err := pool.QueryRow(context.Background(),
		`SELECT xmin::text FROM session_threads WHERE id = $1`, id).Scan(&xmin); err != nil {
		t.Fatalf("read %s's xmin: %v", id, err)
	}
	return xmin
}

// TestPrimaryThreadNeverArchived pins 0041's constraint: a primary thread row
// cannot carry archived_at, while a child's can, because a child's archive and
// its session's end both stamp one (#720).
func TestPrimaryThreadNeverArchived(t *testing.T) {
	pool := open(t, pgtest.FreshDB(t))
	ctx := context.Background()
	seedThreads(t, pool)
	if _, err := pool.Exec(ctx,
		`INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id)
		 VALUES ('sesn_2', 'agent_1', 1, '{}', 'env_1')`); err != nil {
		t.Fatalf("second session: %v", err)
	}

	archive := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	refused := []struct {
		name string
		q    string
		args []any
	}{
		{"the pre-#713 mirror", legacyMirror, []any{"sesn_1", archive, archive}},
		{"a primary inserted stamped", `INSERT INTO session_threads (id, session_id, agent_name, status, archived_at)
		                                VALUES ('sthr_primary2', 'sesn_2', 'a', 'idle', now())`, nil},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tc.q, tc.args...)
			var pgErr *pgconn.PgError
			// By name, as TestFileScopePairAgrees does: 0025's agent CHECK on this
			// table answers with the same SQLSTATE.
			if !errors.As(err, &pgErr) || pgErr.Code != pgCheckViolation || pgErr.ConstraintName != "session_threads_primary_unarchived" {
				t.Errorf("%s => %v, want %s from session_threads_primary_unarchived", tc.name, err, pgCheckViolation)
			}
		})
	}

	if _, err := pool.Exec(ctx, childEnding); err != nil {
		t.Errorf("a child's archive rejected: %v", err)
	}
}

// TestPrimaryThreadCheckClearsLateStamps pins what a fresh database cannot
// show. A replica from before #713 could stamp a primary after 0040 cleared
// them, and a validated CHECK reads existing rows, so 0041 clears again before
// adding it. Without the clear the migration fails, and with it the startup
// that applies it; with a clear too wide, a child's archive would be undone.
// This is also the retry after a stamp lands between the clear and the ALTER:
// by then it is committed, as here.
func TestPrimaryThreadCheckClearsLateStamps(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.FreshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.MigrateThrough(ctx, pool, "0040_primary_thread_unarchived.sql"); err != nil {
		t.Fatalf("migrate through 0040: %v", err)
	}
	seedThreads(t, pool)
	archive := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, legacyMirror, "sesn_1", archive, archive); err != nil {
		t.Fatalf("stamp the primary as a pre-#713 replica would: %v", err)
	}
	if _, err := pool.Exec(ctx, childEnding); err != nil {
		t.Fatalf("archive the child: %v", err)
	}
	childBefore, _ := threadStamps(t, pool, "sthr_child")
	// A primary with nothing to clear, whose row version the clear must not move.
	for _, q := range []string{
		`INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id)
		 VALUES ('sesn_2', 'agent_1', 1, '{}', 'env_1')`,
		`INSERT INTO session_threads (id, session_id, agent_name, status) VALUES ('sthr_unstamped', 'sesn_2', 'a', 'idle')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed an unstamped primary: %v", err)
		}
	}
	unstampedVersion := rowVersion(t, pool, "sthr_unstamped")

	if err := store.MigrateThrough(ctx, pool, "0041_primary_thread_unarchived_check.sql"); err != nil {
		t.Fatalf("0041 over a primary stamped after 0040: %v", err)
	}
	primaryArchived, primaryUpdated := threadStamps(t, pool, "sthr_primary")
	if primaryArchived != nil {
		t.Errorf("primary archived_at = %v after 0041, want NULL", *primaryArchived)
	}
	// The clear touches archived_at alone, as 0040's did: the mirror's updated_at
	// stays, since what it replaced is recorded nowhere.
	if !primaryUpdated.Equal(archive) {
		t.Errorf("primary updated_at = %v after 0041, want the mirror's %v", primaryUpdated, archive)
	}
	if childAfter, _ := threadStamps(t, pool, "sthr_child"); childAfter == nil || !childAfter.Equal(*childBefore) {
		t.Errorf("child archived_at = %v after 0041, want its own %v kept", childAfter, *childBefore)
	}
	// The WHERE's archived_at IS NOT NULL, for 0040's reason: without it every
	// primary is rewritten to the same values inside the startup transaction.
	if got := rowVersion(t, pool, "sthr_unstamped"); got != unstampedVersion {
		t.Errorf("unstamped primary's xmin moved from %s to %s, want the clear to leave it unwritten", unstampedVersion, got)
	}
	// Validated, not NOT VALID: a NOT VALID CHECK refuses new writes just the
	// same, so only the catalog shows the existing rows were read.
	var validated bool
	if err := pool.QueryRow(ctx,
		`SELECT convalidated FROM pg_constraint
		  WHERE conname = 'session_threads_primary_unarchived' AND conrelid = 'session_threads'::regclass`).Scan(&validated); err != nil {
		t.Fatalf("read the constraint: %v", err)
	}
	if !validated {
		t.Error("session_threads_primary_unarchived landed NOT VALID, want it validated over existing rows")
	}
}
