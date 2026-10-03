package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A session create that mounts two of another session's files holds them FOR
// SHARE, and that session's delete takes the same rows FOR UPDATE. Each could
// hold one row the other waits for, and Postgres would cancel one of them
// (40P01, a 500) — unless both lock in one order. These drive the two with
// real concurrent transactions into exactly the interleaving that closes the
// cycle when either side locks out of id order, and require the create to
// meet the delete's outcome (its sources are gone: 404) rather than a
// deadlock. A third transaction holding one row FOR UPDATE is the gate that
// lines the two up behind it before letting them race.
func TestCopyMintsAndBulkDeletesLockInOneOrder(t *testing.T) {
	cases := []struct {
		name string
		// gate is the row held while the delete, then the create, line up.
		gate func(lo, hi string) string
		body func(lo, hi string) map[string]any
	}{{
		// The create names hi before lo. Minting as each resource came up
		// would hold hi while waiting behind the delete for lo, and the
		// delete, holding lo, would wait for hi.
		name: "create names the files out of id order",
		gate: func(lo, _ string) string { return lo },
		body: func(lo, hi string) map[string]any {
			return map[string]any{"resources": []any{
				map[string]any{"type": "file", "file_id": hi},
				map[string]any{"type": "file", "file_id": lo},
			}}
		},
	}, {
		// hi is first in the heap, so a delete locking in scan order would
		// wait for it holding nothing, get it first once the gate opens, and
		// then wait for lo, which the create took while queued behind it.
		name: "delete scans the rows out of id order",
		gate: func(_, hi string) string { return hi },
		body: func(lo, hi string) map[string]any {
			return map[string]any{"resources": []any{
				map[string]any{"type": "file", "file_id": lo},
				map[string]any{"type": "file", "file_id": hi},
			}}
		},
	}, {
		// The rubric is locked after the mounts by the outcome check, so a
		// create holding hi for its mount would wait for lo as its rubric.
		name: "the rubric is named after the mounts",
		gate: func(lo, _ string) string { return lo },
		body: func(lo, hi string) map[string]any {
			return map[string]any{
				"resources": []any{map[string]any{"type": "file", "file_id": hi}},
				"initial_events": []any{defineOutcome("Grade it", map[string]any{
					"rubric": map[string]any{"type": "file", "file_id": lo},
				})},
			}
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			ctx := context.Background()
			agentID, envID := readableFixture(t, s)
			owner := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
			ids := []string{domain.NewID("file").String(), domain.NewID("file").String()}
			slices.Sort(ids)
			lo, hi := ids[0], ids[1]
			// hi first, so the heap holds it ahead of lo.
			for _, id := range []string{hi, lo} {
				if _, err := s.pool.Exec(ctx,
					`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
					 VALUES ($1, $1 || '.md', 'text/markdown', 5, true, 'session', $2)`, id, owner); err != nil {
					t.Fatalf("seed the output %s: %v", id, err)
				}
			}

			gate, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = gate.Rollback(ctx) }()
			if _, err := gate.Exec(ctx, `SELECT 1 FROM files WHERE id = $1 FOR UPDATE`, tc.gate(lo, hi)); err != nil {
				t.Fatal(err)
			}

			type answer struct {
				status int
				body   map[string]any
				err    error
			}
			call := func(method, path string, body any) <-chan answer {
				out := make(chan answer, 1)
				go func() {
					res, err := s.roundTrip(ctx, method, path, body, map[string]string{"x-api-key": testKey})
					if err != nil {
						out <- answer{err: err}
						return
					}
					defer res.Body.Close()
					raw, _ := io.ReadAll(res.Body)
					var obj map[string]any
					_ = json.Unmarshal(raw, &obj)
					out <- answer{status: res.StatusCode, body: obj}
				}()
				return out
			}
			body := tc.body(lo, hi)
			body["agent"], body["environment_id"] = agentID, envID

			deleted := call(http.MethodDelete, "/v1/sessions/"+owner, nil)
			awaitLockWaiters(t, s.pool, 1)
			created := call(http.MethodPost, "/v1/sessions", body)
			awaitLockWaiters(t, s.pool, 2)
			if err := gate.Rollback(ctx); err != nil {
				t.Fatal(err)
			}

			for _, c := range []struct {
				what   string
				ch     <-chan answer
				status int
			}{{"the session delete", deleted, http.StatusOK}, {"the create", created, http.StatusNotFound}} {
				select {
				case a := <-c.ch:
					if a.err != nil {
						t.Fatalf("%s: %v", c.what, a.err)
					}
					if a.status != c.status {
						t.Errorf("%s = %d %v, want %d", c.what, a.status, a.body, c.status)
					}
				case <-time.After(30 * time.Second):
					t.Fatalf("%s never answered", c.what)
				}
			}
		})
	}
}

// The previous build's session delete: the statements that lock or write,
// verbatim from origin/main before #578 (0ae74169) and in its order, its
// reads and its NOTIFY left out. requireNotRunning's lock, the tombstone
// (store.SessionTombstoneInsertSQL, unchanged), the session and checkpoint
// rows, then every session-scoped files row in scan order, and its keys
// enqueued unsorted. During a rolling upgrade it runs beside this build's
// creates.
var prevSessionDeleteSQL = []string{
	`SELECT status FROM sessions WHERE id = $1 FOR UPDATE`,
	store.SessionTombstoneInsertSQL,
	`DELETE FROM sessions WHERE id = $1`,
	`DELETE FROM session_checkpoints WHERE session_id = $1`,
}

