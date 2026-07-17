package toolset_test

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// collectTools installs a manual-reader meter provider for the duration of a
// test and returns a collect func. The global provider is restored after.
func collectTools(t *testing.T) func() metricdata.ResourceMetrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	return func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		return rm
	}
}

// findHistogram returns the named float64 histogram's data points, or nil.
func findHistogram(rm metricdata.ResourceMetrics, name string) []metricdata.HistogramDataPoint[float64] {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok {
				return h.DataPoints
			}
		}
	}
	return nil
}

func attrOf(dp metricdata.HistogramDataPoint[float64], key string) (string, bool) {
	for _, kv := range dp.Attributes.ToSlice() {
		if string(kv.Key) == key {
			return kv.Value.Emit(), true
		}
	}
	return "", false
}

// TestToolRunRecordsDuration: every tool call, on either deployment point,
// goes through Runner.Run — so that is where the platform's one tool-execution
// metric belongs. Without it a slow session is a mystery: the event log says
// which tools ran, never how long any of them took.
func TestToolRunRecordsDuration(t *testing.T) {
	collect := collectTools(t)
	r := runner(t)

	if _, err := r.Run(context.Background(), domain.NewID("sevt"), "bash",
		json.RawMessage(`{"command":"echo metered"}`)); err != nil {
		t.Fatalf("run: %v", err)
	}

	dps := findHistogram(collect(), "tool.execution.duration")
	if len(dps) != 1 {
		t.Fatalf("tool.execution.duration data points = %d, want 1", len(dps))
	}
	dp := dps[0]
	if dp.Count != 1 {
		t.Errorf("count = %d, want 1", dp.Count)
	}
	if dp.Sum <= 0 {
		t.Errorf("sum = %v, want a positive duration", dp.Sum)
	}
	if got, ok := attrOf(dp, "gen_ai.tool.name"); !ok || got != "bash" {
		t.Errorf("gen_ai.tool.name = %q (present=%v), want %q", got, ok, "bash")
	}
	if got, ok := attrOf(dp, "error.type"); ok {
		t.Errorf("error.type = %q on a successful run, want it absent", got)
	}
}

// A tool that fails at the tool level — a nonzero exit, a missing file — is
// the model's business, not a platform fault, but it is still the thing to
// look at when a suite regresses. The metric has to tell the two apart.
func TestToolRunRecordsToolErrorSeparately(t *testing.T) {
	collect := collectTools(t)
	r := runner(t)

	res, err := r.Run(context.Background(), domain.NewID("sevt"), "read",
		json.RawMessage(`{"file_path":"/nope/missing.txt"}`))
	if err != nil {
		t.Fatalf("a tool-level failure must not be a backend fault: %v", err)
	}
	if !res.IsError {
		t.Fatal("reading a missing file should be a tool error")
	}

	dps := findHistogram(collect(), "tool.execution.duration")
	if len(dps) != 1 {
		t.Fatalf("data points = %d, want 1", len(dps))
	}
	if got, ok := attrOf(dps[0], "error.type"); !ok || got != "tool_error" {
		t.Errorf("error.type = %q (present=%v), want %q", got, ok, "tool_error")
	}
	if got, _ := attrOf(dps[0], "gen_ai.tool.name"); got != "read" {
		t.Errorf("gen_ai.tool.name = %q, want %q", got, "read")
	}
}

// An unknown tool never reaches a sandbox, but it is still a tool call the
// model made and a regression worth seeing.
func TestToolRunRecordsUnknownTool(t *testing.T) {
	collect := collectTools(t)
	r := runner(t)

	if _, err := r.Run(context.Background(), domain.NewID("sevt"), "web_fetch",
		json.RawMessage(`{}`)); err != nil {
		t.Fatalf("run: %v", err)
	}

	dps := findHistogram(collect(), "tool.execution.duration")
	if len(dps) != 1 {
		t.Fatalf("data points = %d, want 1", len(dps))
	}
	if got, _ := attrOf(dps[0], "gen_ai.tool.name"); got != "web_fetch" {
		t.Errorf("gen_ai.tool.name = %q, want %q", got, "web_fetch")
	}
	if got, ok := attrOf(dps[0], "error.type"); !ok || got != "tool_error" {
		t.Errorf("error.type = %q (present=%v), want %q", got, ok, "tool_error")
	}
}
