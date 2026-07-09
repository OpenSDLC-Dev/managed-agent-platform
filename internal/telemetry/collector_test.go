package telemetry_test

import (
	"context"
	"net"
	"sync"
	"testing"

	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
)

// fakeCollector is an in-process OTLP/gRPC collector that records every
// exported span and metric, so tests can assert on what actually left the
// process.
type fakeCollector struct {
	traces  fakeTraceService
	metrics fakeMetricService
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
}

func (s *fakeMetricService) Export(_ context.Context, req *collectormetrics.ExportMetricsServiceRequest) (*collectormetrics.ExportMetricsServiceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics = append(s.metrics, req.GetResourceMetrics()...)
	return &collectormetrics.ExportMetricsServiceResponse{}, nil
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
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return c
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
