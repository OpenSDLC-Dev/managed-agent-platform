package worker

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestWorkerFileMaterializeMetrics pins the worker's file instruments — the
// executor twin's names on the worker meter: one present mount records an ok, one
// dangling mount (a valid id the content lane 404s) a not_found, and the pass
// records a duration, so telemetry cannot silently stop with every other test
// green.
func TestWorkerFileMaterializeMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	okID := domain.NewID("file").String()
	goneID := domain.NewID("file").String() // valid id, never seeded -> 404 -> not_found
	h.seedFile(t, okID, "ok.txt", "text/plain", "content")
	h.refFileMounts(t,
		[2]string{okID, "/workspace/uploads/ok.txt"},
		[2]string{goneID, "/workspace/uploads/gone.txt"},
	)
	h.suspend(t, writeUse("out.txt", "x"))
	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	counts := map[string]int64{}
	durations := 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case MetricFilesMaterialized:
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("%s is %T, want an int64 sum", m.Name, m.Data)
				}
				for _, p := range sum.DataPoints {
					if v, ok := p.Attributes.Value("outcome"); ok {
						counts[v.AsString()] += p.Value
					}
				}
			case MetricFilesMaterializeDuration:
				hist, ok := m.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("%s is %T, want a float64 histogram", m.Name, m.Data)
				}
				durations = len(hist.DataPoints)
			}
		}
	}
	if counts[fileOutcomeOK] != 1 || counts[fileOutcomeNotFound] != 1 {
		t.Errorf("outcome counts = %v, want one ok and one not_found", counts)
	}
	if durations == 0 {
		t.Error("no file-materialize-duration point recorded")
	}
}
