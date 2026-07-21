package api_test

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collectMetrics routes the process's meter through a manual reader for one
// test, so the handler's in-process metric recording can be read back.
func collectMetrics(t *testing.T) func() metricdata.ResourceMetrics {
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

func apiStatusCount(t *testing.T, rm metricdata.ResourceMetrics, status string) int64 {
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

// A user.message waking an idle session flips it to running through the API's own
// commit — the production path the brain harness never exercises. This is where
// most idle→running transitions really happen, so it must be counted here.
func TestUserMessageRecordsRunningTransition(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	sid := eventsFixture(t, s)

	sendEvents(t, s, sid, userMessage("hello"))

	if got := apiStatusCount(t, collect(), "running"); got != 1 {
		t.Errorf("running transitions = %d, want 1 (the user.message woke the session)", got)
	}
}
