package queue_test

import (
	"context"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// gaugeValue returns the observed int64 gauge value for one environment, and
// whether a data point for that environment was found at all.
func gaugeValue(t *testing.T, rm metricdata.ResourceMetrics, name, envID string) (int64, bool) {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 gauge", name, m.Data)
			}
			for _, dp := range g.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					if string(kv.Key) == "environment.id" && kv.Value.Emit() == envID {
						return dp.Value, true
					}
				}
			}
		}
	}
	return 0, false
}

// The gauges are the /work/stats numbers sampled at collection time: two queued
// items with one polled reads back as depth 1 / pending 1, keyed by environment.
// A cloud environment's queue is not reported — workers_polling is meaningless
// where the executor claims instead of polling.
func TestQueueStatsGaugesObserveStats(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	q := queue.New(pool)

	s1, env := pgtest.NewSession(t, pool, "self_hosted")
	s2 := pgtest.NewSessionInEnv(t, pool, env)
	if _, err := q.Enqueue(ctx, pool, env, s1, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, pool, env, s2, queue.ToolExec); err != nil {
		t.Fatal(err)
	}
	// Poll one → it moves from depth to pending.
	if w, err := q.Poll(ctx, env, time.Minute); err != nil || w == nil {
		t.Fatalf("poll: %v", err)
	}

	// A cloud environment with its own queued item, which must not be reported.
	cs, cloudEnv := pgtest.NewSession(t, pool, "cloud")
	if _, err := q.Enqueue(ctx, pool, cloudEnv, cs, queue.ToolExec); err != nil {
		t.Fatal(err)
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	reg, err := q.RegisterMetrics()
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { _ = reg.Unregister() })

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	if v, ok := gaugeValue(t, rm, queue.MetricQueueDepth, env.String()); !ok || v != 1 {
		t.Errorf("queue.depth = %d (found=%v), want 1", v, ok)
	}
	if v, ok := gaugeValue(t, rm, queue.MetricQueuePending, env.String()); !ok || v != 1 {
		t.Errorf("queue.pending = %d (found=%v), want 1", v, ok)
	}
	if _, ok := gaugeValue(t, rm, queue.MetricQueueWorkersPolling, env.String()); !ok {
		t.Errorf("queue.workers_polling not observed for the self_hosted environment")
	}
	if _, ok := gaugeValue(t, rm, queue.MetricQueueDepth, cloudEnv.String()); ok {
		t.Errorf("queue.depth reported a cloud environment, want self_hosted only")
	}
}
