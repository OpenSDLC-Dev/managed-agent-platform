package events_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func collectMetrics(t *testing.T) func() metricdata.ResourceMetrics {
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

func floatPoints(rm metricdata.ResourceMetrics, name string) []metricdata.HistogramDataPoint[float64] {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				if h, ok := m.Data.(metricdata.Histogram[float64]); ok {
					return h.DataPoints
				}
			}
		}
	}
	return nil
}

func intPoints(rm metricdata.ResourceMetrics, name string) []metricdata.HistogramDataPoint[int64] {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				if h, ok := m.Data.(metricdata.Histogram[int64]); ok {
					return h.DataPoints
				}
			}
		}
	}
	return nil
}

func attrValue(set []metricdata.HistogramDataPoint[int64], i int, key string) string {
	for _, kv := range set[i].Attributes.ToSlice() {
		if string(kv.Key) == key {
			return kv.Value.Emit()
		}
	}
	return ""
}

// A model turn's duration and token usage ride the same instrumentation point
// as its span and its span.* wire events (CLAUDE.md principle 3), so the three
// views of one turn cannot disagree. The names are OTel's GenAI semantic
// conventions rather than ours: a turn IS a client call to a GenAI provider,
// which is exactly what those instruments describe.
func TestModelRequestRecordsGenAIMetrics(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()
	collect := collectMetrics(t)

	backend := events.Backend{Provider: "anthropic", Model: "claude-x"}
	_, mr, err := log.StartModelRequest(ctx, sid, backend)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := mr.EndEvent(false, &domain.ModelUsage{InputTokens: 11, OutputTokens: 7}); err != nil {
		t.Fatalf("end event: %v", err)
	}
	mr.Finish(ctx, false, nil)

	rm := collect()

	dur := floatPoints(rm, "gen_ai.client.operation.duration")
	if len(dur) != 1 {
		t.Fatalf("gen_ai.client.operation.duration points = %d, want 1", len(dur))
	}
	if dur[0].Sum <= 0 {
		t.Errorf("duration sum = %v, want positive", dur[0].Sum)
	}
	want := map[string]string{
		"gen_ai.operation.name": "chat",
		"gen_ai.provider.name":  "anthropic",
		"gen_ai.request.model":  "claude-x",
	}
	got := map[string]string{}
	for _, kv := range dur[0].Attributes.ToSlice() {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("duration attr %s = %q, want %q", k, got[k], w)
		}
	}
	if _, ok := got["error.type"]; ok {
		t.Errorf("error.type present on a clean turn: %v", got)
	}

	tok := intPoints(rm, "gen_ai.client.token.usage")
	if len(tok) != 2 {
		t.Fatalf("gen_ai.client.token.usage points = %d, want 2 (input and output)", len(tok))
	}
	byType := map[string]int64{}
	for i := range tok {
		byType[attrValue(tok, i, "gen_ai.token.type")] = tok[i].Sum
	}
	if byType["input"] != 11 {
		t.Errorf("input tokens = %d, want 11", byType["input"])
	}
	if byType["output"] != 7 {
		t.Errorf("output tokens = %d, want 7", byType["output"])
	}
}

// A turn that failed is the one worth finding in a dashboard, so it must be
// distinguishable — and it has no usage to report, which must not surface as a
// pair of zero-token readings diluting the real ones.
//
// This drives the brain's real failure ordering: commitFailure renders an end
// event before failTurn finishes the span. The wire schema still wants a
// model_usage object on that event, so it carries zeroes — and those zeroes are
// exactly what must not reach the token histogram. Finishing with no end event
// at all is an ordering the brain never uses while a span is live, so asserting
// on it would leave this gate untested on the only path it exists for.
func TestModelRequestRecordsFailureWithoutTokens(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()
	collect := collectMetrics(t)

	_, mr, err := log.StartModelRequest(ctx, sid, events.Backend{Provider: "openai", Model: "gpt-x"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ev, err := mr.EndEvent(true, nil)
	if err != nil {
		t.Fatalf("end event: %v", err)
	}
	// The wire event still reports a usage object whatever the metric does —
	// the zeroes belong on the log, just not in the histogram.
	if !bytes.Contains(ev.Payload, []byte(`"model_usage"`)) {
		t.Errorf("end event dropped model_usage: %s", ev.Payload)
	}
	mr.Finish(ctx, true, nil)

	rm := collect()
	dur := floatPoints(rm, "gen_ai.client.operation.duration")
	if len(dur) != 1 {
		t.Fatalf("duration points = %d, want 1", len(dur))
	}
	var errType string
	for _, kv := range dur[0].Attributes.ToSlice() {
		if string(kv.Key) == "error.type" {
			errType = kv.Value.Emit()
		}
	}
	if errType == "" {
		t.Error("a failed turn recorded no error.type, so it is invisible in the metric")
	}
	if pts := intPoints(rm, "gen_ai.client.token.usage"); len(pts) != 0 {
		t.Errorf("token usage points = %d on a turn that never reported usage, want 0", len(pts))
	}
}

// gen_ai.client.operation.duration is the convention's measure of the client's
// call to the model provider. The brain finishes a span only after settling the
// turn — a session-locked Postgres transaction the model had nothing to do with
// — so measuring to Finish would file database contention under a model-latency
// instrument, and would do it only on the paths that settle, leaving the metric
// inconsistent with itself. ModelDone stops the clock at the real boundary.
func TestModelRequestDurationExcludesSettlement(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()
	collect := collectMetrics(t)

	_, mr, err := log.StartModelRequest(ctx, sid, events.Backend{Provider: "anthropic", Model: "claude-x"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	mr.ModelDone() // the stream ended here
	settle := 60 * time.Millisecond
	time.Sleep(settle) // a slow settlement transaction
	if _, err := mr.EndEvent(false, &domain.ModelUsage{InputTokens: 1, OutputTokens: 1}); err != nil {
		t.Fatalf("end event: %v", err)
	}
	mr.Finish(ctx, false, nil)

	dur := floatPoints(collect(), "gen_ai.client.operation.duration")
	if len(dur) != 1 {
		t.Fatalf("duration points = %d, want 1", len(dur))
	}
	if dur[0].Sum >= settle.Seconds() {
		t.Errorf("duration = %vs, which includes the %v settlement — the metric is reporting database time as model latency",
			dur[0].Sum, settle)
	}
}

// A turn that died before the stream ever finished never marks a model
// boundary. It still has a duration worth recording — the attempt — so the
// measure must fall back to the request's own elapsed rather than report zero.
func TestModelRequestDurationFallsBackWhenTheStreamNeverEnded(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()
	collect := collectMetrics(t)

	_, mr, err := log.StartModelRequest(ctx, sid, events.Backend{Provider: "anthropic", Model: "claude-x"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	mr.Finish(ctx, true, nil) // no ModelDone: the turn was abandoned mid-stream

	dur := floatPoints(collect(), "gen_ai.client.operation.duration")
	if len(dur) != 1 {
		t.Fatalf("duration points = %d, want 1", len(dur))
	}
	if dur[0].Sum <= 0 {
		t.Errorf("duration = %v on an abandoned turn, want the elapsed attempt", dur[0].Sum)
	}
}
