package executor

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// putCheckpoint stores a placeholder checkpoint blob for the harness session.
func putCheckpoint(t *testing.T, h *harness) {
	t.Helper()
	data := []byte("tar-bytes")
	if err := h.blobs.Put(context.Background(), blob.SessionCheckpointKey(h.sid.String()),
		bytes.NewReader(data), int64(len(data)), "application/gzip"); err != nil {
		t.Fatalf("put checkpoint: %v", err)
	}
}

func hasCheckpoint(t *testing.T, h *harness) bool {
	t.Helper()
	rc, _, err := h.blobs.Get(context.Background(), blob.SessionCheckpointKey(h.sid.String()))
	if errors.Is(err, blob.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("get checkpoint: %v", err)
	}
	rc.Close()
	return true
}

func setStatus(t *testing.T, h *harness, status string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET status = $2 WHERE id = $1`, h.sid.String(), status); err != nil {
		t.Fatal(err)
	}
}

func archiveSession(t *testing.T, h *harness) {
	t.Helper()
	setStatus(t, h, "idle")
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET archived_at = now() WHERE id = $1`, h.sid.String()); err != nil {
		t.Fatal(err)
	}
}

// TestReapPassDeletedSessionReapsAndDeletesCheckpoint: a session whose row is
// gone is the deleted tier — its holding is reaped and its checkpoint blob
// removed, blob first so a failed delete keeps the Owned trigger for the next
// pass.
func TestReapPassDeletedSessionReapsAndDeletesCheckpoint(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	ctx := context.Background()
	putCheckpoint(t, h)
	if _, err := h.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, h.sid.String()); err != nil {
		t.Fatal(err)
	}
	h.prov.owned = []domain.ID{h.sid}

	if err := h.exec.reapPass(ctx); err != nil {
		t.Fatalf("reap pass: %v", err)
	}
	if !slices.Contains(h.prov.reaped, h.sid) {
		t.Error("deleted session's sandbox not reaped")
	}
	if hasCheckpoint(t, h) {
		t.Error("deleted session's checkpoint blob survived")
	}
}

// TestReapPassArchivedReapsKeepsCheckpoint: archived is terminal for the
// sandbox but not for the record — the checkpoint stays until the row goes.
func TestReapPassArchivedReapsKeepsCheckpoint(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	ctx := context.Background()
	putCheckpoint(t, h)
	archiveSession(t, h)
	h.prov.owned = []domain.ID{h.sid}

	if err := h.exec.reapPass(ctx); err != nil {
		t.Fatalf("reap pass: %v", err)
	}
	if !slices.Contains(h.prov.reaped, h.sid) {
		t.Error("archived session's sandbox not reaped")
	}
	if !hasCheckpoint(t, h) {
		t.Error("archived session's checkpoint blob deleted; it must outlive the sandbox")
	}
}

// TestReapPassTerminatedReaps: terminated is unrecoverable — the sandbox goes.
func TestReapPassTerminatedReaps(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	setStatus(t, h, "terminated")
	h.prov.owned = []domain.ID{h.sid}
	if err := h.exec.reapPass(context.Background()); err != nil {
		t.Fatalf("reap pass: %v", err)
	}
	if !slices.Contains(h.prov.reaped, h.sid) {
		t.Error("terminated session's sandbox not reaped")
	}
}

// TestReapPassLeavesLiveSessions: idle, running and rescheduling sessions keep
// their sandboxes — the idle-TTL tier is slice 5's, and running is the hole
// slice 1's guards plus this lock exist to keep closed.
func TestReapPassLeavesLiveSessions(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	h.prov.owned = []domain.ID{h.sid}
	for _, status := range []string{"idle", "running", "rescheduling"} {
		setStatus(t, h, status)
		if err := h.exec.reapPass(context.Background()); err != nil {
			t.Fatalf("reap pass (%s): %v", status, err)
		}
		if len(h.prov.reaped) != 0 {
			t.Errorf("a %s session's sandbox was reaped", status)
		}
	}
}

