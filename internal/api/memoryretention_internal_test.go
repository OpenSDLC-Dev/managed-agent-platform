package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// Memory-version retention (#476). White-box because the sweep has no wire
// surface: it is driven by a ticker, and its two exemptions — a live memory's
// head, and a memory's newest rows whatever their age — are properties of the
// statement rather than of any response.

const retStore = "memstore_01ARZ3NDEKTSV4RRFFQ69G5FRE"

// seedVersions plants n versions for one memory, oldest first, each `age`
// apart ending at `newest`, and returns their ids oldest-first. The memories
// row is not written here: a caller decides whether the memory is live.
func seedVersions(t *testing.T, pool *pgxpool.Pool, memoryID string, n int, newest time.Time, age time.Duration) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, n)
	for i := range n {
		id := domain.NewID(domain.PrefixMemoryVersion).String()
		at := newest.Add(-time.Duration(n-1-i) * age)
		op := "modified"
		if i == 0 {
			op = "created"
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO memory_versions (id, memory_store_id, memory_id, operation, path, content, content_sha256, content_size_bytes, created_at)
			 VALUES ($1, $2, $3, $4, '/notes.md', $5, 'sha', 1, $6)`,
			id, retStore, memoryID, op, fmt.Sprintf("v%d", i), at); err != nil {
			t.Fatalf("seed version %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// liveMemory writes the memories row that makes headVersion a live head.
func liveMemory(t *testing.T, pool *pgxpool.Pool, memoryID, headVersion, path string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO memories (id, memory_store_id, path, content, content_sha256, content_size_bytes, memory_version_id)
		 VALUES ($1, $2, $3, 'x', 'sha', 1, $4)`, memoryID, retStore, path, headVersion); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
}

func retentionPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.NewPool(t)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO memory_stores (id, name) VALUES ($1, 'Retention')`, retStore); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	return pool
}

// surviving lists a memory's remaining versions oldest-first.
func surviving(t *testing.T, pool *pgxpool.Pool, memoryID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id FROM memory_versions WHERE memory_store_id = $1 AND memory_id = $2
		  ORDER BY created_at, id`, retStore, memoryID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// TestPruneKeepsTheNewestWhateverTheirAge: a memory rewritten nine times long
// ago keeps its newest five and loses the rest — the reference's rule, with
// the count this platform chose.
func TestPruneKeepsTheNewestWhateverTheirAge(t *testing.T) {
	pool := retentionPool(t)
	memoryID := domain.NewID(domain.PrefixMemory).String()
	old := time.Now().Add(-90 * 24 * time.Hour)
	ids := seedVersions(t, pool, memoryID, 9, old, time.Hour)
	liveMemory(t, pool, memoryID, ids[8], "/notes.md")

	n, err := pruneMemoryVersions(context.Background(), pool, memoryVersionRetention, memoryVersionsKept)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 4 {
		t.Errorf("pruned %d, want 4 (nine versions, newest five kept)", n)
	}
	got := surviving(t, pool, memoryID)
	if len(got) != 5 {
		t.Fatalf("survivors = %d, want 5: %v", len(got), got)
	}
	for i, id := range ids[4:] {
		if got[i] != id {
			t.Errorf("survivor %d = %s, want %s — the sweep kept the wrong end", i, got[i], id)
		}
	}
}

// TestPruneSparesTheRecentAndTheFew: nothing inside the window is touched, and
// a memory with fewer versions than the keep count loses none however old.
func TestPruneSparesTheRecentAndTheFew(t *testing.T) {
	pool := retentionPool(t)
	ctx := context.Background()

	chatty := domain.NewID(domain.PrefixMemory).String()
	recent := seedVersions(t, pool, chatty, 9, time.Now().Add(-time.Hour), time.Minute)
	liveMemory(t, pool, chatty, recent[8], "/recent.md")

	quiet := domain.NewID(domain.PrefixMemory).String()
	oldFew := seedVersions(t, pool, quiet, 3, time.Now().Add(-365*24*time.Hour), 24*time.Hour)
	liveMemory(t, pool, quiet, oldFew[2], "/quiet.md")

	n, err := pruneMemoryVersions(ctx, pool, memoryVersionRetention, memoryVersionsKept)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned %d, want 0", n)
	}
	if got := surviving(t, pool, chatty); len(got) != 9 {
		t.Errorf("recent survivors = %d, want all 9", len(got))
	}
	if got := surviving(t, pool, quiet); len(got) != 3 {
		t.Errorf("infrequently-changed survivors = %d, want all 3 — the reference keeps history beyond 30 days for these", len(got))
	}
}

