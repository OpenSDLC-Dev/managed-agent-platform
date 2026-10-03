package store_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The previous build's statements, verbatim from origin/main before #578
// (0ae74169). During a rolling upgrade they run against this schema, and each
// must leave a session's copies alone.
const (
	// internal/executor's settleHarvest, replacing a session's snapshot.
	prevHarvestDelete = `DELETE FROM files WHERE scope_type = 'session' AND scope_id = $1 RETURNING id`
	prevHarvestInsert = `INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
				 VALUES ($1, $2, $3, $4, true, 'session', $5)`
	// internal/api's deleteFile, which answers 404 when it deleted no row.
	prevFileDelete = `DELETE FROM files WHERE id = $1`
	// internal/api's purgeExpiredFiles.
	prevPurge = `
		DELETE FROM files
		 WHERE id IN (SELECT f.id FROM files f
		               WHERE f.expires_at < now() - make_interval(secs => $1)
		                 AND (f.dream_id IS NULL
		                      OR NOT EXISTS (SELECT 1 FROM dreams d
		                                      WHERE d.id = f.dream_id AND d.closed_at IS NULL))
		               ORDER BY f.expires_at, f.id
		               LIMIT $2
		               FOR UPDATE SKIP LOCKED)
		 RETURNING id`
	// internal/api's deleteSession, in its order; the tombstone is
	// store.SessionTombstoneInsertSQL, unchanged.
	prevSessionLock          = `SELECT status FROM sessions WHERE id = $1 FOR UPDATE`
	prevSessionDelete        = `DELETE FROM sessions WHERE id = $1`
	prevCheckpointDelete     = `DELETE FROM session_checkpoints WHERE session_id = $1`
	prevSessionFilesDelete   = `DELETE FROM files WHERE scope_type = 'session' AND scope_id = $1 RETURNING id`
	prevPendingObjectEnqueue = `INSERT INTO pending_object_deletes (object_key)
	 SELECT unnest($1::text[])
	 ON CONFLICT (object_key) DO NOTHING`
)

// fileIDs lists the files rows, sorted.
func fileIDs(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id FROM files ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// A previous-build executor's harvest deletes every session-scoped row before
// writing its snapshot. Migration 0046's guard skips the copies, so they and
// the session's resources[] survive it, the RETURNING leaves them out (their
// keys are not enqueued), and the snapshot's own rows are replaced as before
// — under a name a copy may share, the uniqueness leaving copies out.
func TestAPreviousBuildHarvestLeavesTheCopies(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	insertUpload(t, pool, "file_upload")
	insertCopy(t, pool, "file_copy", "file_upload", blob.FilesKey("file_upload"), "sesn_1")
	if _, err := pool.Exec(ctx, prevHarvestInsert, "file_out1", "a.txt", "text/plain", 1, "sesn_1"); err != nil {
		t.Fatalf("the first snapshot: %v", err)
	}
	inTx(t, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, prevHarvestDelete, "sesn_1")
		if err != nil {
			return err
		}
		gone, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if !slices.Equal(gone, []string{"file_out1"}) {
			t.Errorf("the harvest's delete returned %v, want only its own output", gone)
		}
		if _, err := tx.Exec(ctx, prevHarvestInsert, "file_out2", "a.txt", "text/plain", 1, "sesn_1"); err != nil {
			return err
		}
		keys := make([]string, len(gone))
		for i, id := range gone {
			keys[i] = blob.FilesKey(id)
		}
		_, err = tx.Exec(ctx, prevPendingObjectEnqueue, keys)
		return err
	})
	if got, want := fileIDs(t, pool), []string{"file_copy", "file_out2", "file_upload"}; !slices.Equal(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
	if got := pendingKeys(t, pool); !slices.Equal(got, []string{"files/file_out1"}) {
		t.Errorf("queue = %v, want only the replaced output's key", got)
	}
}

