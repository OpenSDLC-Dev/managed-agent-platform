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

// Migration 0046 gives every files row the key its bytes are at (#578). A row
// written before it is backfilled to the key its id derived, which is where
// those bytes are, and a row a previous build inserts afterwards, naming no
// object_key, is filled the same way by the trigger. blob.FilesKey is the
// layout both have to agree with: the writers still put bytes there.
func TestFilesObjectKeyBackfillAndDefault(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.FreshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.MigrateThrough(ctx, pool, "0045_session_threads_transitioned.sql"); err != nil {
		t.Fatalf("migrate through 0045: %v", err)
	}
	for _, q := range []string{
		// An upload and a harvested output, as the previous build wrote them.
		`INSERT INTO files (id, filename, mime_type, size_bytes) VALUES ('file_legacyupload', 'a.txt', 'text/plain', 1)`,
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
		 VALUES ('file_legacyoutput', 'out.txt', 'text/plain', 1, true, 'session', 'sesn_1')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate the rest: %v", err)
	}
	// The previous build's INSERT, after the migration: no object_key named.
	if _, err := pool.Exec(ctx,
		`INSERT INTO files (id, filename, mime_type, size_bytes) VALUES ('file_oldbinary', 'b.txt', 'text/plain', 1)`); err != nil {
		t.Fatalf("an insert naming no object_key: %v", err)
	}
	// This build's INSERT of a copy, naming its source's key.
	if _, err := pool.Exec(ctx,
		`INSERT INTO files (id, filename, mime_type, size_bytes, scope_type, scope_id, object_key, source_file_id)
		 VALUES ('file_copy', 'a.txt', 'text/plain', 1, 'session', 'sesn_1', 'files/file_legacyupload', 'file_legacyupload')`); err != nil {
		t.Fatalf("insert a copy: %v", err)
	}
	for id, want := range map[string]string{
		"file_legacyupload": blob.FilesKey("file_legacyupload"),
		"file_legacyoutput": blob.FilesKey("file_legacyoutput"),
		"file_oldbinary":    blob.FilesKey("file_oldbinary"),
		"file_copy":         blob.FilesKey("file_legacyupload"),
	} {
		var got string
		if err := pool.QueryRow(ctx, `SELECT object_key FROM files WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if got != want {
			t.Errorf("%s: object_key = %q, want %q", id, got, want)
		}
	}
	var source *string
	if err := pool.QueryRow(ctx, `SELECT source_file_id FROM files WHERE id = 'file_legacyupload'`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != nil {
		t.Errorf("a legacy row's source_file_id = %q, want NULL", *source)
	}
}

// The harvest's per-path uniqueness holds for its own rows and not for the
// copies a session's mounts mint: two resources mounting one upload get two
// copies with one filename (2026-09-02 batch2 idx 388), and a copy may share a
// name with an output.
func TestFilesScopeFilenameUniqueSkipsCopies(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	insert := func(id, source string) error {
		var src any
		if source != "" {
			src = source
		}
		_, err := pool.Exec(ctx,
			`INSERT INTO files (id, filename, mime_type, size_bytes, scope_type, scope_id, object_key, source_file_id)
			 VALUES ($1, 'notes.txt', 'text/plain', 1, 'session', 'sesn_1', 'files/' || coalesce($2, $1), $2)`,
			id, src)
		return err
	}
	for _, id := range []string{"file_c1", "file_c2"} {
		if err := insert(id, "file_upload"); err != nil {
			t.Fatalf("copy %s: %v", id, err)
		}
	}
	if err := insert("file_out1", ""); err != nil {
		t.Fatalf("an output sharing a copy's name: %v", err)
	}
	err := insert("file_out2", "")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgUniqueViolation {
		t.Errorf("a second output at the same path: err = %v, want a unique violation", err)
	}
}

// pendingKeys reads the object-delete queue, sorted.
func pendingKeys(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT object_key FROM pending_object_deletes ORDER BY object_key`)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// The queue drops a key some files row still names, whoever enqueues it — the
// reference count that lets a session's copy outlive its upload (#578) — and
// takes every other key, a skill archive's included.
func TestEnqueueObjectDeletesSkipsReferencedKeys(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	if _, err := pool.Exec(ctx,
		`INSERT INTO files (id, filename, mime_type, size_bytes, scope_type, scope_id, object_key, source_file_id)
		 VALUES ('file_copy', 'a.txt', 'text/plain', 1, 'session', 'sesn_1', 'files/file_upload', 'file_upload')`); err != nil {
		t.Fatal(err)
	}
	enqueue := func(keys ...string) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := store.EnqueueObjectDeletes(ctx, tx, keys); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// The upload's row is gone and its copy lives: the key is not owed. The
	// previous build's raw INSERT is held to the same count.
	enqueue("files/file_upload", "files/file_gone", "skills/skill_x/1.zip")
	if _, err := pool.Exec(ctx, store.PendingObjectDeleteInsertSQL, []string{"files/file_upload"}); err != nil {
		t.Fatal(err)
	}
	if got, want := pendingKeys(t, pool), []string{"files/file_gone", "skills/skill_x/1.zip"}; !slices.Equal(got, want) {
		t.Errorf("queue = %v, want %v", got, want)
	}
	// The last row naming it goes: now it is owed.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var key string
	if err := tx.QueryRow(ctx, `DELETE FROM files WHERE id = 'file_copy' RETURNING object_key`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueObjectDeletes(ctx, tx, []string{key}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pendingKeys(t, pool); !slices.Contains(got, "files/file_upload") {
		t.Errorf("queue = %v after the last row naming files/file_upload went, want it owed", got)
	}
}

// Two transactions each deleting one of the last two rows that name a key
// would, under READ COMMITTED, each see the other's row and skip the key, and
// the object would never be deleted. The trigger's advisory lock makes the
// second wait out the first and then see its delete, so the key is owed once
// both commit.
func TestConcurrentLastDeletesStillOweTheObject(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	const key = "files/file_upload"
	for _, q := range []string{
		`INSERT INTO files (id, filename, mime_type, size_bytes, object_key) VALUES ('file_upload', 'a.txt', 'text/plain', 1, 'files/file_upload')`,
		`INSERT INTO files (id, filename, mime_type, size_bytes, scope_type, scope_id, object_key, source_file_id)
		 VALUES ('file_copy', 'a.txt', 'text/plain', 1, 'session', 'sesn_1', 'files/file_upload', 'file_upload')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	deleteAndEnqueue := func(tx pgx.Tx, id string) error {
		if _, err := tx.Exec(ctx, `DELETE FROM files WHERE id = $1`, id); err != nil {
			return err
		}
		return store.EnqueueObjectDeletes(ctx, tx, []string{key})
	}

	first, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Rollback(ctx) }()
	second, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Rollback(ctx) }()
	// The second deletes its row first, so the first's check sees that row
	// still there, takes the lock, and skips the key.
	if _, err := second.Exec(ctx, `DELETE FROM files WHERE id = 'file_copy'`); err != nil {
		t.Fatal(err)
	}
	if err := deleteAndEnqueue(first, "file_upload"); err != nil {
		t.Fatal(err)
	}
	var secondPID uint32
	if err := second.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&secondPID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- store.EnqueueObjectDeletes(ctx, second, []string{key}) }()
	// The second's check sees the first's row, so it waits on the lock the
	// first holds to its commit.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid = $1 AND locktype = 'advisory' AND NOT granted)`,
			secondPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the second enqueue finished without waiting for the first (err %v)", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the second enqueue never waited on the advisory lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := second.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pendingKeys(t, pool); !slices.Equal(got, []string{key}) {
		t.Errorf("queue = %v, want [%s]: the last delete owes the object", got, key)
	}
}
