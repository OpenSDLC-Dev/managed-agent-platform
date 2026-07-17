package telemetry_test

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
)

func ratio(f float64) *float64 { return &f }

func TestInitRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  telemetry.Config
	}{
		{"missing service name", telemetry.Config{Endpoint: "localhost:4317"}},
		{"negative sample ratio", telemetry.Config{ServiceName: "t", SampleRatio: ratio(-0.1)}},
		{"sample ratio above one", telemetry.Config{ServiceName: "t", SampleRatio: ratio(1.5)}},
		{"NaN sample ratio", telemetry.Config{ServiceName: "t", SampleRatio: ratio(math.NaN())}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := telemetry.Init(context.Background(), tc.cfg); err == nil {
				t.Errorf("Init(%+v) = nil error, want error", tc.cfg)
			}
		})
	}
}

func TestInitWithoutEndpointIsNoOp(t *testing.T) {
	before := otel.GetTracerProvider()
	shutdown, err := telemetry.Init(context.Background(), telemetry.Config{ServiceName: "test"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if otel.GetTracerProvider() != before {
		t.Errorf("disabled Init must not replace the global tracer provider")
	}
	if fields := otel.GetTextMapPropagator().Fields(); !slices.Contains(fields, "traceparent") {
		t.Errorf("global propagator fields = %v, want W3C traceparent installed", fields)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown: %v", err)
	}
}

func TestInitExportsSpansAndMetrics(t *testing.T) {
	restoreLogging(t)
	collector := startFakeCollector(t)
	ctx := context.Background()

	shutdown, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: "telemetry-test",
		Endpoint:    collector.addr,
		Insecure:    true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	_, span := otel.Tracer("contract").Start(ctx, "test-operation")
	span.End()

	counter, err := otel.Meter("contract").Int64Counter("test_counter")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(ctx, 1)

	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if names := collector.spanNames(); !slices.Contains(names, "test-operation") {
		t.Errorf("collector spans = %v, want to contain %q", names, "test-operation")
	}
	if got := collector.resourceAttr("service.name"); got != "telemetry-test" {
		t.Errorf("resource service.name = %q, want %q", got, "telemetry-test")
	}
	if names := collector.metricNames(); !slices.Contains(names, "test_counter") {
		t.Errorf("collector metrics = %v, want to contain %q", names, "test_counter")
	}
}

func TestSampleRatioIsApplied(t *testing.T) {
	restoreLogging(t)
	collector := startFakeCollector(t)
	ctx := context.Background()

	shutdown, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: "sampling-test",
		Endpoint:    collector.addr,
		Insecure:    true,
		SampleRatio: ratio(0.0001),
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	const total = 300
	for i := 0; i < total; i++ {
		_, span := otel.Tracer("contract").Start(ctx, "sampled-op")
		span.End()
	}

	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// At ratio 1e-4 the chance of even 60/300 root spans sampling in is
	// effectively zero; receiving that many means the ratio was ignored.
	if got := len(collector.spanNames()); got >= 60 {
		t.Errorf("collector received %d/%d spans at ratio 0.0001, sampler not applied", got, total)
	}
}

func TestSampleRatioZeroSendsNoTraces(t *testing.T) {
	restoreLogging(t)
	collector := startFakeCollector(t)
	ctx := context.Background()

	// Ratio 0 is the operator's "keep metrics, drop traces" switch — it
	// must not be conflated with "unset, sample everything" (nil).
	shutdown, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: "zero-ratio-test",
		Endpoint:    collector.addr,
		Insecure:    true,
		SampleRatio: ratio(0),
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	for i := 0; i < 50; i++ {
		_, span := otel.Tracer("contract").Start(ctx, "dropped-op")
		span.End()
	}
	counter, err := otel.Meter("contract").Int64Counter("still_flowing")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(ctx, 1)

	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if names := collector.spanNames(); len(names) != 0 {
		t.Errorf("ratio 0 exported spans %v, want none", names)
	}
	if names := collector.metricNames(); !slices.Contains(names, "still_flowing") {
		t.Errorf("ratio 0 must not stop metrics; collector metrics = %v", names)
	}
}
