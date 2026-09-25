package events

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace/noop"
)

// A call whose stream ended on the clock tick it began on has a zero
// duration, and zero is still a reading: the abandoned-stream fallback is for
// a call whose end ModelDone never saw. Keyed on a zero elapsed, it timed an
// instant call to Finish instead, filing the settlement's database time as
// model latency — which TestModelRequestDurationExcludesSettlement caught once
// ModelCalling moved the clock's start next to the call. White-box, because
// the instant is a clock race no black-box test can force.
func TestAnInstantCallReadsAsInstant(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	_, span := noop.NewTracerProvider().Tracer("test").Start(context.Background(), "model_request")

	m := &ModelRequest{span: span, backend: Backend{Provider: "anthropic", Model: "m"},
		called: time.Now().Add(-time.Hour), done: true} // ended as it began, an hour ago
	m.Finish(context.Background(), false, nil)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != "gen_ai.client.operation.duration" {
				continue
			}
			pts := mt.Data.(metricdata.Histogram[float64]).DataPoints
			if len(pts) != 1 || pts[0].Sum != 0 {
				t.Errorf("duration points = %+v, want one reading of 0", pts)
			}
			return
		}
	}
	t.Error("no duration recorded for a call that ended")
}
