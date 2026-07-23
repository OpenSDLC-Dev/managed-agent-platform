package worker

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestSetupSkillsMetrics pins the worker's skills instruments — the executor's
// twin, same instrument names in a second scope. Without it the counter or
// histogram could silently stop recording with every other test green
// (telemetry is deliberately never load-bearing), and a corrupt archive could
// quietly be counted as an ordinary failure, losing the one signal that
// separates storage corruption from a dangling reference.
func TestSetupSkillsMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "wire-met-one", "100", "metric-wire", map[string]string{"SKILL.md": "m"})
	h.seedSkill(t, "wire-met-bad", "100", "corrupt-wire", map[string]string{"SKILL.md": "m"})
	h.swapArchive(t, "wire-met-bad", "100", "corrupt-wire", map[string]string{"SKILL.md": "swapped"})
	h.refSkills(t,
		[2]string{"wire-met-one", "latest"},
		[2]string{"wire-met-gone", "latest"},
		[2]string{"wire-met-bad", "100"},
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
			case MetricSkillsMaterialized:
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("%s is %T, want an int64 sum", m.Name, m.Data)
				}
				for _, p := range sum.DataPoints {
					if v, ok := p.Attributes.Value("outcome"); ok {
						counts[v.AsString()] += p.Value
					}
				}
			case MetricSkillsMaterializeDuration:
				hist, ok := m.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("%s is %T, want a float64 histogram", m.Name, m.Data)
				}
				durations = len(hist.DataPoints)
			}
		}
	}
	if counts[skillOutcomeOK] != 1 || counts[skillOutcomeNotFound] != 1 || counts[skillOutcomeCorrupt] != 1 {
		t.Errorf("outcome counts = %v, want one ok, one not_found, one corrupt", counts)
	}
	if durations == 0 {
		t.Error("no materialize-duration point recorded")
	}
}
