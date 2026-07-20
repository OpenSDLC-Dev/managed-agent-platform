package telemetry_test

import (
	"context"
	"encoding/hex"
	"net"
	"sync"
	"testing"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
)

// fakeCollector is an in-process OTLP/gRPC collector that records every
// exported span, metric and log record, so tests can assert on what actually
// left the process.
type fakeCollector struct {
	traces  fakeTraceService
	metrics fakeMetricService
	logs    fakeLogService
	addr    string
}

type fakeTraceService struct {
	collectortrace.UnimplementedTraceServiceServer
	mu    sync.Mutex
	spans []*tracepb.ResourceSpans
}

func (s *fakeTraceService) Export(_ context.Context, req *collectortrace.ExportTraceServiceRequest) (*collectortrace.ExportTraceServiceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spans = append(s.spans, req.GetResourceSpans()...)
	return &collectortrace.ExportTraceServiceResponse{}, nil
}

type fakeMetricService struct {
	collectormetrics.UnimplementedMetricsServiceServer
	mu      sync.Mutex
	metrics []*metricspb.ResourceMetrics
	// stall, when non-nil, blocks every export until it is closed or the
	// caller's context ends — a collector that is up and accepting connections
	// but not answering, which is what exhausts a shared shutdown deadline.
	stall chan struct{}
}

func (s *fakeMetricService) Export(ctx context.Context, req *collectormetrics.ExportMetricsServiceRequest) (*collectormetrics.ExportMetricsServiceResponse, error) {
	if s.stall != nil {
		select {
		case <-s.stall:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics = append(s.metrics, req.GetResourceMetrics()...)
	return &collectormetrics.ExportMetricsServiceResponse{}, nil
}

type fakeLogService struct {
	collectorlogs.UnimplementedLogsServiceServer
	mu   sync.Mutex
	logs []*logspb.ResourceLogs
}

func (s *fakeLogService) Export(_ context.Context, req *collectorlogs.ExportLogsServiceRequest) (*collectorlogs.ExportLogsServiceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, req.GetResourceLogs()...)
	return &collectorlogs.ExportLogsServiceResponse{}, nil
}

func startFakeCollector(t *testing.T) *fakeCollector {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	c := &fakeCollector{addr: lis.Addr().String()}
	srv := grpc.NewServer()
	collectortrace.RegisterTraceServiceServer(srv, &c.traces)
	collectormetrics.RegisterMetricsServiceServer(srv, &c.metrics)
	collectorlogs.RegisterLogsServiceServer(srv, &c.logs)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return c
}

// stallMetrics makes the collector accept metric exports and never answer them,
// for the duration of one test. Used to prove the shutdown drains logs before
// spending its deadline on anything else.
func (c *fakeCollector) stallMetrics(t *testing.T) {
	t.Helper()
	stall := make(chan struct{})
	c.metrics.stall = stall
	t.Cleanup(func() { close(stall) })
}

// spanNames flattens every received span name across resource/scope nesting.
func (c *fakeCollector) spanNames() []string {
	c.traces.mu.Lock()
	defer c.traces.mu.Unlock()
	var names []string
	for _, rs := range c.traces.spans {
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				names = append(names, s.GetName())
			}
		}
	}
	return names
}

// resourceAttr returns the value of a resource attribute on received spans,
// or "" if absent.
func (c *fakeCollector) resourceAttr(key string) string {
	c.traces.mu.Lock()
	defer c.traces.mu.Unlock()
	for _, rs := range c.traces.spans {
		for _, kv := range rs.GetResource().GetAttributes() {
			if kv.GetKey() == key {
				return kv.GetValue().GetStringValue()
			}
		}
	}
	return ""
}

// logRecord is one exported log record, flattened to what the tests assert on.
type logRecord struct {
	body     string
	severity string
	attrs    map[string]string
	traceID  string
	spanID   string
}

// logRecords flattens every received log record across resource/scope nesting.
func (c *fakeCollector) logRecords() []logRecord {
	c.logs.mu.Lock()
	defer c.logs.mu.Unlock()
	var out []logRecord
	for _, rl := range c.logs.logs {
		for _, sl := range rl.GetScopeLogs() {
			for _, r := range sl.GetLogRecords() {
				rec := logRecord{
					body:     r.GetBody().GetStringValue(),
					severity: r.GetSeverityText(),
					attrs:    map[string]string{},
					traceID:  hex.EncodeToString(r.GetTraceId()),
					spanID:   hex.EncodeToString(r.GetSpanId()),
				}
				for _, kv := range r.GetAttributes() {
					rec.attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
				}
				out = append(out, rec)
			}
		}
	}
	return out
}

// logBodies flattens every received log record's body.
func (c *fakeCollector) logBodies() []string {
	var bodies []string
	for _, r := range c.logRecords() {
		bodies = append(bodies, r.body)
	}
	return bodies
}

// metricNames flattens every received metric name.
func (c *fakeCollector) metricNames() []string {
	c.metrics.mu.Lock()
	defer c.metrics.mu.Unlock()
	var names []string
	for _, rm := range c.metrics.metrics {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				names = append(names, m.GetName())
			}
		}
	}
	return names
}
