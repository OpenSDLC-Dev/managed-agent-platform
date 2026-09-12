package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// seedExpiredFile writes a files row whose expiry is the given age, plus its
// object, the way an upload and the clock together would have.
func seedExpiredFile(t *testing.T, pool *pgxpool.Pool, blobs blob.Store, expiredFor time.Duration) string {
	t.Helper()
	ctx := context.Background()
	id := domain.NewID(domain.PrefixFile).String()
	if _, err := pool.Exec(ctx,
		`INSERT INTO files (id, filename, mime_type, size_bytes, expires_at)
		 VALUES ($1, 'x.bin', 'application/octet-stream', 5, now() - make_interval(secs => $2))`,
		id, expiredFor.Seconds()); err != nil {
		t.Fatalf("seed expired file: %v", err)
	}
	if blobs != nil {
		if err := blobs.Put(ctx, blob.FilesKey(id), strings.NewReader("bytes"), 5, "application/octet-stream"); err != nil {
			t.Fatalf("seed object: %v", err)
		}
	}
	return id
}

// TestFilePurgeIntervalIsTheDocumentedCadence is memoryretention's twin, for
// the same reason: both loop tests override the interval, so the production
// value is pinned by nothing else, and docs/ARCHITECTURE.md publishes it.
func TestFilePurgeIntervalIsTheDocumentedCadence(t *testing.T) {
	if filePurgeInterval != time.Hour {
		t.Errorf("filePurgeInterval = %s, want 1h — the cadence ARCHITECTURE.md publishes", filePurgeInterval)
	}
}

// TestPurgeRecordsWhatItRemoved pins the instrument by its exact exported name:
// a sweep that removed rows counts them, and a sweep that removed none records
// nothing, so a quiet database leaves no series (memoryretention's twin).
func TestPurgeRecordsWhatItRemoved(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	pool := pgtest.NewPool(t)
	blobs := blobtest.Mem()
	ctx := context.Background()
	window := 30 * 24 * time.Hour

	// A sweep over an empty registry must leave no series at all — not a series
	// reading zero.
	if _, err := purgeExpiredFiles(ctx, pool, window); err != nil {
		t.Fatalf("purge over an empty registry: %v", err)
	}
	if _, found := purgedCount(t, reader); found {
		t.Errorf("%s exists after a sweep that removed nothing", MetricExpiredFilesPurged)
	}

	for i := 0; i < 3; i++ {
		seedExpiredFile(t, pool, blobs, window+time.Hour)
	}
	seedExpiredFile(t, pool, blobs, window-time.Hour) // inside the window

	if _, err := purgeExpiredFiles(ctx, pool, window); err != nil {
		t.Fatalf("purge: %v", err)
	}
	got, found := purgedCount(t, reader)
	if !found || got != 3 {
		t.Errorf("%s = %d (found=%v), want 3", MetricExpiredFilesPurged, got, found)
	}
	// The second sweep finds only the row inside the window; the counter must
	// not move.
	if _, err := purgeExpiredFiles(ctx, pool, window); err != nil {
		t.Fatalf("second purge: %v", err)
	}
	if got, _ := purgedCount(t, reader); got != 3 {
		t.Errorf("%s = %d after an empty sweep, want it unmoved at 3", MetricExpiredFilesPurged, got)
	}
}

// TestPurgeTakesTheOldestFirst pins the batch's ORDER BY, which is what makes
// "eventually leaves the registry" true: without it PostgreSQL may take any
// subset, and a row can be passed over every tick while the batch stays full.
// Below the production cap the order is unobservable, so the batch is shrunk.
func TestPurgeTakesTheOldestFirst(t *testing.T) {
	restore := SetFilePurgeBatchForTest(1)
	defer restore()

	pool := pgtest.NewPool(t)
	blobs := blobtest.Mem()
	ctx := context.Background()
	window := 30 * 24 * time.Hour

	// Seeded newest-first, so heap order and expiry order disagree.
	newest := seedExpiredFile(t, pool, blobs, window+time.Hour)
	middle := seedExpiredFile(t, pool, blobs, window+24*time.Hour)
	oldest := seedExpiredFile(t, pool, blobs, window+72*time.Hour)

	for _, want := range []string{oldest, middle, newest} {
		n, err := purgeExpiredFiles(ctx, pool, window)
		if err != nil {
			t.Fatalf("purge: %v", err)
		}
		if n != 1 {
			t.Fatalf("purged %d, want the batch of 1", n)
		}
		var gone bool
		if err := pool.QueryRow(ctx,
			`SELECT NOT EXISTS (SELECT 1 FROM files WHERE id = $1)`, want).Scan(&gone); err != nil {
			t.Fatal(err)
		}
		if !gone {
			t.Fatalf("the batch did not take %s: the oldest expiry must go first", want)
		}
	}
}

