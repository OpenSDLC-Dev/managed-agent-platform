package brain_test

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collectBrainMetrics routes the process's meter through a reader for one test,
// as production wiring routes it through the global provider.
func collectBrainMetrics(t *testing.T) func() metricdata.ResourceMetrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	return func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		return rm
	}
}

func tokenSums(t *testing.T, rm metricdata.ResourceMetrics) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "gen_ai.client.token.usage" {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[int64])
			if !ok {
				t.Fatalf("gen_ai.client.token.usage is %T, want an int64 histogram", m.Data)
			}
			for _, dp := range h.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					if string(kv.Key) == "gen_ai.token.type" {
						out[kv.Value.Emit()] = dp.Sum
					}
				}
			}
		}
	}
	return out
}

// The GenAI metrics are only as good as the brain's call into them. Everything
// about ModelDone is proven in internal/events, but those tests call it
// directly: nothing there notices if the brain stops calling it, and then the
// token histogram silently reports nothing for every turn while the duration
// quietly re-includes the settlement transaction — with the whole suite green.
// This is the wiring's own test, driving a real turn end to end.
func TestATurnReportsWhatTheModelSpent(t *testing.T) {
	collect := collectBrainMetrics(t)

	h := newHarness(t, [][]provider.Chunk{{
		textChunk(0, "hi"),
		done("end_turn", 25), // done() reports 10 input tokens
	}}, nil)
	h.wake(t, "hello")
	h.runOnce(t)

	got := tokenSums(t, collect())
	if len(got) == 0 {
		t.Fatal("a completed turn recorded no gen_ai.client.token.usage: the brain never told the span what the model spent")
	}
	if got["input"] != 10 || got["output"] != 25 {
		t.Errorf("usage = in %d / out %d, want 10/25", got["input"], got["output"])
	}
}
