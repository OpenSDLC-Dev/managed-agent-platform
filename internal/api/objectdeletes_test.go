package api_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
)

// refusingStore is an object store that can be told to refuse every delete and
// later told to stop — the outage this whole plan exists for, and the one a
// store that simply works can never exercise. Every attempt is recorded, so a
// test can tell "tried and was refused" from "never looked", and the embedded
// MemStore stays reachable so a test can seed and verify without going through
// the refusal.
type refusingStore struct {
	*blobtest.MemStore
	mu        sync.Mutex
	refuse    bool
	attempted []string
}

func newRefusingStore() *refusingStore {
	return &refusingStore{MemStore: blobtest.Mem()}
}

func (s *refusingStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	s.attempted = append(s.attempted, key)
	refuse := s.refuse
	s.mu.Unlock()
	if refuse {
		return errors.New("object store is refusing deletes")
	}
	return s.MemStore.Delete(ctx, key)
}

func (s *refusingStore) setRefusing(refuse bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse = refuse
}

func (s *refusingStore) attempts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.attempted)
}

// newTestServerWithStore is newTestServer with the object store chosen by the
// caller, for the rungs that need one which can fail. tserver.blobs is the
// embedded MemStore rather than the wrapper, so seeding and verification are
// never subject to the refusal under test.
func newTestServerWithStore(t *testing.T, store *refusingStore) *tserver {
	t.Helper()
	cipher, err := local.New(local.Config{KeyID: "test-1", Key: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	pool := newPoolWithKey(t)
	srv := httptest.NewServer(api.NewHandler(pool, store, cipher, nil, api.WithDreamRunner()))
	t.Cleanup(srv.Close)
	return &tserver{t: t, url: srv.URL, pool: pool, blobs: store.MemStore}
}

// seedDeliverable writes the row internal/executor's harvest writes and the
// object beside it, which is what a session delete is then on the hook for.
func seedDeliverable(t *testing.T, s *tserver, sid, name string) string {
	t.Helper()
	ctx := context.Background()
	id := domain.NewID("file").String()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
		 VALUES ($1, $2, 'text/markdown', 5, true, 'session', $3)`, id, name, sid); err != nil {
		t.Fatalf("seed the deliverable row: %v", err)
	}
	if err := s.blobs.Put(ctx, blob.FilesKey(id), strings.NewReader("bytes"), 5, "text/markdown"); err != nil {
		t.Fatalf("seed the deliverable object: %v", err)
	}
	return id
}

// pendingKeys reads what the queue still owes — the durable statement this plan
// turns on: a key here is an object the deployment has promised to remove and
// has not removed yet.
func pendingKeys(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT object_key FROM pending_object_deletes ORDER BY object_key`)
	if err != nil {
		t.Fatalf("read the pending queue: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out = append(out, k)
	}
	return out
}

// startSweeper runs the drain beside the test and returns a stop function. The
// wake is returned too, because a rung that waits out the one-minute interval
// is a rung nobody will run.
func startSweeper(t *testing.T, s *tserver, store blob.Store) *api.ObjectDeleteQueue {
	t.Helper()
	q := api.NewObjectDeleteQueue()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); api.StartPendingObjectDeletes(ctx, s.pool, store, q) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the sweeper did not return after its context was cancelled")
		}
	})
	return q
}

// awaitDrained waits for the queue to empty, which is what "the bytes are gone"
// looks like from outside: a row outlives its object and nothing else.
func awaitDrained(t *testing.T, pool *pgxpool.Pool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if len(pendingKeys(t, pool)) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the queue still owes %v", what, pendingKeys(t, pool))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitAttempt waits until the store has been asked to delete key — the
// observable that separates "the sweeper tried and was refused" from "the
// sweeper never looked", which is the difference the outage rung turns on.
func awaitAttempt(t *testing.T, store *refusingStore, key, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if slices.Contains(store.attempts(), key) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %q was never attempted", what, key)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSessionDeleteEnqueuesItsObjectsRatherThanDeletingThem: the request path
// removes no bytes (plan 49 decision 3). What it leaves is a row per key,
// committed with the rows that stopped referring to the objects — which is what
// makes a failed delete retryable instead of forgotten, and what a delete that
// went on removing bytes itself would not leave.
func TestSessionDeleteEnqueuesItsObjectsRatherThanDeletingThem(t *testing.T) {
	store := newRefusingStore()
	s := newTestServerWithStore(t, store)
	agentID, envID := fixture(t, s)
	sid := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID})["id"].(string)
	first := seedDeliverable(t, s, sid, "one.md")
	second := seedDeliverable(t, s, sid, "two.md")

	if status, res := s.do(http.MethodDelete, "/v1/sessions/"+sid, nil); status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, res)
	}

	if got := store.attempts(); len(got) != 0 {
		t.Fatalf("the request path deleted %v; plan 49 leaves every object to the sweeper", got)
	}
	// The checkpoint key rides with the deliverables (#320): its old remover was
	// the reaper's deleted tier, which never revisits a session whose sandbox
	// the idle tier already destroyed.
	want := []string{blob.FilesKey(first), blob.FilesKey(second), blob.SessionCheckpointKey(sid)}
	got := pendingKeys(t, s.pool)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the delete enqueued %v, want the two deliverables and the checkpoint %v", got, want)
	}
}