// TestPurgeOwesTheObjectsItOrphans: the sweep removes rows and no bytes, and
// what it writes in their place is the debt — one pending_object_deletes row
// per object, which is the only thing still naming those objects once the ids
// are gone. Until plan 50 this sweep deleted the bytes itself, best-effort, and
// a store refusing the batch took every id with it (#698's first item).
func TestPurgeOwesTheObjectsItOrphans(t *testing.T) {
	pool := pgtest.NewPool(t)
	mem := blobtest.Mem()
	ctx := context.Background()
	window := 30 * 24 * time.Hour

	var expired []string
	for i := 0; i < 3; i++ {
		expired = append(expired, seedExpiredFile(t, pool, mem, window+time.Duration(i+1)*time.Hour))
	}
	seedExpiredFile(t, pool, mem, window-time.Hour) // inside the window

	n, err := purgeExpiredFiles(ctx, pool, window)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != len(expired) {
		t.Fatalf("purged %d, want %d", n, len(expired))
	}

	want := make([]string, 0, len(expired))
	for _, id := range expired {
		want = append(want, blob.FilesKey(id))
	}
	slices.Sort(want)
	// Exactly these: a key too few is an object nothing can find again, and the
	// file still inside its window would be a key too many.
	if got := owedKeys(t, pool); !slices.Equal(got, want) {
		t.Errorf("the queue owes %v, want %v", got, want)
	}
	// The bytes are still there. Writing down what is owed is the whole of this
	// sweep's job; the drain is what pays it.
	for _, id := range expired {
		rc, _, err := mem.Get(ctx, blob.FilesKey(id))
		if err != nil {
			t.Errorf("the sweep deleted the object for %s itself: %v", id, err)
			continue
		}
		rc.Close()
	}
}

// TestThePurgeDebtRidesTheDeletingTransaction: the keys are written on the
// transaction, not beside it — deleteSession's rule (plan 50 decision 2),
// reached here by a sweep rather than a request. Which one it is cannot be seen
// once the sweep has answered, and it decides everything before that. On the
// transaction, a sweep that does not commit owes nothing and one that commits
// cannot fail to owe. Beside it, both halves come apart: a rolled-back sweep
// leaves the queue claiming bytes nobody orphaned, and a sweep that died
// between its two statements leaves objects unreferenced and unrecorded.
//
// So the rung brackets the commit from a connection outside the transaction.
// Before it: nothing, because uncommitted rows are invisible there while rows
// written beside the transaction are not. After it: the whole batch.
func TestThePurgeDebtRidesTheDeletingTransaction(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	window := 30 * 24 * time.Hour
	id := seedExpiredFile(t, pool, nil, window+time.Hour)

	owedBefore := -1
	t.Cleanup(SetFilePurgeBeforeCommitHookForTest(func() error {
		owedBefore = len(owedKeys(t, pool))
		return nil
	}))
	if _, err := purgeExpiredFiles(ctx, pool, window); err != nil {
		t.Fatalf("purge: %v", err)
	}
	switch {
	case owedBefore < 0:
		t.Fatal("the sweep committed without reaching the before-commit seam")
	case owedBefore > 0:
		t.Errorf("%d key(s) were visible outside the sweep's transaction before it committed: the enqueue is running beside the transaction", owedBefore)
	}
	if got := owedKeys(t, pool); !slices.Equal(got, []string{blob.FilesKey(id)}) {
		t.Errorf("the queue owes %v the instant the commit returned, want exactly the purged key", got)
	}
}

// TestAPurgeThatDoesNotCommitLosesNothing is the half #696 is about. A sweep
// interrupted between the DELETE and the commit has to leave the rows where
// they were: an id is the only name its object has, so a statement that
// committed without the debt recorded would strand the bytes with nothing able
// to enumerate them — which is what a cancellation landing in the RETURNING
// drain used to do. Rolling back costs an hour's delay and loses nothing.
func TestAPurgeThatDoesNotCommitLosesNothing(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	window := 30 * 24 * time.Hour
	id := seedExpiredFile(t, pool, nil, window+time.Hour)

	interrupted := errors.New("the sweep is interrupted before it commits")
	t.Cleanup(SetFilePurgeBeforeCommitHookForTest(func() error { return interrupted }))
	if _, err := purgeExpiredFiles(ctx, pool, window); !errors.Is(err, interrupted) {
		t.Fatalf("purge = %v, want the interruption to fail it", err)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM files WHERE id = $1)`, id).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Errorf("%s went with a sweep that never committed: its object is now unreferenced and unrecorded", id)
	}
	if got := owedKeys(t, pool); len(got) != 0 {
		t.Errorf("the queue owes %v after a sweep that did not commit", got)
	}
}

// TestTheSweepWakesTheDrain: a committed batch is bytes the drain can free
// immediately, and the drain's own backstop is a minute away, so the sweep asks
// it to look — deleteSession's move for the same debt. Nothing depends on the
// wake for correctness, which is precisely why nothing else here would notice
// it going missing.
func TestTheSweepWakesTheDrain(t *testing.T) {
	pool := pgtest.NewPool(t)
	seedExpiredFile(t, pool, nil, fileMetadataRetention+time.Hour)

	q := NewObjectDeleteQueue()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); StartFileRetention(ctx, pool, q) }()
	t.Cleanup(func() { cancel(); <-done })

	select {
	case <-q.waits():
	case <-time.After(10 * time.Second):
		t.Error("the sweep committed a batch and never woke the drain, which then waits out its own interval for work this replica already knows about")
	}
}

// owedKeys reads what the queue owes, ordered — the durable statement this
// sweep makes instead of deleting bytes.
func owedKeys(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT object_key FROM pending_object_deletes ORDER BY object_key`)
	if err != nil {
		t.Fatalf("read the queue: %v", err)
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

// purgedCount reports the counter's total and whether the instrument exists at
// all. The second half is the one that can see "a sweep that removed nothing
// records nothing": adding zero leaves the same total as not adding.
func purgedCount(t *testing.T, reader *sdkmetric.ManualReader) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			// The literal, deliberately: looking the instrument up by the
			// constant would rename both sides together and pin nothing.
			if m.Name != "files.expired.purged" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", m.Name, m.Data)
			}
			var total int64
			for _, p := range sum.DataPoints {
				total += p.Value
			}
			return total, true
		}
	}
	return 0, false
}