const (
	prevSessionFilesDeleteSQL = `DELETE FROM files WHERE scope_type = 'session' AND scope_id = $1 RETURNING id`
	prevObjectEnqueueSQL      = `INSERT INTO pending_object_deletes (object_key)
	 SELECT unnest($1::text[])
	 ON CONFLICT (object_key) DO NOTHING`
)

// prevDeleteSession runs that delete in its own transaction.
func prevDeleteSession(ctx context.Context, pool *pgxpool.Pool, id string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range prevSessionDeleteSQL {
		if _, err := tx.Exec(ctx, q, id); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, prevSessionFilesDeleteSQL, id)
	if err != nil {
		return err
	}
	gone, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	keys := []string{blob.SessionCheckpointKey(id)}
	for _, fid := range gone {
		keys = append(keys, blob.FilesKey(fid))
	}
	if _, err := tx.Exec(ctx, prevObjectEnqueueSQL, keys); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// A previous build's session delete beside this build's create, during a
// rolling upgrade. Its own DELETE locks the session's rows in scan order, and
// 0046's tombstone trigger runs before it; were the trigger to take only the
// copies, the delete would hold them while waiting for an output, and a
// create naming both in id order (lockFileRows) would hold the output while
// waiting for a copy. The trigger takes every session-scoped row in one
// id-ordered statement instead, so the old DELETE that follows finds nothing
// left to lock. Each case is the interleaving that closed the cycle with the
// trigger taking copies alone: a deadlock (40P01) that failed one side, this
// build's POST /v1/sessions answering 500 or the old delete failing.
func TestAPreviousBuildSessionDeleteLocksInTheCreatesOrder(t *testing.T) {
	cases := []struct {
		name string
		// seed writes the session's two rows, lo < hi.
		seed func(t *testing.T, s *tserver, owner, lo, hi string)
	}{{
		// The trigger took the copy hi first, and the old DELETE then wanted
		// the output lo the create held.
		name: "a copy after an output in id order",
		seed: func(t *testing.T, s *tserver, owner, lo, hi string) {
			ctx := context.Background()
			upload := uploadOneFile(t, s, "in.md")
			if _, err := s.pool.Exec(ctx,
				`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
				 VALUES ($1, 'out.md', 'text/markdown', 5, true, 'session', $2)`, lo, owner); err != nil {
				t.Fatalf("seed the output: %v", err)
			}
			if _, err := s.pool.Exec(ctx,
				`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id, object_key, source_file_id)
				 VALUES ($1, 'in.md', 'text/markdown', 5, false, 'session', $2, $3, $4)`,
				hi, owner, blob.FilesKey(upload), upload); err != nil {
				t.Fatalf("seed the copy: %v", err)
			}
		},
	}, {
		// Two outputs, hi first in the heap and in every index the old
		// DELETE could scan: it waited for hi holding nothing, then wanted lo.
		name: "two outputs scanned out of id order",
		seed: func(t *testing.T, s *tserver, owner, lo, hi string) {
			for _, r := range []struct{ id, name string }{{hi, "a.md"}, {lo, "b.md"}} {
				if _, err := s.pool.Exec(context.Background(),
					`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
					 VALUES ($1, $2, 'text/markdown', 5, true, 'session', $3)`, r.id, r.name, owner); err != nil {
					t.Fatalf("seed the output %s: %v", r.id, err)
				}
			}
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			ctx := context.Background()
			agentID, envID := readableFixture(t, s)
			owner := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
			ids := []string{domain.NewID("file").String(), domain.NewID("file").String()}
			slices.Sort(ids)
			lo, hi := ids[0], ids[1]
			tc.seed(t, s, owner, lo, hi)

			// The gate holds hi, which the delete reaches first either way.
			gate, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = gate.Rollback(ctx) }()
			if _, err := gate.Exec(ctx, `SELECT 1 FROM files WHERE id = $1 FOR UPDATE`, hi); err != nil {
				t.Fatal(err)
			}

			deleted := make(chan error, 1)
			go func() { deleted <- prevDeleteSession(ctx, s.pool, owner) }()
			awaitLockWaiters(t, s.pool, 1)
			created := s.sendAsync(http.MethodPost, "/v1/sessions", map[string]any{
				"agent": agentID, "environment_id": envID,
				"resources": []any{
					map[string]any{"type": "file", "file_id": lo},
					map[string]any{"type": "file", "file_id": hi},
				},
			}, map[string]string{"x-api-key": testKey})
			awaitLockWaiters(t, s.pool, 2)
			if err := gate.Rollback(ctx); err != nil {
				t.Fatal(err)
			}

			select {
			case err := <-deleted:
				if err != nil {
					t.Errorf("the previous build's session delete: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the previous build's session delete never finished")
			}
			if r := awaitReply(t, created); r.err != nil || r.code != http.StatusNotFound {
				t.Errorf("the create = %d %s (err %v), want 404: its sources went with the session", r.code, r.body, r.err)
			}
			var left int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE scope_id = $1`, owner).Scan(&left); err != nil {
				t.Fatal(err)
			}
			if left != 0 {
				t.Errorf("%d of the session's files survived its delete", left)
			}
		})
	}
}

// awaitLockWaiters waits until n backends of this test's database are waiting
// on a lock.
func awaitLockWaiters(t *testing.T, pool *pgxpool.Pool, n int) {
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
		if time.Now().After(deadline) {
			t.Fatalf("%d backends waiting on a lock, want %d", waiting, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
