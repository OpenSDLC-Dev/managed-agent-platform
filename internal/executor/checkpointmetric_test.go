package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// TestCheckpointAndRestoreMetrics pins both transfer instruments and their
// outcome attribute — telemetry is never load-bearing, so without the pin a
// counter could silently stop recording with every other test green. One
// successful capture, one failed restore: the two outcomes the TTL tier's
// operators alert on.
func TestCheckpointAndRestoreMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	sb := &fakeSandbox{}
	sb.execHook = func(req sandbox.ExecRequest) *sandbox.ExecResult {
		if strings.Contains(req.Command, "tar -xf") {
			return &sandbox.ExecResult{ExitCode: 1}
		}
		return nil
	}
	h := newHarness(t, sb)
	h.prov.owned = []domain.ID{h.sid}
	h.prov.exports = map[string][]byte{
		"/workspace": exportTar(t, map[string]string{"workspace/f.txt": "x"}, nil),
	}
	if err := h.exec.captureCheckpoint(context.Background(), h.sid); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if _, err := h.exec.provisionSandbox(context.Background(), h.sid,
		sessionRun{networking: domain.Networking{Type: domain.NetUnrestricted}}, func() {}); err == nil {
		t.Fatal("restore succeeded with a failing extraction")
	}

	// And the degraded outcome slice 5's TTL tier alerts on: an over-budget
	// capture records too_large, distinct from error.
	tiny := newHarnessWith(t, &fakeProvider{sb: &fakeSandbox{}}, Config{CheckpointMaxBytes: 8})
	tiny.prov = tiny.exec.provider.(*fakeProvider)
	tiny.prov.owned = []domain.ID{tiny.sid}
	tiny.prov.exports = map[string][]byte{
		"/workspace": exportTar(t, map[string]string{"workspace/big.bin": "far more than eight bytes"}, nil),
	}
	if err := tiny.exec.captureCheckpoint(context.Background(), tiny.sid); !errors.Is(err, ErrCheckpointTooLarge) {
		t.Fatalf("over-budget capture: %v, want ErrCheckpointTooLarge", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	counts := map[string]int64{}
	durations := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case MetricCheckpoints, MetricRestores:
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("%s is %T, want an int64 sum", m.Name, m.Data)
				}
				for _, p := range sum.DataPoints {
					if v, ok := p.Attributes.Value("outcome"); ok {
						counts[m.Name+":"+v.AsString()] += p.Value
					}
				}
			case MetricCheckpoints + ".duration", MetricRestores + ".duration":
				if _, ok := m.Data.(metricdata.Histogram[float64]); ok {
					durations[m.Name] = true
				}
			}
		}
	}
	if counts[MetricCheckpoints+":ok"] != 1 {
		t.Errorf("checkpoint counts = %v, want one ok", counts)
	}
	if counts[MetricCheckpoints+":too_large"] != 1 {
		t.Errorf("checkpoint counts = %v, want one too_large", counts)
	}
	if counts[MetricRestores+":error"] != 1 {
		t.Errorf("restore counts = %v, want one error", counts)
	}
	for _, name := range []string{MetricCheckpoints + ".duration", MetricRestores + ".duration"} {
		if !durations[name] {
			t.Errorf("no %s histogram recorded", name)
		}
	}
}
