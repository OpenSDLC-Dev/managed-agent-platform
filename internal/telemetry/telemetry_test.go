package telemetry_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
)

func TestInitRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  telemetry.Config
	}{
		{"missing service name", telemetry.Config{Endpoint: "localhost:4317"}},
		{"negative sample ratio", telemetry.Config{ServiceName: "t", SampleRatio: -0.1}},
		{"sample ratio above one", telemetry.Config{ServiceName: "t", SampleRatio: 1.5}},
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
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown: %v", err)
	}
}

func TestInitExportsSpansAndMetrics(t *testing.T) {
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
	collector := startFakeCollector(t)
	ctx := context.Background()

	shutdown, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: "sampling-test",
		Endpoint:    collector.addr,
		Insecure:    true,
		SampleRatio: 0.0001,
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
