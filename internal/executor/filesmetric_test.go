package executor

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestFileMaterializeMetrics pins the executor's file instruments: one present
// mount records an ok, one dangling mount a not_found, and the pass records a
// duration — mirroring the skills metric contract so telemetry cannot silently
// stop with every other test green.
func TestFileMaterializeMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedFile(t, "file_met_one", "content")
	h.refFiles(t,
		[2]string{"file_met_one", "/mnt/session/uploads/file_met_one"},
		[2]string{"file_met_gone", "/mnt/session/uploads/file_met_gone"},
	)
	h.suspend(t, writeUse("out.txt", "x"))
	if _, err := h.exec.step(context.Background()); err != nil {
		t.Fatalf("step: %v", err)
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
