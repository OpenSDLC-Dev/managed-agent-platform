package telemetry_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// The business metrics issue #44 asks for must survive the real OTLP export, not
// only the in-process manual readers each package tests against. This mirrors the
// traces/synthetic-counter contract test above: it drives every business metric
// name through the exporting provider Init installs and asserts each one reaches
// a collector. The values and attributes are proven per package; this proves the
// export path carries these exact instruments, whose names come from the same
// exported constants the production code records under, so a rename cannot leave
// this test asserting a name nothing emits.
func TestBusinessMetricsExportOverOTLP(t *testing.T) {
	restoreLogging(t)
	collector := startFakeCollector(t)
	ctx := context.Background()

	shutdown, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: "metrics-contract",
		Endpoint:    collector.addr,
		Insecure:    true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	m := otel.Meter("contract")
	// Synchronous instruments recorded directly under their real names.
	for _, name := range []string{
		brain.MetricTimeToFirstToken,
		toolset.MetricToolDuration,
		"gen_ai.client.operation.duration",
	} {
		h, err := m.Float64Histogram(name)
		if err != nil {
			t.Fatalf("Float64Histogram %q: %v", name, err)
		}
		h.Record(ctx, 1)
	}
	for _, name := range []string{
		events.MetricCacheTokenUsage,
		"gen_ai.client.token.usage",
	} {
		h, err := m.Int64Histogram(name)
		if err != nil {
			t.Fatalf("Int64Histogram %q: %v", name, err)
		}
		h.Record(ctx, 1)
	}
	// The two session-lifecycle metrics go through their real record helpers,
	// which need no database.
	events.RecordSessionStatus(ctx, domain.SessionIdle)
	events.RecordApprovalWait(ctx, 1)
	// The queue gauges are asynchronous: register each with a callback so the
	// final collection samples it.
	for _, name := range []string{
		queue.MetricQueueDepth,
		queue.MetricQueuePending,
		queue.MetricQueueWorkersPolling,
	} {
		if _, err := m.Int64ObservableGauge(name, metric.WithInt64Callback(
			func(_ context.Context, o metric.Int64Observer) error {
				o.Observe(1)
				return nil
			})); err != nil {
			t.Fatalf("Int64ObservableGauge %q: %v", name, err)
		}
	}

	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	want := []string{
		brain.MetricTimeToFirstToken,
		toolset.MetricToolDuration,
		"gen_ai.client.operation.duration",
		"gen_ai.client.token.usage",
		events.MetricCacheTokenUsage,
		events.MetricSessionStatus,
		events.MetricApprovalWait,
		queue.MetricQueueDepth,
		queue.MetricQueuePending,
		queue.MetricQueueWorkersPolling,
	}
	got := collector.metricNames()
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("business metric %q did not reach the collector; got %v", w, got)
		}
	}
}
