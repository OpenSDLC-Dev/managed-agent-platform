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

// memoryMetrics is one collection: each counter's points summed by its own
// attribute, kept apart by instrument so a future action named like an outcome
// cannot merge the two, and how many histogram points each duration instrument
// holds.
type memoryMetrics struct {
	outcomes  map[string]int64 // memory.materialized, by outcome
	actions   map[string]int64 // memory.sync.actions, by action
	durations map[string]int   // point counts, keyed by instrument name
}

// memoryMeter installs a manual reader for one test and answers with the
// collector.
func memoryMeter(t *testing.T) func() memoryMetrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	sumBy := func(m metricdata.Metrics, key attribute.Key) map[string]int64 {
		t.Helper()
		sum, ok := m.Data.(metricdata.Sum[int64])
		if !ok {
			t.Fatalf("%s is %T, want an int64 sum", m.Name, m.Data)
		}
		out := map[string]int64{}
		for _, p := range sum.DataPoints {
			if v, ok := p.Attributes.Value(key); ok {
				out[v.AsString()] += p.Value
			}
		}
		return out
	}

	return func() memoryMetrics {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		got := memoryMetrics{outcomes: map[string]int64{}, actions: map[string]int64{}, durations: map[string]int{}}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				switch m.Name {
				case MetricMemoryMaterialized:
					got.outcomes = sumBy(m, "outcome")
				case MetricMemorySyncActions:
					got.actions = sumBy(m, "action")
				case MetricMemoryMaterializeDuration, MetricMemorySyncDuration:
					hist, ok := m.Data.(metricdata.Histogram[float64])
					if !ok {
						t.Fatalf("%s is %T, want a float64 histogram", m.Name, m.Data)
					}
					got.durations[m.Name] = len(hist.DataPoints)
				}
			}
		}
		return got
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

	got := collect()
	if got.outcomes[memoryOutcomeOK] != 1 || got.outcomes[memoryOutcomeNotFound] != 1 {
		t.Errorf("outcome counts = %v, want one ok and one not_found", got.outcomes)
	}
	if got.durations[MetricMemoryMaterializeDuration] == 0 {
		t.Error("no memory-materialize-duration point recorded")
	}

	h.step(t)
	got = collect()
	if got.outcomes[memoryOutcomeUnchanged] != 1 {
		t.Errorf("unchanged = %d after a second run over the same sandbox, want 1 (outcomes %v)", got.outcomes[memoryOutcomeUnchanged], got.outcomes)
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

	got := collect()
	want := map[string]int64{
		"pulled":   2, // the remote create, and the conflict's own overwrite
		"pushed":   1,
		"deleted":  1,
		"conflict": 1,
		"refused":  2, // the oversized file, judged by both of the run's syncs
	}
	for action, n := range want {
		if got.actions[action] != n {
			t.Errorf("%s = %d, want %d (actions %v)", action, got.actions[action], n, got.actions)
		}
	}
	if got.durations[MetricMemorySyncDuration] == 0 {
		t.Error("no memory-sync-duration point recorded")
	}
}
