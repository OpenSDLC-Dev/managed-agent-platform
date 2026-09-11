package api_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
)

// expireBy moves a file's expiry that far into the past, so a test can put a
// row on either side of the retention window without waiting 30 days.
func expireBy(t *testing.T, s *tserver, id string, ago time.Duration) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE files SET expires_at = now() - make_interval(secs => $2) WHERE id = $1`,
		id, ago.Seconds()); err != nil {
		t.Fatalf("expire %s: %v", id, err)
	}
}

func fileRowExists(t *testing.T, s *tserver, id string) bool {
	t.Helper()
	var exists bool
	if err := s.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM files WHERE id = $1)`, id).Scan(&exists); err != nil {
		t.Fatalf("read files row %s: %v", id, err)
	}
	return exists
}

func blobExists(t *testing.T, s *tserver, id string) bool {
	t.Helper()
	rc, _, err := s.blobs.Get(context.Background(), blob.FilesKey(id))
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return false
		}
		t.Fatalf("read object for %s: %v", id, err)
	}
	_ = rc.Close()
	return true
}

// TestExpiredFilePurge pins the second half of the published lifecycle (#655,
// plan 49 slice 2): metadata "remains readable for up to 30 days, with
// expires_at in the past", and then does not. The window is measured from
// expires_at, so a file one second short of it survives a sweep that removes
// the one just past it — row and object together.
func TestExpiredFilePurge(t *testing.T) {
	s := newTestServer(t)
	oct := "application/octet-stream"
	ctx := context.Background()
	window := 30 * 24 * time.Hour

	// Three files, one on each side of the window and one that never expires.
	past := s.uploadFile(t, "past.bin", &oct, "long gone")["id"].(string)
	inside := s.uploadFile(t, "inside.bin", &oct, "still readable")["id"].(string)
	never := s.uploadFile(t, "never.bin", &oct, "no lifetime")["id"].(string)
	expireBy(t, s, past, window+time.Minute)
	expireBy(t, s, inside, window-time.Minute)

	n, err := api.PurgeExpiredFilesForTest(ctx, s.pool, s.blobs, window)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d, want exactly the one past the window", n)
	}

	if fileRowExists(t, s, past) {
		t.Error("a file past the grace window kept its row")
	}
	if blobExists(t, s, past) {
		t.Error("a file past the grace window kept its object")
	}
	// Inside the window the metadata route still answers — the documented
	// behavior the sweep must not shorten.
	if status, body := s.do("GET", "/v1/files/"+inside, nil); status != http.StatusOK {
		t.Errorf("metadata inside the window = %d, want 200: %v", status, body)
	}
	if !blobExists(t, s, inside) {
		t.Error("a file inside the window lost its object")
	}
	// A file with no expiry is not a candidate at all, whatever its age.
	if !fileRowExists(t, s, never) {
		t.Error("a file with no expiry was purged")
	}
	if !blobExists(t, s, never) {
		t.Error("a file with no expiry lost its object")
	}
	// The route agrees with the row: past the window the file is simply gone.
	if status, body := s.do("GET", "/v1/files/"+past, nil); status != http.StatusNotFound {
		t.Errorf("metadata after the purge = %d, want 404: %v", status, body)
	}

	// A second sweep finds nothing left to do.
	if n, err := api.PurgeExpiredFilesForTest(ctx, s.pool, s.blobs, window); err != nil || n != 0 {
		t.Errorf("second sweep = %d, %v; want 0, nil", n, err)
	}
}

// TestExpiredFilePurgeWithoutBlobStore: a deployment that has lost its object
// store still removes the rows. The objects are beyond this process's reach,
// which is the operator's doing, and leaving the metadata to outlive the
// published window instead would be the worse answer.
func TestExpiredFilePurgeWithoutBlobStore(t *testing.T) {
	s := newTestServer(t)
	oct := "application/octet-stream"
	window := 30 * 24 * time.Hour

	id := s.uploadFile(t, "orphan.bin", &oct, "bytes")["id"].(string)
	expireBy(t, s, id, window+time.Minute)

	n, err := api.PurgeExpiredFilesForTest(context.Background(), s.pool, nil, window)
	if err != nil {
		t.Fatalf("purge with no blob store: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d, want 1", n)
	}
	if fileRowExists(t, s, id) {
		t.Error("the row survived a purge that had no object store")
	}
	if !blobExists(t, s, id) {
		t.Error("the object was removed by a sweep with no object store, which cannot happen")
	}
}

// TestFileRetentionSweepRuns drives the loop itself rather than its statement:
// the ticker fires, the sweep runs, and cancelling the context ends it. It also
// pins the production window's value — 30 days is the number the reference
// publishes, and the statement test passes its own window in, so without these
// two files the constant could be any duration at all and nothing would fail.
func TestFileRetentionSweepRuns(t *testing.T) {
	s := newTestServer(t)
	oct := "application/octet-stream"
	restore := api.SetFilePurgeIntervalForTest(20 * time.Millisecond)
	defer restore()

	// One file either side of the real window, since the loop uses the
	// production constant rather than a parameter.
	swept := s.uploadFile(t, "swept.bin", &oct, "bytes")["id"].(string)
	kept := s.uploadFile(t, "kept.bin", &oct, "bytes")["id"].(string)
	expireBy(t, s, swept, 31*24*time.Hour)
	expireBy(t, s, kept, 29*24*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); api.StartFileRetention(ctx, s.pool, s.blobs) }()

	deadline := time.Now().Add(10 * time.Second)
	for fileRowExists(t, s, swept) {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the sweep did not remove a file 31 days past its expiry")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if blobExists(t, s, swept) {
		t.Error("the sweep removed the row but left the object")
	}
	// The 29-day file was a candidate of the same statement that removed the
	// 31-day one, so its survival is decided rather than merely pending.
	if !fileRowExists(t, s, kept) {
		t.Error("a file 29 days past its expiry was purged: the window is not the published 30 days")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartFileRetention did not return after its context was cancelled")
	}
}
