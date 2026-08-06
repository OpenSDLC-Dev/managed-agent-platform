package executor

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
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

// deleteSessionRow removes the harness session the way the API does: the
// tombstone (carrying the environment kind) lands before the row goes, in one
// transaction — via store.SessionTombstoneInsertSQL, the same statement the
// API's deleteSession runs, so these tests cannot drift from the shape
// production writes.
func deleteSessionRow(t *testing.T, h *harness) {
	t.Helper()
	deleteSessionRowByID(t, h, h.sid)
}

func deleteSessionRowByID(t *testing.T, h *harness, sid domain.ID) {
	t.Helper()
	ctx := context.Background()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, store.SessionTombstoneInsertSQL, sid.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sid.String()); err != nil {
		t.Fatal(err)
	}
	// The marker row dies in the deleting transaction too (the API's shape;
	// the reaper can never own it — a session whose sandbox was already
	// idle-reaped never reappears in Owned).
	if _, err := tx.Exec(ctx, `DELETE FROM session_checkpoints WHERE session_id = $1`, sid.String()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
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

// TestReapPassDeletedSessionReapsAndDeletesCheckpoint: a tombstoned session is
// the deleted tier — its holding is reaped and its checkpoint blob removed,
// blob first so a failed delete keeps the Owned trigger for the next pass.
func TestReapPassDeletedSessionReapsAndDeletesCheckpoint(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	ctx := context.Background()
	putCheckpoint(t, h)
	deleteSessionRow(t, h)
	h.prov.owned = []domain.ID{h.sid}

	if err := h.exec.reapPass(ctx); err != nil {
		t.Fatalf("reap pass: %v", err)
	}
	if !slices.Contains(h.prov.reapedSnapshot(), h.sid) {
		t.Error("deleted session's sandbox not reaped")
	}
	if hasCheckpoint(t, h) {
		t.Error("deleted session's checkpoint blob survived")
	}
}

// TestReapPassSkipsForeignSandbox: a holding whose session id has neither a
// sessions row nor a tombstone was never this deployment's to destroy — a
// second deployment sharing the Docker daemon or K8s namespace, or a contract
// suite's fixtures on a shared dev daemon, all label sandboxes with ids this
// database has never seen. The deleted tier runs on the tombstone's
// affirmative evidence, not on the row's absence.
func TestReapPassSkipsForeignSandbox(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	foreign := domain.NewID(domain.PrefixSession)
	h.prov.owned = []domain.ID{foreign}

	if err := h.exec.reapPass(context.Background()); err != nil {
		t.Fatalf("reap pass: %v", err)
	}
	if len(h.prov.reapedSnapshot()) != 0 {
		t.Fatal("a sandbox this database has never seen was reaped")
	}
}

// TestReapPassContinuesPastAFailingSession: per-session failures are joined,
// not fatal to the pass — one wedged session must not shield the rest of the
// endpoint from teardown.
func TestReapPassContinuesPastAFailingSession(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	archiveSession(t, h)
	wedged := pgtest.NewSessionInEnv(t, h.pool, h.envID)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET status = 'terminated' WHERE id = $1`, wedged.String()); err != nil {
		t.Fatal(err)
	}
	h.prov.owned = []domain.ID{wedged, h.sid}
	h.prov.reapFailFor = wedged

	err := h.exec.reapPass(context.Background())
	if err == nil {
		t.Fatal("reap pass = nil error with one session's reap failing")
	}
	if !slices.Contains(h.prov.reapedSnapshot(), h.sid) {
		t.Fatal("the failing session shielded the healthy one from teardown")
	}
}

// TestReapPassLeavesSelfHostedSessions: a self_hosted session's sandbox
// belongs to the customer's BYOC worker, which shares the ownership label but
// never takes the advisory lock — on a shared daemon the platform reaper must
// leave it alone in every tier, terminal or not (the plan's cloud-only rule).
func TestReapPassLeavesSelfHostedSessions(t *testing.T) {
	h := newHarness(t, &fakeSandbox{})
	ctx := context.Background()
	sid, _ := pgtest.NewSession(t, h.pool, "self_hosted")

	// Archived tier: terminal, but not the platform's asset.
	if _, err := h.pool.Exec(ctx,
		`UPDATE sessions SET archived_at = now() WHERE id = $1`, sid.String()); err != nil {
		t.Fatal(err)
	}
	h.prov.owned = []domain.ID{sid}
	if err := h.exec.reapPass(ctx); err != nil {
		t.Fatalf("reap pass (archived): %v", err)
	}
	if len(h.prov.reapedSnapshot()) != 0 {
		t.Fatal("an archived self_hosted session's sandbox was reaped")
	}

	// Deleted tier: the tombstone records the environment kind, and a
	// self_hosted tombstone still does not make the sandbox the platform's.
	deleteSessionRowByID(t, h, sid)
	if err := h.exec.reapPass(ctx); err != nil {
		t.Fatalf("reap pass (deleted): %v", err)
	}
	if len(h.prov.reapedSnapshot()) != 0 {
		t.Fatal("a deleted self_hosted session's sandbox was reaped")
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
	if !slices.Contains(h.prov.reapedSnapshot(), h.sid) {
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
	if !slices.Contains(h.prov.reapedSnapshot(), h.sid) {
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
		if len(h.prov.reapedSnapshot()) != 0 {
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
	if len(h.prov.reapedSnapshot()) != 0 {
		t.Fatal("reaped while the session's provision lock was held")
	}

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, sessionLockKey(h.sid)); err != nil {
		t.Fatal(err)
	}
	if err := h.exec.reapPass(ctx); err != nil {
		t.Fatalf("reap pass after release: %v", err)
	}
	if !slices.Contains(h.prov.reapedSnapshot(), h.sid) {
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
	if len(h.prov.reapedSnapshot()) != 0 {
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
	deleteSessionRow(t, h)
	h.prov.owned = []domain.ID{h.sid}
	h.exec.blobs = failingDeleteStore{Store: h.blobs}

	if err := h.exec.reapPass(ctx); err == nil {
		t.Fatal("reap pass = nil error with the checkpoint delete failing")
	}
	if len(h.prov.reapedSnapshot()) != 0 {
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
	// lock answers immediately and fails the pending check below. Filtered to
	// this database and THIS session's key (pg_locks splits the 64-bit key
	// into classid/objid), so an unrelated waiter elsewhere in the cluster
	// cannot satisfy the barrier.
	key := uint64(sessionLockKey(h.sid))
	waitSQL := `SELECT EXISTS (SELECT 1 FROM pg_locks
	  WHERE locktype = 'advisory' AND NOT granted
	    AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
	    AND classid = $1::oid AND objid = $2::oid)`
	for deadline := time.Now().Add(10 * time.Second); ; {
		select {
		case err := <-done:
			t.Fatalf("provision returned (%v) without waiting on the session lock", err)
		default:
		}
		var waiting bool
		if err := h.pool.QueryRow(ctx, waitSQL, uint32(key>>32), uint32(key)).Scan(&waiting); err != nil {
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

// TestRunWaitsForTheReaperToStop: Run's return means the reaper is no longer
// running — a caller that closes the pool after Run must not race a reap pass
// still in flight. The hook holds a pass open past the cancellation; Run may
// return only once the pass finishes.
func TestRunWaitsForTheReaperToStop(t *testing.T) {
	h := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{ReapInterval: 20 * time.Millisecond})
	h.prov = h.exec.provider.(*fakeProvider)
	archiveSession(t, h)
	h.prov.owned = []domain.ID{h.sid}

	classified := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	releaseReaper := func() { releaseOnce.Do(func() { close(release) }) }
	// A t.Fatal below must still unblock the held pass, or the reaper
	// goroutine hangs and the package timeout buries this test's failure.
	defer releaseReaper()
	reapHookAfterClassify = func(domain.ID) {
		once.Do(func() { close(classified) })
		<-release
	}
	defer func() { reapHookAfterClassify = nil }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { _ = h.exec.Run(ctx); close(runDone) }()

	<-classified // a reap pass is now held open inside reapSession
	cancel()
	select {
	case <-runDone:
		t.Fatal("Run returned while a reap pass was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	releaseReaper()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the reap pass finished")
	}
}
