package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// triggersAndFunctions lists the user triggers on the files tables and the
// functions 0046 defined, by name, sorted, so a test can see what a migration
// left of them.
func triggersAndFunctions(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT 'trigger ' || c.relname || '.' || tg.tgname
		  FROM pg_trigger tg JOIN pg_class c ON c.oid = tg.tgrelid
		 WHERE NOT tg.tgisinternal
		   AND c.relname IN ('files', 'deleted_sessions', 'pending_object_deletes')
		UNION ALL
		SELECT 'function ' || p.proname
		  FROM pg_proc p
		 WHERE p.proname IN ('files_copy_delete_guard', 'files_follow_session',
		                     'files_name_object', 'pending_object_deletes_skip_referenced')
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Migration 0047 drops 0046's copy-delete guard and its tombstone trigger
// (#856), which served binaries built before 0046 alone, and keeps the
// reference count on the delete queue, which this build's own removers need.
// After it, a copy is deleted like any other row by a transaction that set
// nothing, and a session's tombstone leaves its files to the session delete,
// which takes them itself.
func TestMigration0047DropsTheCopyGuardAndTheTombstoneTrigger(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.FreshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.MigrateThrough(ctx, pool, "0046_files_object_key.sql"); err != nil {
		t.Fatalf("migrate through 0046: %v", err)
	}
	kept := []string{
		"function files_name_object",
		"function pending_object_deletes_skip_referenced",
		"trigger pending_object_deletes.pending_object_deletes_skip_referenced",
	}
	dropped := []string{
		"function files_copy_delete_guard",
		"function files_follow_session",
		"trigger deleted_sessions.files_follow_session",
		"trigger files.files_copy_delete_guard",
	}
	want := slices.Sorted(slices.Values(append(slices.Clone(kept), dropped...)))
	if got := triggersAndFunctions(t, pool); !slices.Equal(got, want) {
		t.Fatalf("after 0046: %v, want %v", got, want)
	}
	seedSessionChain(t, pool)
	insertUpload(t, pool, "file_upload")
	insertCopy(t, pool, "file_copy", "file_upload", blob.FilesKey("file_upload"), "sesn_1")
	if _, err := pool.Exec(ctx,
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
		 VALUES ('file_output', 'out.txt', 'text/plain', 1, true, 'session', 'sesn_1')`); err != nil {
		t.Fatal(err)
	}

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate the rest: %v", err)
	}
	if got := triggersAndFunctions(t, pool); !slices.Equal(got, kept) {
		t.Errorf("after 0047: %v, want only the reference count's: %v", got, kept)
	}

	// A tombstone takes nothing: the session delete that writes it deletes
	// the session's files itself.
	if _, err := pool.Exec(ctx, store.SessionTombstoneInsertSQL, "sesn_1"); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE scope_id = 'sesn_1'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 2 {
		t.Errorf("%d of the session's 2 files left after its tombstone, want both", left)
	}
	// A plain DELETE takes a copy, and the count still keeps the object its
	// upload names.
	inObjectDeleteTx(t, pool, func(tx store.ObjectDeleteTx) error {
		var key string
		if err := tx.QueryRow(ctx, `DELETE FROM files WHERE id = 'file_copy' RETURNING `+store.FileObjectKeySQL).Scan(&key); err != nil {
			return err
		}
		return store.EnqueueObjectDeletes(ctx, tx, []string{key})
	})
	if got := pendingKeys(t, pool); len(got) != 0 {
		t.Errorf("queue = %v after deleting the copy, want nothing: its upload still names the object", got)
	}
}

// 0047 drops a trigger on each of deleted_sessions and files, which takes the
// table ACCESS EXCLUSIVE, and it locks them in the order a session delete
// takes them, the tombstone first. Here a transaction holds deleted_sessions
// as a session delete's tombstone insert holds it: the migration waits for it
// holding nothing on files, so that delete's own DELETE FROM files still goes
// through rather than closing a cycle. The wait is bounded by the
// migration's lock_timeout (55P03), which, with one attempt allowed, fails
// the run and leaves the schema at 0046, and a run once the holder commits
// applies it.
func TestMigration0047LocksTheTombstoneBeforeTheFiles(t *testing.T) {
	defer store.SetMigrateRetryForTest(1, 10*time.Millisecond)()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.FreshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.MigrateThrough(ctx, pool, "0046_files_object_key.sql"); err != nil {
		t.Fatalf("migrate through 0046: %v", err)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `LOCK TABLE deleted_sessions IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	migrated := make(chan error, 1)
	go func() { migrated <- store.Migrate(ctx, pool) }()
	var migrator int32
	for deadline := time.Now().Add(15 * time.Second); ; {
		err := pool.QueryRow(ctx, `SELECT pid FROM pg_locks
			WHERE relation = 'deleted_sessions'::regclass AND mode = 'AccessExclusiveLock' AND NOT granted`).Scan(&migrator)
		if err == nil {
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case err := <-migrated:
			t.Fatalf("Migrate returned %v without waiting for deleted_sessions", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("0047 never waited for deleted_sessions within 15s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var onFiles []string
	rows, err := pool.Query(ctx, `SELECT mode FROM pg_locks WHERE pid = $1 AND relation = 'files'::regclass`, migrator)
	if err != nil {
		t.Fatal(err)
	}
	if onFiles, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if len(onFiles) != 0 {
		t.Errorf("the migration holds %v on files while it waits for deleted_sessions; want nothing", onFiles)
	}
	for _, q := range []string{
		`SET LOCAL lock_timeout = '500ms'`,
		`LOCK TABLE files IN ROW EXCLUSIVE MODE`, // what the delete's DELETE FROM files takes
	} {
		if _, err := holder.Exec(ctx, q); err != nil {
			t.Fatalf("the session delete's %s while 0047 waits: %v", q, err)
		}
	}

	select {
	case err := <-migrated:
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("Migrate with the tombstone table held = %v, want its lock_timeout (55P03)", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Migrate still waiting 15s on, past its 2s lock_timeout")
	}
	var applied bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0047_drop_file_copy_guard.sql')`).Scan(&applied); err != nil || applied {
		t.Errorf("0047 applied = %v (%v) after giving up, want false", applied, err)
	}
	if got := triggersAndFunctions(t, pool); !slices.Contains(got, "trigger files.files_copy_delete_guard") {
		t.Errorf("after the give-up: %v, want 0046's guard still in place", got)
	}

	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after the holder committed = %v", err)
	}
	if got := triggersAndFunctions(t, pool); slices.Contains(got, "trigger files.files_copy_delete_guard") {
		t.Errorf("after 0047: %v, want the guard gone", got)
	}
}
