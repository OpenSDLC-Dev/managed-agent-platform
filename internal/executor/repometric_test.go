package executor

import (
	"context"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestRepoMaterializeMetrics is m-telemetry: one clone that lands and one that
// fails record their own outcomes, the pass records a duration, and the landed
// repository records its shipped size — so telemetry cannot silently stop with
// every other test green.
func TestRepoMaterializeMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	good := newGitFixture(t, map[string]string{"README.md": "x\n"})
	bad := newGitFixture(t, map[string]string{"README.md": "y\n"})
	bad.status.Store(http.StatusNotFound)

	h := newHarness(t, &fakeSandbox{})
	h.seedRepoResource(t, "sesrsc_met_ok", good.url(), "/workspace/ok", "ghp_fixture", nil)
	h.seedRepoResource(t, "sesrsc_met_bad", bad.url(), "/workspace/bad", "ghp_fixture", nil)

	h.runPass(t)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	counts := map[string]int64{}
	durations, sizes := 0, 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case MetricReposMaterialized:
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("%s is %T, want an int64 sum", m.Name, m.Data)
				}
				for _, p := range sum.DataPoints {
					if v, ok := p.Attributes.Value("outcome"); ok {
						counts[v.AsString()] += p.Value
					}
				}
			case MetricReposMaterializeDuration:
				hist, ok := m.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("%s is %T, want a float64 histogram", m.Name, m.Data)
				}
				durations = len(hist.DataPoints)
			case MetricReposMaterializeBytes:
				hist, ok := m.Data.(metricdata.Histogram[int64])
				if !ok {
					t.Fatalf("%s is %T, want an int64 histogram", m.Name, m.Data)
				}
				sizes = len(hist.DataPoints)
			}
		}
	}
	if counts[repoOutcomeOK] != 1 || counts[repoOutcomeNotFound] != 1 {
		t.Errorf("outcome counts = %v, want one ok and one not_found", counts)
	}
	if durations == 0 {
		t.Error("no repo-materialize-duration point recorded")
	}
	if sizes == 0 {
		t.Error("no repo-materialize-bytes point recorded for the landed clone")
	}
}
