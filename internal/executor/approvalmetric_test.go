package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The API cannot measure an approval still queued behind a platform call. Its
// executor settlement owns that observation, and only after it commits.
func TestDelayedApprovalWaitAfterExecutorCommit(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(previous); _ = mp.Shutdown(ctx) })
	count := func() uint64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(ctx, &rm); err != nil {
			t.Fatal(err)
		}
		var n uint64
		for _, scope := range rm.ScopeMetrics {
			for _, metric := range scope.Metrics {
				if metric.Name == events.MetricApprovalWait {
					for _, point := range metric.Data.(metricdata.Histogram[float64]).DataPoints {
						if point.Sum < 0 {
							t.Fatal("negative approval wait")
						}
						n += point.Count
					}
				}
			}
		}
		return n
	}
	h := newHarness(t, &fakeSandbox{})
	h.suspend(t, writeUse("first.txt", "first"))
	first := h.types(t, "agent.tool_use")[0].ID
	uses, err := h.log.AppendWith(ctx, h.sid, []events.NewEvent{{Type: domain.EventAgentToolUse,
		Payload: json.RawMessage(`{"name":"write","input":{"file_path":"second.txt","content":"second"},"evaluated_permission":"ask"}`)}}, events.AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	confirmation, _ := json.Marshal(map[string]any{"tool_use_id": uses[0].ID, "result": "allow"})
	if _, err := h.log.AppendWith(ctx, h.sid, []events.NewEvent{
		{Type: domain.EventSessionThreadStatusIdle, Payload: json.RawMessage(`{"stop_reason":{"type":"requires_action","event_ids":[]}}`)},
		{Type: domain.EventUserToolConfirm, Payload: confirmation},
	}, events.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(map[string]any{"tool_use_id": first, "content": []any{map[string]any{"type": "text", "text": "written"}}})
	rolledBack := errors.New("settlement rollback")
	err = h.exec.commitResults(ctx, h.sid, []events.NewEvent{{Type: domain.EventAgentToolResult, Payload: result}}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := h.log.AdvanceThreadTools(ctx, tx, h.sid, "", func(string) bool { return false }); err != nil {
			return err
		}
		return rolledBack
	})
	if !errors.Is(err, rolledBack) || count() != 0 {
		t.Fatalf("rollback recorded approval wait: %v", err)
	}
	for step := 0; step < 2; step++ {
		if ok, err := h.exec.step(ctx); err != nil || !ok {
			t.Fatalf("step %d: %v %v", step, ok, err)
		}
		if got := count(); got != 1 {
			t.Fatalf("after step %d: approval waits=%d, want exactly one", step, got)
		}
	}
	if h.liveOf(t, queue.ModelTurn) != 1 {
		t.Fatal("completed tools did not resume model")
	}
}