// TestReapSkipsWhenLockHeld: the per-session advisory lock is taken with
// try-lock — a session mid-provision is skipped this pass, not waited on, and
// the next pass picks it up once the lock is free.
func TestReapSkipsWhenLockHeld(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	ctx := context.Background()
	archiveSession(t, h)
	h.prov.owned = []domain.ID{h.sid}

	conn, err := h.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, sessionLockKey(h.sid)); err != nil {
		t.Fatal(err)
	}

	if err := h.exec.reapPass(ctx); err != nil {
		t.Fatalf("reap pass under a held lock: %v", err)
	}
	if len(h.prov.reaped) != 0 {
		t.Fatal("reaped while the session's provision lock was held")
	}

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, sessionLockKey(h.sid)); err != nil {
		t.Fatal(err)
	}
	if err := h.exec.reapPass(ctx); err != nil {
		t.Fatalf("reap pass after release: %v", err)
	}
	if !slices.Contains(h.prov.reaped, h.sid) {
		t.Error("not reaped after the lock was released")
	}
}

// TestReapRechecksUnderTheLock: the pre-lock classification is a candidate
// filter only (plan 24 D4) — a session revived between classify and lock must
// be re-read under the lock and left alone.
func TestReapRechecksUnderTheLock(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	ctx := context.Background()
	archiveSession(t, h)
	h.prov.owned = []domain.ID{h.sid}

	reapHookAfterClassify = func(sid domain.ID) {
		if _, err := h.pool.Exec(ctx,
			`UPDATE sessions SET archived_at = NULL, status = 'running' WHERE id = $1`, sid.String()); err != nil {
			t.Errorf("revive session: %v", err)
		}
	}
	defer func() { reapHookAfterClassify = nil }()

	if err := h.exec.reapPass(ctx); err != nil {
		t.Fatalf("reap pass: %v", err)
	}
	if len(h.prov.reaped) != 0 {
		t.Fatal("reaped on the stale pre-lock classification — the criteria were not re-read under the lock")
	}
}

// TestReapDeletedTierBlobFailureKeepsTheSandbox: the deleted tier deletes the
// checkpoint before it reaps — a failed blob delete aborts with the sandbox
// still owned, so the next pass retries the whole tier instead of orphaning
// the blob forever (a reaped sandbox leaves Owned, and with it every retry).
func TestReapDeletedTierBlobFailureKeepsTheSandbox(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, h.sid.String()); err != nil {
		t.Fatal(err)
	}
	h.prov.owned = []domain.ID{h.sid}
	h.exec.blobs = failingDeleteStore{Store: h.blobs}

	if err := h.exec.reapPass(ctx); err == nil {
		t.Fatal("reap pass = nil error with the checkpoint delete failing")
	}
	if len(h.prov.reaped) != 0 {
		t.Fatal("sandbox reaped before its checkpoint blob was deleted — the retry trigger is gone")
	}
}

// failingDeleteStore fails every Delete; everything else delegates.
type failingDeleteStore struct{ blob.Store }

func (f failingDeleteStore) Delete(context.Context, string) error {
	return errors.New("storage down")
}

// TestProvisionWaitsForTheSessionLock: provisionSandbox serializes against the
// reaper on the same advisory lock — blocking, not try: work must proceed the
// moment the reap finishes.
func TestProvisionWaitsForTheSessionLock(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	ctx := context.Background()

	conn, err := h.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, sessionLockKey(h.sid)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.exec.provisionSandbox(ctx, h.sid, sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}})
		done <- err
	}()

	// Deterministic barrier: the provision must be observably waiting on the
	// advisory lock before the release — a provision that does not take the
	// lock answers immediately and fails the pending check below.
	waitSQL := `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND NOT granted)`
	for deadline := time.Now().Add(10 * time.Second); ; {
		select {
		case err := <-done:
			t.Fatalf("provision returned (%v) without waiting on the session lock", err)
		default:
		}
		var waiting bool
		if err := h.pool.QueryRow(ctx, waitSQL).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provision never blocked on the session advisory lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, sessionLockKey(h.sid)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("provision after the lock released: %v", err)
	}
}

// TestRunDrivesTheReaper: Run owns the reap loop — an archived session's
// sandbox is reaped without any test calling reapPass, within a few intervals.
func TestRunDrivesTheReaper(t *testing.T) {
	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{ReapInterval: 20 * time.Millisecond})
	h.prov = h.exec.provider.(*fakeProvider)
	archiveSession(t, h)
	h.prov.owned = []domain.ID{h.sid}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { _ = h.exec.Run(ctx); close(runDone) }()

	deadline := time.Now().Add(5 * time.Second)
	for len(h.prov.reapedSnapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run never drove a reap pass")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-runDone
}
