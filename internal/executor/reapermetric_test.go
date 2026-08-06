package executor

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// TestReapMetric pins the reaper's counter and its tier attribute: archived
// and terminated have identical side effects, so the label is the only
// observable difference between them — and telemetry is deliberately never
// load-bearing, so without this pin it could silently stop recording with
// every other test green.
func TestReapMetric(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	h := newHarness(t, &fakeSandbox{})
	archiveSession(t, h)
	h.prov.owned = []domain.ID{h.sid}
	if err := h.exec.reapPass(context.Background()); err != nil {
		t.Fatalf("reap pass: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	counts := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != MetricSessionsReaped {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", m.Name, m.Data)
			}
			for _, p := range sum.DataPoints {
				if v, ok := p.Attributes.Value("tier"); ok {
					counts[v.AsString()] += p.Value
				}
			}
		}
	}
	if counts[string(tierArchived)] != 1 {
		t.Errorf("reap counts by tier = %v, want one %q", counts, tierArchived)
	}
}