// The previous build's other removers that can reach a copy skip it too: its
// DELETE /v1/files/{copy} deletes nothing (that build answers 404), and its
// expiry sweep takes the expired upload but not the expired copy, which this
// build's sweep removes later. A transaction that allows copy deletes takes
// it, and the permission ends with that transaction rather than staying on
// the pooled connection.
func TestOnlyATransactionThatAllowsItDeletesACopy(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	insertUpload(t, pool, "file_upload")
	insertCopy(t, pool, "file_copy", "file_upload", blob.FilesKey("file_upload"), "sesn_1")

	tag, err := pool.Exec(ctx, prevFileDelete, "file_copy")
	if err != nil || tag.RowsAffected() != 0 {
		t.Errorf("the previous build's delete of a copy: %v rows, err %v; want 0", tag.RowsAffected(), err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE files SET expires_at = now() - interval '31 days' WHERE id IN ('file_upload', 'file_copy')`); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, prevPurge, float64(30*24*3600), 1000)
	if err != nil {
		t.Fatal(err)
	}
	purged, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(purged, []string{"file_upload"}) {
		t.Errorf("the previous build's sweep took %v, want the upload alone", purged)
	}

	// One pooled connection, so the second transaction runs where the first
	// set the permission.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AllowFileCopyDeletes(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if tag, err := conn.Exec(ctx, prevFileDelete, "file_copy"); err != nil || tag.RowsAffected() != 0 {
		t.Errorf("a delete after the allowing transaction committed: %v rows, err %v; want 0", tag.RowsAffected(), err)
	}
	tx, err = conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.AllowFileCopyDeletes(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if tag, err := tx.Exec(ctx, prevFileDelete, "file_copy"); err != nil || tag.RowsAffected() != 1 {
		t.Errorf("a delete in an allowing transaction: %v rows, err %v; want 1", tag.RowsAffected(), err)
	}
}

// A previous-build session delete removes only the session's outputs — the
// guard keeps its DELETE off the copies — but it writes the tombstone first,
// and 0046's trigger on it takes the copies and owes their objects through
// the reference count: kept while another row names one, owed once none does.
// Nothing is left behind for a sweep to find.
func TestAPreviousBuildSessionDeleteTakesItsCopies(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	seedSessionChain(t, pool)
	insertUpload(t, pool, "file_kept")
	insertCopy(t, pool, "file_copykept", "file_kept", blob.FilesKey("file_kept"), "sesn_1")
	// file_gone's upload was deleted already: this copy is its object's last name.
	insertCopy(t, pool, "file_copylast", "file_gone", blob.FilesKey("file_gone"), "sesn_1")
	if _, err := pool.Exec(ctx, prevHarvestInsert, "file_output", "out.txt", "text/plain", 1, "sesn_1"); err != nil {
		t.Fatal(err)
	}
	// Another session's copy of the same upload stays.
	insertCopy(t, pool, "file_elsewhere", "file_kept", blob.FilesKey("file_kept"), "sesn_other")

	inTx(t, pool, func(tx pgx.Tx) error {
		for _, q := range []string{prevSessionLock, store.SessionTombstoneInsertSQL, prevSessionDelete, prevCheckpointDelete} {
			if _, err := tx.Exec(ctx, q, "sesn_1"); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, prevSessionFilesDelete, "sesn_1")
		if err != nil {
			return err
		}
		gone, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		keys := []string{blob.SessionCheckpointKey("sesn_1")}
		for _, id := range gone {
			keys = append(keys, blob.FilesKey(id))
		}
		_, err = tx.Exec(ctx, prevPendingObjectEnqueue, keys)
		return err
	})
	if got, want := fileIDs(t, pool), []string{"file_elsewhere", "file_kept"}; !slices.Equal(got, want) {
		t.Errorf("rows after the delete = %v, want %v", got, want)
	}
	want := []string{"files/file_gone", "files/file_output", blob.SessionCheckpointKey("sesn_1")}
	slices.Sort(want)
	if got := pendingKeys(t, pool); !slices.Equal(got, want) {
		t.Errorf("queue = %v, want %v", got, want)
	}
}

// This build's session delete has allowed copy deletes before it writes the
// tombstone, and removes the copies with its outputs in one statement before
// it enqueues anything, so the trigger leaves them to it.
func TestATombstoneLeavesCopiesToATransactionThatDeletesThem(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	seedSessionChain(t, pool)
	insertCopy(t, pool, "file_copy", "file_upload", blob.FilesKey("file_upload"), "sesn_1")
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.AllowFileCopyDeletes(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, store.SessionTombstoneInsertSQL, "sesn_1"); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM files WHERE id = 'file_copy'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Error("the tombstone's trigger took the copy from a transaction that deletes it itself")
	}
}

// Every DELETE FROM files in production code has decided whether it deletes
// session copies (#578): those that do call store.AllowFileCopyDeletes in
// their transaction, and those that do not are listed as such. A new one fails
// here until it is added, because the guard's silent skip is right for the
// previous build and wrong for a path this build forgot. Each allowing path
// has a behavioural test that the copy goes: internal/api's
// TestArchivingASessionLeavesItsCopies (DELETE /v1/files/{copy}),
// TestSessionDeleteTakesItsCopies and TestACopyExpiresWithItsUpload, and
// TestAPreviousBuildSessionDeleteTakesItsCopies above for the trigger.
func TestEveryFilesDeleteDecidesAboutCopies(t *testing.T) {
	type decision struct{ deletes, allows int }
	want := map[string]decision{
		"internal/api/files.go":         {1, 1}, // deleteFile: the id may be a copy
		"internal/api/sessions.go":      {1, 1}, // deleteSession: its copies go with it
		"internal/api/fileretention.go": {1, 1}, // purgeExpiredFiles: copies expire with their upload
		"internal/api/dreamrunner.go":   {1, 0}, // a dream's transcripts; a copy carries no dream_id
		"internal/executor/harvest.go":  {1, 0}, // the outputs snapshot, never a copy
		// The trigger on deleted_sessions, for a previous build's session delete.
		"internal/store/migrations/0046_files_object_key.sql": {1, 1},
	}
	deleteRE := regexp.MustCompile(`(?i)\bdelete\s+from\s+files\b`)
	allowSQL := regexp.MustCompile(`set_config\('map\.copy_delete',\s*'on'`)
	root := filepath.Join("..", "..")
	got := map[string]decision{}
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			switch {
			case strings.HasSuffix(path, ".sql"):
				src, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				var code strings.Builder
				for _, line := range strings.Split(string(src), "\n") {
					code.WriteString(strings.SplitN(line, "--", 2)[0] + "\n")
				}
				if n := len(deleteRE.FindAllString(code.String(), -1)); n > 0 {
					got[rel] = decision{n, len(allowSQL.FindAllString(code.String(), -1))}
				}
			case strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go"):
				f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
				if err != nil {
					return err
				}
				var dec decision
				ast.Inspect(f, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.BasicLit:
						if n.Kind == token.STRING {
							s, err := strconv.Unquote(n.Value)
							if err != nil {
								s = n.Value
							}
							dec.deletes += len(deleteRE.FindAllString(s, -1))
						}
					case *ast.SelectorExpr:
						if n.Sel.Name == "AllowFileCopyDeletes" {
							dec.allows++
						}
					}
					return true
				})
				if dec.deletes > 0 {
					got[rel] = dec
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if !maps.Equal(got, want) {
		t.Errorf("DELETE FROM files sites (file: {deletes, AllowFileCopyDeletes calls}) =\n%v\nwant\n%v\n"+
			"A new or moved delete decides whether it deletes session copies: if it does, its transaction calls "+
			"store.AllowFileCopyDeletes and a test shows the copy go; if not, say why beside it here.", got, want)
	}
}
