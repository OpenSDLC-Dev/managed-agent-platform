package executor

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestMaterializeMetrics pins the executor's skills instruments: without it,
// the counter or histogram could silently stop recording with every other
// test green (telemetry is deliberately never load-bearing).
func TestMaterializeMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	sb := &fakeSandbox{}
	h := newHarness(t, sb)
	h.seedSkill(t, "skill_met_one", "100", "metric-skill", map[string]string{"SKILL.md": "m"})
	h.refSkills(t,
		[2]string{"skill_met_one", "latest"},
		[2]string{"skill_met_gone", "latest"},
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
	if counts[skillOutcomeOK] != 1 || counts[skillOutcomeNotFound] != 1 {
		t.Errorf("outcome counts = %v, want one ok and one not_found", counts)
	}
	if durations == 0 {
		t.Error("no materialize-duration point recorded")
	}
}
