package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// insertCopy writes a session's copy of source, naming key, as
// internal/api's mountFileCopy does.
func insertCopy(t *testing.T, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, id, source, key, sessionID string) {
	t.Helper()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO files (id, filename, mime_type, size_bytes, scope_type, scope_id, object_key, source_file_id)
		 VALUES ($1, 'a.txt', 'text/plain', 1, 'session', $2, $3, $4)`, id, sessionID, key, source); err != nil {
		t.Fatalf("insert the copy %s: %v", id, err)
	}
}

// insertUpload writes an upload as every build writes it, naming no key.
func insertUpload(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO files (id, filename, mime_type, size_bytes) VALUES ($1, 'a.txt', 'text/plain', 1)`, id); err != nil {
		t.Fatalf("insert the upload %s: %v", id, err)
	}
}

// Migration 0046 lets a files row name another row's object (#578) without
// touching a row that owns its own: an existing row keeps a NULL object_key,
// which reads as the key its id derives, and so does a row a previous build
// inserts afterwards, naming no key. blob.FilesKey is the layout both have to
// agree with: the writers still put bytes there. The table is not rewritten —
// a rewrite gives it a new file — so the migration holds files for its index
// builds only.
func TestFilesObjectKeyMigratesWithoutARewrite(t *testing.T) {
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
	var before, after uint32
	if err := pool.QueryRow(ctx, `SELECT pg_relation_filenode('files')`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate the rest: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT pg_relation_filenode('files')`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("files was rewritten (filenode %d, then %d); 0046 should only add columns and indexes", before, after)
	}
	insertUpload(t, pool, "file_oldbinary") // the previous build's INSERT, after the migration
	insertCopy(t, pool, "file_copy", "file_legacyupload", blob.FilesKey("file_legacyupload"), "sesn_1")
	for id, want := range map[string]string{
		"file_legacyupload": blob.FilesKey("file_legacyupload"),
		"file_legacyoutput": blob.FilesKey("file_legacyoutput"),
		"file_oldbinary":    blob.FilesKey("file_oldbinary"),
		"file_copy":         blob.FilesKey("file_legacyupload"),
	} {
		var got string
		if err := pool.QueryRow(ctx, `SELECT `+store.FileObjectKeySQL+` FROM files WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if got != want {
			t.Errorf("%s: key = %q, want %q", id, got, want)
		}
	}
	var key, source *string
	if err := pool.QueryRow(ctx, `SELECT object_key, source_file_id FROM files WHERE id = 'file_legacyupload'`).Scan(&key, &source); err != nil {
		t.Fatal(err)
	}
	if key != nil || source != nil {
		t.Errorf("a legacy row's object_key, source_file_id = %v, %v; want both NULL", key, source)
	}
	// The two columns are a copy's together or no row's.
	for _, q := range []string{
		`INSERT INTO files (id, filename, mime_type, size_bytes, object_key) VALUES ('file_half1', 'a', 'text/plain', 1, 'files/x')`,
		`INSERT INTO files (id, filename, mime_type, size_bytes, source_file_id) VALUES ('file_half2', 'a', 'text/plain', 1, 'file_x')`,
	} {
		_, err := pool.Exec(ctx, q)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != pgCheckViolation {
			t.Errorf("%s => %v, want a check violation", q, err)
		}
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
		var src, key any
		if source != "" {
			src, key = source, blob.FilesKey(source)
		}
		_, err := pool.Exec(ctx,
			`INSERT INTO files (id, filename, mime_type, size_bytes, scope_type, scope_id, object_key, source_file_id)
			 VALUES ($1, 'notes.txt', 'text/plain', 1, 'session', 'sesn_1', $2, $3)`,
			id, key, src)
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

// inTx runs fn in a transaction and commits it.
func inTx(t *testing.T, pool *pgxpool.Pool, fn func(pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// The queue drops a key some files row still names, whoever enqueues it — the
// reference count that lets a session's copy outlive its upload (#578) — and
// takes every other key, a skill archive's included. An owner names its key by
// its id, a copy by its object_key.
func TestEnqueueObjectDeletesSkipsReferencedKeys(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	insertUpload(t, pool, "file_owner")
	insertCopy(t, pool, "file_copy", "file_upload", blob.FilesKey("file_upload"), "sesn_1")
	enqueue := func(keys ...string) {
		t.Helper()
		inTx(t, pool, func(tx pgx.Tx) error { return store.EnqueueObjectDeletes(ctx, tx, keys) })
	}
	// file_upload's row is gone and its copy lives; file_owner's row lives.
	// Neither key is owed. The previous build's raw INSERT is held to the same
	// count.
	enqueue("files/file_upload", "files/file_owner", "files/file_gone", "skills/skill_x/1.zip")
	if _, err := pool.Exec(ctx, `INSERT INTO pending_object_deletes (object_key)
		 SELECT unnest($1::text[]) ON CONFLICT (object_key) DO NOTHING`, []string{"files/file_upload", "files/file_owner"}); err != nil {
		t.Fatal(err)
	}
	if got, want := pendingKeys(t, pool), []string{"files/file_gone", "skills/skill_x/1.zip"}; !slices.Equal(got, want) {
		t.Errorf("queue = %v, want %v", got, want)
	}
	// The last row naming it goes: now it is owed.
	inTx(t, pool, func(tx pgx.Tx) error {
		if err := store.AllowFileCopyDeletes(ctx, tx); err != nil {
			return err
		}
		var key string
		if err := tx.QueryRow(ctx, `DELETE FROM files WHERE id = 'file_copy' RETURNING `+store.FileObjectKeySQL).Scan(&key); err != nil {
			return err
		}
		return store.EnqueueObjectDeletes(ctx, tx, []string{key})
	})
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
	insertUpload(t, pool, "file_upload")
	insertCopy(t, pool, "file_copy", "file_upload", key, "sesn_1")
	begin := func() pgx.Tx {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AllowFileCopyDeletes(ctx, tx); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	first, second := begin(), begin()
	defer func() { _ = first.Rollback(ctx) }()
	defer func() { _ = second.Rollback(ctx) }()
	// The second deletes its row first, so the first's check sees that row
	// still there, takes the lock, and skips the key.
	if _, err := second.Exec(ctx, `DELETE FROM files WHERE id = 'file_copy'`); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Exec(ctx, `DELETE FROM files WHERE id = 'file_upload'`); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueObjectDeletes(ctx, first, []string{key}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- store.EnqueueObjectDeletes(ctx, second, []string{key}) }()
	// The second's check sees the first's row, so it waits on the lock the
	// first holds to its commit.
	awaitLockWaiters(t, pool, 1, done)
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

// awaitLockWaiters waits until n backends of pool's database are waiting on a
// lock, failing early if one of done's senders finishes instead.
func awaitLockWaiters(t *testing.T, pool *pgxpool.Pool, n int, done ...chan error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		for _, d := range done {
			select {
			case err := <-d:
				t.Fatalf("finished without waiting on a lock (err %v)", err)
			default:
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d backends waiting on a lock, want %d", waiting, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Looking again after the lock sees the other remover's commit only because a
// READ COMMITTED statement takes a new snapshot. Under REPEATABLE READ or
// SERIALIZABLE both removers would keep seeing each other's row and the
// object would never be owed, so the trigger refuses those levels outright.
func TestObjectDeleteEnqueueRefusesStricterIsolation(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	for _, iso := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
		if err != nil {
			t.Fatal(err)
		}
		err = store.EnqueueObjectDeletes(ctx, tx, []string{"files/file_x"})
		_ = tx.Rollback(ctx)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "25000" {
			t.Errorf("enqueue under %s => %v, want invalid_transaction_state (25000)", iso, err)
		}
	}
	inTx(t, pool, func(tx pgx.Tx) error { return store.EnqueueObjectDeletes(ctx, tx, []string{"files/file_x"}) })
	if got := pendingKeys(t, pool); !slices.Equal(got, []string{"files/file_x"}) {
		t.Errorf("queue = %v under READ COMMITTED, want [files/file_x]", got)
	}
}

// A database can default to a stricter level than the count accepts, which a
// plain Begin inherits. BeginObjectDelete names READ COMMITTED, so a remover
// that begins there enqueues on such a database, and one that does not meets
// the refusal rather than skipping a key.
func TestBeginObjectDeleteIsReadCommittedWhateverTheDefault(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	pgtest.DefaultRepeatableRead(t, pool)
	plain, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = store.EnqueueObjectDeletes(ctx, plain, []string{"files/file_x"})
	_ = plain.Rollback(ctx)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25000" {
		t.Errorf("enqueue in a plain transaction => %v, want invalid_transaction_state (25000)", err)
	}

	tx, err := store.BeginObjectDelete(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var iso string
	if err := tx.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&iso); err != nil || iso != "read committed" {
		t.Fatalf("BeginObjectDelete's transaction is %q (err %v), want read committed", iso, err)
	}
	if err := store.EnqueueObjectDeletes(ctx, tx, []string{"files/file_x"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pendingKeys(t, pool); !slices.Equal(got, []string{"files/file_x"}) {
		t.Errorf("queue = %v, want [files/file_x]", got)
	}
}

// The count's advisory lock is keyed by hashtext, so the keys go in in that
// order: two removers sharing keys then take the locks in one order. Ordered
// as strings instead, two keys whose hashes collide (one lock) would let a
// third key sort between them, and each remover could take one lock the other
// waits for. Here a gate holds b's lock while one remover queues for [b, c]
// and the other for [a, b], with a and c colliding and a < b < c as strings.
func TestEnqueueTakesSharedKeysInLockOrder(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	var pair []string
	if err := pool.QueryRow(ctx,
		`SELECT array_agg(k ORDER BY k) FROM (SELECT 'files/c' || g AS k FROM generate_series(1, 400000) g) s
		  GROUP BY hashtext(k) HAVING count(*) = 2 LIMIT 1`).Scan(&pair); err != nil {
		t.Fatalf("find two keys whose hashtext collides: %v", err)
	}
	a, c := pair[0], pair[1]
	b := a + "!" // between a and c as a string: c is past a, at a digit if a is its prefix
	var distinct bool
	if err := pool.QueryRow(ctx, `SELECT hashtext($1) <> hashtext($2)`, a, b).Scan(&distinct); err != nil || !distinct {
		t.Fatalf("b = %q shares a's lock (err %v); pick another", b, err)
	}
	if !(a < b && b < c) {
		t.Fatalf("keys %q, %q, %q are not in string order", a, b, c)
	}
	// Each key named by a live row, so each enqueue takes its lock.
	for i, k := range []string{a, b, c} {
		insertCopy(t, pool, "file_named"+string(rune('a'+i)), "file_src", k, "sesn_1")
	}

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Rollback(ctx) }()
	if _, err := gate.Exec(ctx, `SELECT pg_advisory_xact_lock(578, hashtext($1))`, b); err != nil {
		t.Fatal(err)
	}
	remove := func(keys ...string) (pgx.Tx, chan error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- store.EnqueueObjectDeletes(ctx, tx, keys) }()
		return tx, done
	}
	second, secondDone := remove(b, c)
	defer func() { _ = second.Rollback(ctx) }()
	awaitLockWaiters(t, pool, 1, secondDone)
	first, firstDone := remove(a, b)
	defer func() { _ = first.Rollback(ctx) }()
	awaitLockWaiters(t, pool, 2, firstDone)
	if err := gate.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// The second was queued first and finishes first; the first then waits
	// for it to commit. Locks taken out of order end one of them in a
	// deadlock (40P01) instead.
	for _, r := range []struct {
		name string
		tx   pgx.Tx
		done chan error
	}{{"[b c]", second, secondDone}, {"[a b]", first, firstDone}} {
		select {
		case err := <-r.done:
			if err != nil {
				t.Fatalf("the remover of %s: %v", r.name, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("the remover of %s never finished", r.name)
		}
		if err := r.tx.Commit(ctx); err != nil {
			t.Fatalf("commit the remover of %s: %v", r.name, err)
		}
	}
}

// GET /v1/files without scope_id lists unscoped rows only (#578), and a
// session's copies and outputs, which are scoped, can outnumber the uploads
// without bound. 0046's partial index answers that list, in both directions
// and past a cursor, without walking them: here three thousand scoped rows are
// newer than every upload, so the full (created_at, id) index would pass all
// of them first. The statements are the shapes internal/api's listFiles sends.
func TestTheUnfilteredFileListReadsTheUnscopedIndex(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	if _, err := pool.Exec(ctx, `
		INSERT INTO files (id, filename, mime_type, size_bytes, scope_type, scope_id, created_at)
		SELECT 'file_s' || g, 'f' || g || '.txt', 'text/plain', 1, 'session', 'sesn_' || (g / 10), now() - make_interval(secs => g)
		  FROM generate_series(1, 3000) g;
		INSERT INTO files (id, filename, mime_type, size_bytes, created_at)
		SELECT 'file_u' || g, 'u.txt', 'text/plain', 1, now() - interval '1 day' - make_interval(secs => g)
		  FROM generate_series(1, 30) g;
		ANALYZE files`); err != nil {
		t.Fatal(err)
	}
	const cols = `SELECT id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id, created_at, expires_at FROM files WHERE true AND scope_id IS NULL`
	for _, q := range []string{
		cols + ` ORDER BY created_at DESC, id DESC LIMIT 21`,
		cols + ` AND (created_at, id) < (now() - interval '1 day', 'file_u5') ORDER BY created_at DESC, id DESC LIMIT 21`,
		cols + ` AND (created_at, id) > (now() - interval '2 days', 'file_u5') ORDER BY created_at ASC, id ASC LIMIT 21`,
	} {
		var plan string
		if err := pool.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+q).Scan(&plan); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan, `"files_unscoped_created_at_id_idx"`) || strings.Contains(plan, `"Sort"`) {
			t.Errorf("%s\nplans as %s; want an ordered scan of files_unscoped_created_at_id_idx", q, plan)
		}
	}
}
