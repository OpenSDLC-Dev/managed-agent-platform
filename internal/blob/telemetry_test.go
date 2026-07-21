package blob_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
)

// fake is an in-memory Store for exercising the decorator without a backend.
// It lives in the test only: the platform has no memory backend, and growing
// one here would be scope creep the contract suite doesn't ask for.
type fake struct {
	objects map[string][]byte
	fail    error // returned from every op when set
}

func newFake() *fake { return &fake{objects: map[string][]byte{}} }

func (f *fake) Put(_ context.Context, key string, r io.Reader, size int64, _ string) error {
	if f.fail != nil {
		return f.fail
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.objects[key] = data
	return nil
}

func (f *fake) Get(_ context.Context, key string) (io.ReadCloser, int64, error) {
	if f.fail != nil {
		return nil, 0, f.fail
	}
	data, ok := f.objects[key]
	if !ok {
		return nil, 0, blob.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

func (f *fake) Delete(_ context.Context, key string) error {
	if f.fail != nil {
		return f.fail
	}
	delete(f.objects, key)
	return nil
}

// collect installs a manual-reader meter provider for the test's duration and
// returns a collect func; the global provider is restored after (the
// internal/toolset pattern).
func collect(t *testing.T) func() metricdata.ResourceMetrics {
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

func findMetric(rm metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

// durationPoint returns the single blob.op.duration data point matching op and
// asserts its outcome attribute.
func wantDuration(t *testing.T, rm metricdata.ResourceMetrics, op, outcome string) {
	t.Helper()
	m, ok := findMetric(rm, blob.MetricOpDuration)
	if !ok {
		t.Fatalf("%s not recorded", blob.MetricOpDuration)
	}
	h, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("%s is %T, want float64 histogram", blob.MetricOpDuration, m.Data)
	}
	for _, dp := range h.DataPoints {
		attrs := map[string]string{}
		for _, kv := range dp.Attributes.ToSlice() {
			attrs[string(kv.Key)] = kv.Value.Emit()
		}
		if attrs["blob.op"] == op {
			if attrs["outcome"] != outcome {
				t.Errorf("op %s outcome = %q, want %q", op, attrs["outcome"], outcome)
			}
			if dp.Count != 1 {
				t.Errorf("op %s count = %d, want 1", op, dp.Count)
			}
			return
		}
	}
	t.Errorf("no %s data point for op %q", blob.MetricOpDuration, op)
}

// wantBytes asserts the blob.op.bytes point for op sums to total; want < 0
// asserts no point exists for that op.
func wantBytes(t *testing.T, rm metricdata.ResourceMetrics, op string, want int64) {
	t.Helper()
	m, ok := findMetric(rm, blob.MetricOpBytes)
	if !ok {
		if want >= 0 {
			t.Fatalf("%s not recorded", blob.MetricOpBytes)
		}
		return
	}
	h, ok := m.Data.(metricdata.Histogram[int64])
	if !ok {
		t.Fatalf("%s is %T, want int64 histogram", blob.MetricOpBytes, m.Data)
	}
	for _, dp := range h.DataPoints {
		for _, kv := range dp.Attributes.ToSlice() {
			if string(kv.Key) == "blob.op" && kv.Value.Emit() == op {
				if want < 0 {
					t.Errorf("unexpected %s point for op %q", blob.MetricOpBytes, op)
				} else if dp.Sum != want {
					t.Errorf("op %s bytes sum = %d, want %d", op, dp.Sum, want)
				}
				return
			}
		}
	}
	if want >= 0 {
		t.Errorf("no %s data point for op %q", blob.MetricOpBytes, op)
	}
}

func TestMetricsRecordSuccessfulOps(t *testing.T) {
	c := collect(t)
	s := blob.WithMetrics(newFake())
	ctx := context.Background()

	content := "twelve bytes"
	if err := s.Put(ctx, "k", strings.NewReader(content), int64(len(content)), "text/plain"); err != nil {
		t.Fatalf("put: %v", err)
	}
	rc, size, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != content || size != int64(len(content)) {
		t.Fatalf("decorator altered behavior: content=%q size=%d", data, size)
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	rm := c()
	wantDuration(t, rm, "put", "ok")
	wantDuration(t, rm, "get", "ok")
	wantDuration(t, rm, "delete", "ok")
	wantBytes(t, rm, "put", int64(len(content)))
	wantBytes(t, rm, "get", int64(len(content)))
	wantBytes(t, rm, "delete", -1) // deletes carry no payload; absent, not zero
}

func TestMetricsRecordNotFoundOutcome(t *testing.T) {
	c := collect(t)
	s := blob.WithMetrics(newFake())

	if _, _, err := s.Get(context.Background(), "missing"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("get missing through decorator = %v, want blob.ErrNotFound", err)
	}

	rm := c()
	wantDuration(t, rm, "get", "not_found")
	wantBytes(t, rm, "get", -1) // a failed get records no payload size
}

func TestMetricsRecordErrorOutcome(t *testing.T) {
	c := collect(t)
	f := newFake()
	f.fail = errors.New("backend down")
	s := blob.WithMetrics(f)
	ctx := context.Background()

	if err := s.Put(ctx, "k", strings.NewReader("x"), 1, ""); err == nil {
		t.Fatal("decorator swallowed the backend error")
	}
	if err := s.Delete(ctx, "k"); err == nil {
		t.Fatal("decorator swallowed the backend error")
	}

	rm := c()
	wantDuration(t, rm, "put", "error")
	wantDuration(t, rm, "delete", "error")
	wantBytes(t, rm, "put", -1) // a failed put records no payload size
}