// TestPruneNeverTakesALiveHead: the guard that matters most, shown against a
// head the keep window does not cover. A memory whose head pointer names an
// old version outside the newest five would, without the NOT EXISTS, lose the
// row memory_version_id resolves to — a wire 404 on a memory that is fine.
func TestPruneNeverTakesALiveHead(t *testing.T) {
	pool := retentionPool(t)
	memoryID := domain.NewID(domain.PrefixMemory).String()
	old := time.Now().Add(-90 * 24 * time.Hour)
	ids := seedVersions(t, pool, memoryID, 9, old, time.Hour)
	// The oldest row as the head: not a state the write path produces, which
	// is the point — the guard must not rest on the keep window covering it.
	liveMemory(t, pool, memoryID, ids[0], "/notes.md")

	if _, err := pruneMemoryVersions(context.Background(), pool, memoryVersionRetention, memoryVersionsKept); err != nil {
		t.Fatalf("prune: %v", err)
	}
	var dangling int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM memories m WHERE m.memory_store_id = $1
		    AND NOT EXISTS (SELECT 1 FROM memory_versions v WHERE v.id = m.memory_version_id)`,
		retStore).Scan(&dangling); err != nil {
		t.Fatal(err)
	}
	if dangling != 0 {
		t.Errorf("%d memories point at a version the sweep removed", dangling)
	}
}

// TestPruneLeavesADeletedMemoryListable: a deleted memory's lineage prunes by
// the same rule rather than being exempt — but its newest rows are the end of
// its history, so the `deleted` version that closes it always survives and the
// lineage stays listable while the store exists.
func TestPruneLeavesADeletedMemoryListable(t *testing.T) {
	pool := retentionPool(t)
	ctx := context.Background()
	memoryID := domain.NewID(domain.PrefixMemory).String()
	old := time.Now().Add(-90 * 24 * time.Hour)
	ids := seedVersions(t, pool, memoryID, 8, old, time.Hour)
	// The delete that ends it, newest of all. No memories row: it is gone.
	deletedID := domain.NewID(domain.PrefixMemoryVersion).String()
	if _, err := pool.Exec(ctx,
		`INSERT INTO memory_versions (id, memory_store_id, memory_id, operation, path, created_at)
		 VALUES ($1, $2, $3, 'deleted', '/notes.md', $4)`,
		deletedID, retStore, memoryID, old.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	n, err := pruneMemoryVersions(ctx, pool, memoryVersionRetention, memoryVersionsKept)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 4 {
		t.Errorf("pruned %d, want 4 (nine rows, newest five kept)", n)
	}
	got := surviving(t, pool, memoryID)
	if len(got) != 5 || got[4] != deletedID {
		t.Fatalf("survivors = %v, want five ending in the deleted version %s", got, deletedID)
	}
	// The oldest survivor is a real row, not the `created` one: the lineage is
	// bounded, which is the whole point of pruning it at all.
	if got[0] != ids[4] {
		t.Errorf("oldest survivor = %s, want %s", got[0], ids[4])
	}
}

// TestPruneRecordsWhatItRemoved pins the instrument: a sweep that removed rows
// counts them, and a sweep that removed none records nothing.
func TestPruneRecordsWhatItRemoved(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	pool := retentionPool(t)
	ctx := context.Background()
	memoryID := domain.NewID(domain.PrefixMemory).String()
	ids := seedVersions(t, pool, memoryID, 9, time.Now().Add(-90*24*time.Hour), time.Hour)
	liveMemory(t, pool, memoryID, ids[8], "/notes.md")

	if _, err := pruneMemoryVersions(ctx, pool, memoryVersionRetention, memoryVersionsKept); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := prunedCount(t, reader); got != 4 {
		t.Errorf("%s = %d, want 4", MetricMemoryVersionsPruned, got)
	}
	// The second sweep finds nothing; the counter must not move.
	if _, err := pruneMemoryVersions(ctx, pool, memoryVersionRetention, memoryVersionsKept); err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if got := prunedCount(t, reader); got != 4 {
		t.Errorf("%s = %d after an empty sweep, want it unmoved at 4", MetricMemoryVersionsPruned, got)
	}
}

func prunedCount(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != MetricMemoryVersionsPruned {
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
			return total
		}
	}
	return 0
}

// TestRetentionLoopSweepsThenStops drives the loop itself, not just the
// statement: a tick has to reach the sweep, and a cancelled context has to end
// it. Without the first half, the loop could stop calling the sweep entirely
// and every other test here would stay green.
func TestRetentionLoopSweepsThenStops(t *testing.T) {
	restore := SetMemoryPruneIntervalForTest(10 * time.Millisecond)
	t.Cleanup(restore)

	pool := retentionPool(t)
	memoryID := domain.NewID(domain.PrefixMemory).String()
	ids := seedVersions(t, pool, memoryID, 9, time.Now().Add(-90*24*time.Hour), time.Hour)
	liveMemory(t, pool, memoryID, ids[8], "/notes.md")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); StartMemoryRetention(ctx, pool) }()

	deadline := time.Now().Add(30 * time.Second)
	for {
		if len(surviving(t, pool, memoryID)) == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the loop never swept: survivors = %d, want 5", len(surviving(t, pool, memoryID)))
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("StartMemoryRetention did not return after its context was cancelled")
	}
}

// TestPruneAcrossOneStoresMemories is the production shape the single-memory
// rows do not reach: several memories under one store, their versions
// interleaved in time, all settled by one statement. Each keeps its own newest
// five — the correlation is per memory, not per store.
func TestPruneAcrossOneStoresMemories(t *testing.T) {
	pool := retentionPool(t)
	old := time.Now().Add(-90 * 24 * time.Hour)
	// Interleaved: staggering each memory's start by a fraction of the spacing
	// mixes their rows in the store-wide order, so a statement that ranked by
	// store rather than by memory would take the wrong ones.
	memories := make([]string, 3)
	for i := range memories {
		memories[i] = domain.NewID(domain.PrefixMemory).String()
		ids := seedVersions(t, pool, memories[i], 8, old.Add(time.Duration(i)*time.Minute), time.Hour)
		liveMemory(t, pool, memories[i], ids[7], fmt.Sprintf("/m%d.md", i))
	}

	n, err := pruneMemoryVersions(context.Background(), pool, memoryVersionRetention, memoryVersionsKept)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 9 {
		t.Errorf("pruned %d, want 9 (three memories of eight, newest five each)", n)
	}
	for i, memoryID := range memories {
		if got := surviving(t, pool, memoryID); len(got) != 5 {
			t.Errorf("memory %d survivors = %d, want 5", i, len(got))
		}
	}
}