// TestTheEnqueueRidesTheDeletingTransaction: the keys are written on the
// transaction, not beside it (plan 49 decision 2). Which one it is cannot be
// seen once the delete has answered — both leave the same rows — and it decides
// everything before that. On the transaction, the queue learns what is owed
// exactly when the rows that referred to the objects stop existing: a delete
// that rolls back owes nothing, and a delete that commits cannot fail to owe.
// Beside it, on the pool, both halves come apart — a rolled-back delete leaves
// rows claiming bytes nobody orphaned, and a process that dies between the
// commit and the insert leaves objects that are unreferenced and unrecorded,
// which is the crash window #645 is about and the one nothing can reopen later.
//
// So the assertion has to be made while the transaction is still open, from a
// connection that is not in it: uncommitted rows are invisible there and rows
// written beside the transaction are not. That window is the seam's whole
// reason to exist.
func TestTheEnqueueRidesTheDeletingTransaction(t *testing.T) {
	store := newRefusingStore()
	s := newTestServerWithStore(t, store)
	agentID, envID := fixture(t, s)
	sid := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID})["id"].(string)
	seedDeliverable(t, s, sid, "one.md")

	// Buffered and read after the response, so the count crosses goroutines on
	// the channel rather than on a shared variable, and a hook that never fires
	// leaves it empty rather than leaving a stale zero that would pass.
	seen := make(chan int, 1)
	t.Cleanup(api.SetDeleteSessionBeforeCommitHookForTest(func() {
		var n int
		// Another connection from the same pool: a read on the handler's own
		// would be inside the transaction and would see the rows either way.
		if err := s.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pending_object_deletes`).Scan(&n); err != nil {
			n = -1
		}
		seen <- n
	}))

	if status, res := s.do(http.MethodDelete, "/v1/sessions/"+sid, nil); status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, res)
	}

	var during int
	select {
	case during = <-seen:
	default:
		t.Fatal("the delete answered without reaching the seam: it never committed, so the window under test did not happen")
	}
	switch {
	case during < 0:
		t.Fatal("could not read the queue from outside the transaction")
	case during > 0:
		t.Fatalf("%d key(s) were already visible outside the delete's transaction before it committed; the enqueue is running beside the transaction rather than on it, which reopens the crash window between the commit and the insert", during)
	}
	// And the commit is what publishes them, so the rung cannot pass by the
	// enqueue simply never happening.
	if got := pendingKeys(t, s.pool); len(got) == 0 {
		t.Fatal("the committed delete owes nothing: the seam fired but no key was ever enqueued")
	}
}

// TestASweepRemovesWhatADeleteCouldNot is the issue itself. A store refusing
// every delete used to cost the bytes permanently, because the request path was
// the only remover and nothing revisited what it failed. The queue now holds
// the debt across the outage and the sweeper pays it on recovery.
func TestASweepRemovesWhatADeleteCouldNot(t *testing.T) {
	// A refused key waits out its backoff and then comes due, and what notices
	// is the sweeper's interval — a wake reports an ending, and nothing ends
	// when a store recovers. Both are shortened so the rung runs in test time;
	// the production pacing of each is not what this one is about, and the
	// deferral has its own rung below.
	t.Cleanup(api.SetObjectDeleteBackoffForTest(20 * time.Millisecond))
	t.Cleanup(api.SetObjectDeleteIntervalForTest(20 * time.Millisecond))
	store := newRefusingStore()
	store.setRefusing(true)
	s := newTestServerWithStore(t, store)
	agentID, envID := fixture(t, s)
	sid := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID})["id"].(string)
	key := blob.FilesKey(seedDeliverable(t, s, sid, "report.md"))

	if status, res := s.do(http.MethodDelete, "/v1/sessions/"+sid, nil); status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, res)
	}
	q := startSweeper(t, s, store)

	// The outage: the sweeper tries and is refused, so the row stays owed and
	// the object stays put.
	q.Wake()
	awaitAttempt(t, store, key, "the sweep during the outage")
	if _, _, err := s.blobs.Get(context.Background(), key); err != nil {
		t.Fatalf("the object went missing during an outage that refused every delete: %v", err)
	}
	if !slices.Contains(pendingKeys(t, s.pool), key) {
		t.Fatal("a refused delete dropped its row; the object would now be orphaned with no record of it")
	}

	// And the recovery, reached by the sweeper's own retry rather than by a
	// second wake: nothing ends when a store comes back, so if the interval did
	// not revisit a key whose backoff had expired, the bytes would sit there
	// until the next unrelated delete happened to wake this replica.
	store.setRefusing(false)
	awaitDrained(t, s.pool, "the sweeper's retry after the store recovered")
	if _, _, err := s.blobs.Get(context.Background(), key); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("the object survived the sweep: %v", err)
	}
}

// TestAFailedObjectDeleteIsDeferredNotDropped: the row is the only record that
// an object is still owed, so a refusal must leave it — with the attempt
// counted, the cause on it, and the next try in the future rather than
// immediately, which is what keeps a permanently refused key from spinning
// (plan 49 decision 6).
func TestAFailedObjectDeleteIsDeferredNotDropped(t *testing.T) {
	store := newRefusingStore()
	store.setRefusing(true)
	s := newTestServerWithStore(t, store)
	agentID, envID := fixture(t, s)
	sid := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID})["id"].(string)
	key := blob.FilesKey(seedDeliverable(t, s, sid, "held.md"))

	if status, res := s.do(http.MethodDelete, "/v1/sessions/"+sid, nil); status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, res)
	}
	startSweeper(t, s, store).Wake()
	awaitAttempt(t, store, key, "the refused sweep")

	deadline := time.Now().Add(10 * time.Second)
	for {
		var attempts int
		var lastErr *string
		var due time.Time
		if err := s.pool.QueryRow(context.Background(),
			`SELECT attempts, last_error, next_attempt_at FROM pending_object_deletes WHERE object_key = $1`,
			key).Scan(&attempts, &lastErr, &due); err != nil {
			t.Fatalf("the refused key lost its row: %v", err)
		}
		if attempts > 0 {
			if lastErr == nil || *lastErr == "" {
				t.Fatal("the attempt was counted but its cause was not recorded; the row cannot answer what went wrong")
			}
			if !due.After(time.Now()) {
				t.Fatalf("the next attempt is due at %s, which is not in the future: a refused key would spin", due)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the refusal was never recorded on the row")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTheSweeperDrainsWithoutAWake: the wake is an optimization and nothing may
// depend on it — a key another replica enqueued raises no wake in this process
// at all. Asserted with no queue, which is also every deployment that wires
// none.
func TestTheSweeperDrainsWithoutAWake(t *testing.T) {
	store := newRefusingStore()
	s := newTestServerWithStore(t, store)
	ctx := context.Background()
	key := "files/file_from-another-replica"
	if err := s.blobs.Put(ctx, key, strings.NewReader("x"), 1, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO pending_object_deletes (object_key) VALUES ($1)`, key); err != nil {
		t.Fatal(err)
	}

	// The interval is the thing under test, so it is shortened rather than
	// waited out — with the production minute this rung would be a minute long
	// and nobody would run the suite.
	t.Cleanup(api.SetObjectDeleteIntervalForTest(20 * time.Millisecond))
	sweepCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); api.StartPendingObjectDeletes(sweepCtx, s.pool, store, nil) }()
	defer func() { cancel(); <-done }()

	awaitDrained(t, s.pool, "the sweeper's own interval, with no wake")
	if _, _, err := s.blobs.Get(ctx, key); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("the object survived: %v", err)
	}
}

// TestEnqueuedKeysSurviveUntilASweeperExists: the enqueue rides the deleting
// transaction, so a process that dies before any sweep leaves the work behind
// rather than losing it. Modelled by starting no sweeper until after the
// delete, which is what a restart looks like to the table.
func TestEnqueuedKeysSurviveUntilASweeperExists(t *testing.T) {
	store := newRefusingStore()
	s := newTestServerWithStore(t, store)
	agentID, envID := fixture(t, s)
	sid := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID})["id"].(string)
	key := blob.FilesKey(seedDeliverable(t, s, sid, "survivor.md"))

	if status, res := s.do(http.MethodDelete, "/v1/sessions/"+sid, nil); status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, res)
	}
	if !slices.Contains(pendingKeys(t, s.pool), key) {
		t.Fatal("the delete left nothing for a later process to find")
	}

	startSweeper(t, s, store).Wake()
	awaitDrained(t, s.pool, "a sweeper that started after the delete")
	if _, _, err := s.blobs.Get(context.Background(), key); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("the object survived: %v", err)
	}
}
