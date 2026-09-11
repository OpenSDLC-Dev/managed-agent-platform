package api_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
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
	hang      bool
	attempted []string
}

func newRefusingStore() *refusingStore {
	return &refusingStore{MemStore: blobtest.Mem()}
}

func (s *refusingStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	s.attempted = append(s.attempted, key)
	refuse, hang := s.refuse, s.hang
	s.mu.Unlock()
	// A store that answers nothing at all, which is not the same failure as one
	// that refuses: an error comes back and can be recorded, while a hang comes
	// back only when something upstream decides it has waited long enough.
	if hang {
		<-ctx.Done()
		return ctx.Err()
	}
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

func (s *refusingStore) setHanging(hang bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hang = hang
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

// TestDrainSweepsBeforeItsFirstTick: the queue holds precisely what a process
// died before deleting, so a backlog at boot is the normal case — and the wake
// that would have announced it died with that process. Nothing here wakes the
// sweeper and the interval is production's minute, longer than the wait below,
// so only a pass taken before the first wait can empty the queue. Without it a
// replica restarting more often than the interval never drains at all.
func TestDrainSweepsBeforeItsFirstTick(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	key := blob.FilesKey(domain.NewID("file").String())
	if err := s.blobs.Put(ctx, key, strings.NewReader("bytes"), 5, "text/markdown"); err != nil {
		t.Fatalf("seed the object: %v", err)
	}
	// The row a dead process's transaction left behind.
	if _, err := s.pool.Exec(ctx, store.PendingObjectDeleteInsertSQL, []string{key}); err != nil {
		t.Fatalf("seed the owed key: %v", err)
	}

	startSweeper(t, s, s.blobs)
	awaitDrained(t, s.pool, "the backlog a restart inherits")

	if _, _, err := s.blobs.Get(ctx, key); err == nil {
		t.Errorf("%s: the row went but the object did not", key)
	}
}

// TestSessionDeleteEnqueuesItsObjectsRatherThanDeletingThem: the request path
// removes no bytes (plan 50 decision 3). What it leaves is a row per key,
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
		t.Fatalf("the request path deleted %v; plan 50 leaves every object to the sweeper", got)
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
// transaction, not beside it (plan 50 decision 2). Which one it is cannot be
// seen once the delete has answered — both leave the same rows — and it decides
// everything before that. On the transaction, the queue learns what is owed
// exactly when the rows that referred to the objects stop existing: a delete
// that rolls back owes nothing, and a delete that commits cannot fail to owe.
// Beside it, on the pool, both halves come apart — a rolled-back delete leaves
// rows claiming bytes nobody orphaned, and a process that dies between the
// commit and the insert leaves objects that are unreferenced and unrecorded,
// which is the crash window #645 is about and the one nothing can reopen later.
//
// So the rung brackets the commit from both sides, from a connection that is
// not in the transaction. Before it: nothing, because uncommitted rows are
// invisible there while rows written beside the transaction are not. The
// instant after it: everything, because the commit is what publishes them —
// which is what an enqueue moved to where the old object cleanup ran, after the
// commit, would fail. One assertion alone would let the other shape through.
func TestTheEnqueueRidesTheDeletingTransaction(t *testing.T) {
	store := newRefusingStore()
	s := newTestServerWithStore(t, store)
	agentID, envID := fixture(t, s)
	sid := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID})["id"].(string)
	seedDeliverable(t, s, sid, "one.md")

	// Buffered and read after the response, so each count crosses goroutines on
	// its channel rather than on a shared variable, and a hook that never fired
	// leaves one empty rather than leaving a stale zero that would pass.
	before, after := make(chan int, 1), make(chan int, 1)
	count := func() int {
		var n int
		// Another connection from the same pool: a read on the handler's own
		// would be inside the transaction and would see the rows either way.
		if err := s.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pending_object_deletes`).Scan(&n); err != nil {
			return -1
		}
		return n
	}
	t.Cleanup(api.SetDeleteSessionBeforeCommitHookForTest(func() error {
		before <- count()
		return nil
	}))
	t.Cleanup(api.SetDeleteSessionAfterCommitHookForTest(func() { after <- count() }))

	if status, res := s.do(http.MethodDelete, "/v1/sessions/"+sid, nil); status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, res)
	}

	read := func(c chan int, which string) int {
		t.Helper()
		select {
		case n := <-c:
			if n < 0 {
				t.Fatalf("could not read the queue %s the commit", which)
			}
			return n
		default:
			t.Fatalf("the delete answered without reaching the %s-commit seam", which)
			return 0
		}
	}
	if n := read(before, "before"); n > 0 {
		t.Fatalf("%d key(s) were visible outside the delete's transaction before it committed: the enqueue is running beside the transaction, which leaves rows behind a delete that rolls back", n)
	}
	// The deliverable and the checkpoint, both there the instant the commit
	// returned. An enqueue that had moved after the commit would show nothing
	// here and the right rows later, and the rung above alone would pass it.
	if n := read(after, "after"); n != 2 {
		t.Fatalf("%d key(s) were owed the instant the commit returned, want 2: the enqueue did not commit with the rows it is owed to", n)
	}
}

// TestADeleteThatRollsBackOwesNothing is decision 2's other half, and the one
// an enqueue beside the transaction gets wrong in the opposite direction: rows
// claiming bytes that nobody orphaned, for a session that is still there. It
// needs the delete to fail after the enqueue and before the commit, which is
// the window the seam already exists for.
func TestADeleteThatRollsBackOwesNothing(t *testing.T) {
	store := newRefusingStore()
	s := newTestServerWithStore(t, store)
	agentID, envID := fixture(t, s)
	sid := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID})["id"].(string)
	fileID := seedDeliverable(t, s, sid, "one.md")

	t.Cleanup(api.SetDeleteSessionBeforeCommitHookForTest(func() error {
		return errors.New("the delete fails after enqueuing and before committing")
	}))
	if status, _ := s.do(http.MethodDelete, "/v1/sessions/"+sid, nil); status == http.StatusOK {
		t.Fatal("the delete answered OK though it never committed")
	}

	if got := pendingKeys(t, s.pool); len(got) != 0 {
		t.Fatalf("a delete that rolled back left %v owed: those objects are still referenced, and a sweeper will delete them", got)
	}
	// And the rollback really did take the rows back with it, so the emptiness
	// above is the transaction undoing the enqueue rather than the enqueue
	// never having run.
	var rows int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM files WHERE id = $1`, fileID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatal("the rolled-back delete removed the deliverable's row anyway")
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
	// deferral has its own rung below. The cap is shortened with the base
	// because the sweeper keeps retrying through the outage and every refusal
	// doubles the next wait: uncapped, a loaded machine that spends a few
	// seconds between the refusal and the recovery finds the key deferred past
	// the recovery, and the rung fails on its own pacing.
	t.Cleanup(api.SetObjectDeleteBackoffForTest(20*time.Millisecond, 50*time.Millisecond))
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
// (plan 50 decision 6).
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

// TestADeleteOnABloblessReplicaStillRecordsWhatItOrphaned: the enqueue does not
// ask whether this process has an object store, because that is a fact about
// the replica and not about the objects. A control plane that had a store and
// comes back without one — an env var dropped in a deploy, a configuration
// drifted from the executor's — would otherwise delete the rows that name those
// objects and write nothing down, and restoring the configuration afterwards
// could not recover keys nobody recorded: the permanent orphaning this plan
// exists to end, reintroduced by a guard against it.
//
// The other direction is what the guard was for, and it costs nothing. A
// deployment that never had a store has no deliverables to enqueue, so what
// accumulates is one row per deleted session for a checkpoint that was never
// written — and the first sweeper to run deletes a missing key, which every
// backend answers nil, and drains them.
func TestADeleteOnABloblessReplicaStillRecordsWhatItOrphaned(t *testing.T) {
	cipher, err := local.New(local.Config{KeyID: "test-1", Key: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	pool := newPoolWithKey(t)
	// No object store at all, which cmd/controlplane supports and logs: the
	// storage-backed routes report the absence and everything else serves.
	srv := httptest.NewServer(api.NewHandler(pool, nil, cipher, nil, api.WithDreamRunner()))
	t.Cleanup(srv.Close)
	s := &tserver{t: t, url: srv.URL, pool: pool}

	agentID, envID := fixture(t, s)
	sid := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID})["id"].(string)
	// The row without its object, which is exactly the state a replica that has
	// lost its store sees: the bytes were written by a deployment that had one.
	fileID := domain.NewID("file").String()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
		 VALUES ($1, 'report.md', 'text/markdown', 5, true, 'session', $2)`, fileID, sid); err != nil {
		t.Fatalf("seed a harvested file: %v", err)
	}

	if status, res := s.do(http.MethodDelete, "/v1/sessions/"+sid, nil); status != http.StatusOK {
		t.Fatalf("delete on a blob-less replica: %d %v", status, res)
	}

	want := []string{blob.FilesKey(fileID), blob.SessionCheckpointKey(sid)}
	slices.Sort(want)
	if got := pendingKeys(t, pool); !slices.Equal(got, want) {
		t.Fatalf("a blob-less replica recorded %v, want %v: the objects it just stopped referring to are orphaned with nothing naming them", got, want)
	}
}

// TestABackoffOutlastsALongOutage: a key refused for long enough must still be
// deferred. The backoff multiplies an interval by `power(2, attempts)`, which
// leaves interval range once attempts passes 38 — and the cap cannot save a
// product that errored on its way to being computed, so the whole UPDATE fails.
// What that costs is the reverse of what the cap is for: the attempt goes
// uncounted and the cause unrecorded, so the column an operator reads to answer
// "what has this deployment failed to delete" freezes, and the row keeps only
// its claim, coming back far sooner than the cap says rather than later. Seven
// doublings reach the cap and every attempt after is an hour apart, so a day
// and a half of one refused key is all it takes to get there.
func TestABackoffOutlastsALongOutage(t *testing.T) {
	t.Cleanup(api.SetObjectDeleteIntervalForTest(20 * time.Millisecond))
	store := newRefusingStore()
	store.setRefusing(true)
	s := newTestServerWithStore(t, store)
	ctx := context.Background()

	// Seeded at the attempt count rather than driven to it: the real path takes
	// a day and a half of wall clock to arrive, and the row is the whole of
	// what this queue is either way.
	const key = "files/file_refusedforadayandahalf"
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO pending_object_deletes (object_key, attempts) VALUES ($1, 39)`, key); err != nil {
		t.Fatalf("seed a long-refused key: %v", err)
	}

	startSweeper(t, s, store).Wake()
	awaitAttempt(t, store, key, "the sweep of a long-refused key")

	deadline := time.Now().Add(10 * time.Second)
	for {
		var attempts int
		var capped bool
		if err := s.pool.QueryRow(ctx,
			`SELECT attempts, next_attempt_at > now() + interval '50 minutes'
			   FROM pending_object_deletes WHERE object_key = $1`, key).Scan(&attempts, &capped); err != nil {
			t.Fatalf("read the long-refused key: %v", err)
		}
		if attempts > 39 {
			if !capped {
				t.Fatal("the refusal was counted but the next attempt was not pushed out to the cap")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("attempts is still %d after a refused sweep: the UPDATE that records a failure is itself failing, so the key keeps neither its count nor its backoff", attempts)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// poisonStore refuses every delete with an error a text column cannot hold, in
// all three shapes at once: a NUL byte, an invalid UTF-8 sequence, and enough
// length that the bound has to cut — with a multi-byte rune straddling the cut
// so the cut itself can make the value invalid.
type poisonStore struct {
	*blobtest.MemStore
	mu        sync.Mutex
	attempted []string
}

func (s *poisonStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	s.attempted = append(s.attempted, key)
	s.mu.Unlock()
	// The padding is computed rather than guessed, because the two removals
	// shorten the head and the rune has to straddle the cut *after* them: a "€"
	// that ends up on either side of byte 500 would let a byte-wise cut pass,
	// and the rung would agree with the code instead of checking it.
	head := "storage said: \x00\xff "
	survives := strings.ReplaceAll(strings.ToValidUTF8(head, ""), "\x00", "")
	return errors.New(head + strings.Repeat("x", 498-len(survives)) + "€" +
		strings.Repeat("y", 600))
}

func (s *poisonStore) asked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.attempted)
}

// TestARefusalIsRecordedWhateverTheStoreSaid: the cause lands on the row even
// when the store's message is bytes Postgres will not store. Each of the three
// shapes fails the same way and the failure is the expensive one — the whole
// deferral UPDATE rolls back, so the attempt goes uncounted and the key keeps
// only its claim, which is the state a permanently refused key must never reach
// and the one an operator reading last_error cannot diagnose, the column having
// stopped being written.
//
// NUL is the one that survives a validity check: U+0000 is valid UTF-8, so
// making the string valid is not enough on its own.
func TestARefusalIsRecordedWhateverTheStoreSaid(t *testing.T) {
	t.Cleanup(api.SetObjectDeleteIntervalForTest(20 * time.Millisecond))
	store := &poisonStore{MemStore: blobtest.Mem()}
	cipher, err := local.New(local.Config{KeyID: "test-1", Key: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	pool := newPoolWithKey(t)
	srv := httptest.NewServer(api.NewHandler(pool, store, cipher, nil, api.WithDreamRunner()))
	t.Cleanup(srv.Close)
	s := &tserver{t: t, url: srv.URL, pool: pool, blobs: store.MemStore}
	ctx := context.Background()

	const key = "files/file_poisonedcause"
	if _, err := pool.Exec(ctx,
		`INSERT INTO pending_object_deletes (object_key) VALUES ($1)`, key); err != nil {
		t.Fatalf("seed the key: %v", err)
	}
	startSweeper(t, s, store).Wake()

	deadline := time.Now().Add(10 * time.Second)
	for {
		var attempts int
		var cause *string
		if err := pool.QueryRow(ctx,
			`SELECT attempts, last_error FROM pending_object_deletes WHERE object_key = $1`,
			key).Scan(&attempts, &cause); err != nil {
			t.Fatalf("read the refused key: %v", err)
		}
		if attempts > 0 {
			if cause == nil || *cause == "" {
				t.Fatal("the refusal was counted but its cause was dropped")
			}
			if strings.ContainsRune(*cause, 0) {
				t.Fatal("a NUL reached the column, which Postgres cannot hold")
			}
			return
		}
		if time.Now().After(deadline) && store.asked() > 0 {
			t.Fatal("the store was asked and refused, but no attempt was recorded: the UPDATE carrying the cause is failing on the cause itself, so the key keeps neither its count nor its backoff")
		}
		if time.Now().After(deadline) {
			t.Fatal("the store was never asked")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAHangingStoreDoesNotStopTheSweeper: every store call is bounded, so a
// delete that never returns is a failed attempt rather than the end of this
// replica's cleanup. Nothing below the call supplies that bound — a blackholed
// endpoint accepts the connection and then answers nothing, which no HTTP
// client here times out on — and the keys are worked one at a time, so an
// unbounded call would hold the sweeper for the life of the process while the
// queue behind it grew. On a deployment with one control plane that is the
// whole of the cleanup, stopped.
func TestAHangingStoreDoesNotStopTheSweeper(t *testing.T) {
	t.Cleanup(api.SetObjectDeleteCallBudgetForTest(200 * time.Millisecond))
	t.Cleanup(api.SetObjectDeleteBackoffForTest(20*time.Millisecond, 50*time.Millisecond))
	t.Cleanup(api.SetObjectDeleteIntervalForTest(20 * time.Millisecond))
	store := newRefusingStore()
	store.setHanging(true)
	s := newTestServerWithStore(t, store)
	ctx := context.Background()

	first, second := "files/file_hangs01", "files/file_hangs02"
	for _, key := range []string{first, second} {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO pending_object_deletes (object_key) VALUES ($1)`, key); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	startSweeper(t, s, store).Wake()

	// The second key is the assertion. A sweeper stuck on the first would never
	// reach it, and the queue would sit there for as long as the store hung.
	awaitAttempt(t, store, second, "the key behind the one that hung")

	// And the hang was recorded as what it is, so an operator asking what this
	// deployment cannot delete gets an answer rather than silence.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var attempts int
		var cause *string
		if err := s.pool.QueryRow(ctx,
			`SELECT attempts, last_error FROM pending_object_deletes WHERE object_key = $1`,
			first).Scan(&attempts, &cause); err != nil {
			t.Fatalf("read the hung key: %v", err)
		}
		if attempts > 0 {
			if cause == nil || *cause == "" {
				t.Fatal("the hung delete was counted but left no cause on the row")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the hung delete was never recorded as an attempt: it is not being treated as a failure, so the key carries no backoff and no cause")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTwoSweepersDoNotDuplicateTheStoreRoundTrips is plan 50's acceptance item
// 6, and it pins the half of that sentence a test can reach. Two replicas drain
// one table; every key must reach the store exactly once, which is what the
// claim buys — it pushes next_attempt_at past the pass that took it, and the
// row goes as soon as its object does, so the other replica's claim cannot
// return the same key.
//
// It does not pin "neither blocks the other". Without SKIP LOCKED the second
// claim would wait for the first to commit and then find the rows no longer
// due, so it would still not duplicate: what changes is latency under
// contention, and nothing here would notice. Decision 5 says correctness does
// not rest on the claim at all, a double delete being nil for every backend —
// what rests on it is the waste, and the waste is what this counts.
func TestTwoSweepersDoNotDuplicateTheStoreRoundTrips(t *testing.T) {
	t.Cleanup(api.SetObjectDeleteIntervalForTest(20 * time.Millisecond))
	store := newRefusingStore()
	s := newTestServerWithStore(t, store)
	ctx := context.Background()

	const keys = 60
	want := make([]string, 0, keys)
	for i := range keys {
		key := fmt.Sprintf("files/file_shared%02d", i)
		want = append(want, key)
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO pending_object_deletes (object_key) VALUES ($1)`, key); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
		if err := s.blobs.Put(ctx, key, strings.NewReader("bytes"), 5, "text/plain"); err != nil {
			t.Fatalf("seed the object for %s: %v", key, err)
		}
	}

	// Two loops over one pool, the deployment shape this is about.
	startSweeper(t, s, store).Wake()
	startSweeper(t, s, store).Wake()
	awaitDrained(t, s.pool, "two sweepers draining one table")

	got := store.attempts()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the store was asked %d times for %d keys; a key asked twice is the duplicated round trip the claim exists to avoid\n got: %v\nwant: %v",
			len(got), len(want), got, want)
	}
	for _, key := range want {
		if _, _, err := s.blobs.Get(ctx, key); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("%s survived the sweep: %v", key, err)
		}
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
