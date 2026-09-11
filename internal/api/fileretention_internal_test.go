package api

import (
	"context"
	"errors"
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
	if _, err := purgeExpiredFiles(ctx, pool, blobs, window); err != nil {
		t.Fatalf("purge over an empty registry: %v", err)
	}
	if _, found := purgedCount(t, reader); found {
		t.Errorf("%s exists after a sweep that removed nothing", MetricExpiredFilesPurged)
	}

	for i := 0; i < 3; i++ {
		seedExpiredFile(t, pool, blobs, window+time.Hour)
	}
	seedExpiredFile(t, pool, blobs, window-time.Hour) // inside the window

	if _, err := purgeExpiredFiles(ctx, pool, blobs, window); err != nil {
		t.Fatalf("purge: %v", err)
	}
	got, found := purgedCount(t, reader)
	if !found || got != 3 {
		t.Errorf("%s = %d (found=%v), want 3", MetricExpiredFilesPurged, got, found)
	}
	// The second sweep finds only the row inside the window; the counter must
	// not move.
	if _, err := purgeExpiredFiles(ctx, pool, blobs, window); err != nil {
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
		n, err := purgeExpiredFiles(ctx, pool, blobs, window)
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

// TestPurgeSurvivesAFailingObjectStore: the row deletion is committed before a
// single object delete is attempted, so a store that refuses every key must not
// turn into rows that survive. It leaves orphans, which is the accepted outcome,
// and the sweep still reports what it removed from the registry.
func TestPurgeSurvivesAFailingObjectStore(t *testing.T) {
	pool := pgtest.NewPool(t)
	failing := &refusingBlobStore{Store: blobtest.Mem()}
	ctx := context.Background()
	window := 30 * 24 * time.Hour
	id := seedExpiredFile(t, pool, failing.Store, window+time.Hour)

	n, err := purgeExpiredFiles(ctx, pool, failing, window)
	if err != nil {
		t.Fatalf("purge against a failing store: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d, want 1: a failing object delete must not change what left the registry", n)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM files WHERE id = $1)`, id).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("the row survived because its object delete failed")
	}
	if got := failing.attempts(); got != 1 {
		t.Errorf("object deletes attempted = %d, want 1: the sweep must ask even when the store refuses", got)
	}
}

// refusingBlobStore refuses every Delete and counts what it was asked for. The
// count is what separates "the sweep tried and the store refused" from "the
// sweep never asked", which look the same from the rows.
type refusingBlobStore struct {
	blob.Store
	asked int
}

func (r *refusingBlobStore) Delete(context.Context, string) error {
	r.asked++
	return errors.New("object store refuses every key")
}

func (r *refusingBlobStore) attempts() int { return r.asked }

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
