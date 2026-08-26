package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/memsync"
)

// Plan 36 slice 4's four memory instruments (#488). The skills, files and
// repos recorders each have this test; without it the memory ones could stop
// recording with every other memory test still green, because none of them
// reads a meter.

// memoryMeter installs a manual reader for one test and answers with the
// collector: the counter's points summed by their one attribute, and how many
// histogram points each duration instrument holds.
func memoryMeter(t *testing.T) func(t *testing.T) (counts map[string]int64, durations map[string]int) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	return func(t *testing.T) (map[string]int64, map[string]int) {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		counts, durations := map[string]int64{}, map[string]int{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				switch m.Name {
				case MetricMemoryMaterialized, MetricMemorySyncActions:
					sum, ok := m.Data.(metricdata.Sum[int64])
					if !ok {
						t.Fatalf("%s is %T, want an int64 sum", m.Name, m.Data)
					}
					// One attribute each — outcome on the materialize
					// counter, action on the sync one — so the key needs no
					// instrument prefix and the two never collide.
					for _, p := range sum.DataPoints {
						for _, key := range []attribute.Key{"outcome", "action"} {
							if v, ok := p.Attributes.Value(key); ok {
								counts[v.AsString()] += p.Value
							}
						}
					}
				case MetricMemoryMaterializeDuration, MetricMemorySyncDuration:
					hist, ok := m.Data.(metricdata.Histogram[float64])
					if !ok {
						t.Fatalf("%s is %T, want a float64 histogram", m.Name, m.Data)
					}
					durations[m.Name] = len(hist.DataPoints)
				}
			}
		}
		return counts, durations
	}
}

// TestMemoryMaterializeMetrics: a session holding one live store and one
// whose row is gone records an ok beside a not_found, and the pass records a
// duration; a second run over the same sandbox records the unchanged that
// says the marker was found and nothing re-landed.
func TestMemoryMaterializeMetrics(t *testing.T) {
	collect := memoryMeter(t)

	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedMemoryStore(t, memStoreID, "Notes")
	h.seedMemory(t, memStoreID, "/notes.md", "hello")
	// Two elements, so one pass records both outcomes — the files test's
	// present-plus-dangling shape.
	raw, _ := json.Marshal([]map[string]any{
		{"type": "memory_store", "memory_store_id": memStoreID, "access": "read_write",
			"name": "Notes", "description": "", "mount_path": memMount},
		{"type": "memory_store", "memory_store_id": "memstore_01ARZ3NDEKTSV4RRFFQ69G5FBB", "access": "read_write",
			"name": "Gone", "description": "", "mount_path": "/mnt/memory/gone"},
	})
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resources = $2::jsonb WHERE id = $1`, h.sid.String(), raw); err != nil {
		t.Fatalf("set session resources: %v", err)
	}
	h.step(t)

	counts, durations := collect(t)
	if counts[memoryOutcomeOK] != 1 || counts[memoryOutcomeNotFound] != 1 {
		t.Errorf("outcome counts = %v, want one ok and one not_found", counts)
	}
	if durations[MetricMemoryMaterializeDuration] == 0 {
		t.Error("no memory-materialize-duration point recorded")
	}

	h.step(t)
	counts, _ = collect(t)
	if counts[memoryOutcomeUnchanged] != 1 {
		t.Errorf("unchanged = %d after a second run over the same sandbox, want 1 (counts %v)", counts[memoryOutcomeUnchanged], counts)
	}
}

// TestMemorySyncActionMetrics: one run that does all five things the actions
// instrument separates — a push, a deletion, a pull, a conflict the store
// wins, and a refusal — and the sync records a duration. Two of the counts are
// not one, and both are the instrument's design rather than this test's
// accident: a conflict is a pull that also carries the flag (memory.go's Pull
// arm counts both, so the store's bytes landing locally is still a pull), and
// a run over a mount that already exists syncs twice — once before the tools
// so they read a current directory, once after — of which only the refused
// file is still there to be judged again the second time.
func TestMemorySyncActionMetrics(t *testing.T) {
	h, sb := materialized(t, "read_write")
	collect := memoryMeter(t) // after materialization: this run's sync only

	ctx := context.Background()
	// A push and a deletion, both local.
	sb.files[memMount+"/new.md"] = "fresh"
	delete(sb.files, memMount+"/a/b.md")
	// A refusal: over the per-memory content ceiling.
	sb.files[memMount+"/big.md"] = strings.Repeat("x", memsync.MaxContentBytes+1)
	// A conflict: /notes.md edited on both sides since the baseline.
	sb.files[memMount+"/notes.md"] = "mine"
	if _, err := h.pool.Exec(ctx,
		`UPDATE memories SET content = 'theirs', content_sha256 = $2 WHERE memory_store_id = $1 AND path = '/notes.md'`,
		memStoreID, sha256hex([]byte("theirs"))); err != nil {
		t.Fatal(err)
	}
	// A pull: a memory that appeared in the store since the baseline.
	h.seedMemory(t, memStoreID, "/c.md", "new remote")
	h.step(t)

	counts, durations := collect(t)
	want := map[string]int64{
		"pulled":   2, // the remote create, and the conflict's own overwrite
		"pushed":   1,
		"deleted":  1,
		"conflict": 1,
		"refused":  2, // the oversized file, judged by both of the run's syncs
	}
	for action, n := range want {
		if counts[action] != n {
			t.Errorf("%s = %d, want %d (counts %v)", action, counts[action], n, counts)
		}
	}
	if durations[MetricMemorySyncDuration] == 0 {
		t.Error("no memory-sync-duration point recorded")
	}
}
