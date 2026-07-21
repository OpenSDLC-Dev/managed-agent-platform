package brain_test

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
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

// tokenPoints returns the token histogram's data points. tokenSums cannot
// answer this test's question: a zero-valued point and an absent point both
// sum to zero, and the whole point of #90 is that those are different facts.
func tokenPoints(t *testing.T, rm metricdata.ResourceMetrics) []metricdata.HistogramDataPoint[int64] {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "gen_ai.client.token.usage" {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[int64])
			if !ok {
				t.Fatalf("gen_ai.client.token.usage is %T, want an int64 histogram", m.Data)
			}
			return h.DataPoints
		}
	}
	return nil
}

// An endpoint that reports no usage — an OpenAI-compatible gateway ignoring
// stream_options.include_usage — must record no token reading at all. Recording
// zeroes instead would dilute the histogram with turns that look free (#90).
func TestATurnWhoseEndpointReportedNoUsageRecordsNoTokens(t *testing.T) {
	collect := collectBrainMetrics(t)

	h := newHarness(t, [][]provider.Chunk{{
		textChunk(0, "hi"),
		// Deliberately not done(): that helper always attaches usage.
		{Kind: provider.KindDone, StopReason: "end_turn"},
	}}, nil)
	h.wake(t, "hello")
	h.runOnce(t)

	if pts := tokenPoints(t, collect()); len(pts) != 0 {
		t.Errorf("recorded %d token data point(s), want none: the endpoint reported no usage", len(pts))
	}
}

// floatPoints returns a float histogram's data points by name.
func floatPoints(t *testing.T, rm metricdata.ResourceMetrics, name string) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is %T, want a float64 histogram", name, m.Data)
			}
			return h.DataPoints
		}
	}
	return nil
}

// Time to first token is the platform's responsiveness signal, and it is a brain
// fact: the clock starts when the brain claims the work — replay and request
// assembly are latency the user feels — and stops at the first content the model
// streams. Nothing else in the turn observes both boundaries, so this is the
// wiring's own test that the two are captured and recorded.
func TestATurnRecordsTimeToFirstToken(t *testing.T) {
	collect := collectBrainMetrics(t)

	h := newHarness(t, [][]provider.Chunk{{
		textChunk(0, "hi"),
		done("end_turn", 25),
	}}, nil)
	h.wake(t, "hello")
	h.runOnce(t)

	pts := floatPoints(t, collect(), brain.MetricTimeToFirstToken)
	if len(pts) != 1 {
		t.Fatalf("%s points = %d, want 1", brain.MetricTimeToFirstToken, len(pts))
	}
	if pts[0].Sum <= 0 {
		t.Errorf("time to first token = %vs, want positive (claim precedes the first token)", pts[0].Sum)
	}
	got := map[string]string{}
	for _, kv := range pts[0].Attributes.ToSlice() {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	if got["gen_ai.provider.name"] != "fake" {
		t.Errorf("provider attr = %q, want fake", got["gen_ai.provider.name"])
	}
}

// A turn that streams no content — the model went straight to a tool call — has
// no first token to measure. Recording zero would report an instant response that
// never happened, so the metric stays silent, the same absent-is-not-zero rule
// the token histogram follows.
func TestATurnWithNoStreamedContentRecordsNoTimeToFirstToken(t *testing.T) {
	collect := collectBrainMetrics(t)

	h := newHarness(t, [][]provider.Chunk{{
		toolUseChunk("tu_1", "bash"),
		done("tool_use", 5),
	}}, nil)
	h.wake(t, "run something")
	h.runOnce(t)

	if pts := floatPoints(t, collect(), brain.MetricTimeToFirstToken); len(pts) != 0 {
		t.Errorf("recorded %d first-token point(s) for a turn that streamed no content, want 0", len(pts))
	}
}

// brainStatusCount reads the session.status.transitions counter for one status.
func brainStatusCount(t *testing.T, rm metricdata.ResourceMetrics, status string) int64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != events.MetricSessionStatus {
				continue
			}
			s, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", events.MetricSessionStatus, m.Data)
			}
			for _, dp := range s.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					if string(kv.Key) == "session.status" && kv.Value.Emit() == status {
						return dp.Value
					}
				}
			}
		}
	}
	return 0
}

// The brain drives real status transitions the events unit test cannot: a turn
// that settles moves the session running→idle from inside a brain-owned
// transaction (not the AppendWith wrapper), so this proves that commit site
// records too, and only after it commits.
func TestASettledTurnRecordsSessionStatusTransitions(t *testing.T) {
	collect := collectBrainMetrics(t)

	h := newHarness(t, [][]provider.Chunk{{
		textChunk(0, "hi"),
		done("end_turn", 5),
	}}, nil)
	h.wake(t, "hello") // idle→running, via the harness's AppendWith
	h.runOnce(t)       // running→idle, via the brain's own settle commit

	rm := collect()
	if got := brainStatusCount(t, rm, "running"); got < 1 {
		t.Errorf("running transitions = %d, want at least 1 (the wake)", got)
	}
	if got := brainStatusCount(t, rm, "idle"); got != 1 {
		t.Errorf("idle transitions = %d, want 1 (the settle)", got)
	}
}
