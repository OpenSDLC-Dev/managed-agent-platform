package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// The span.* wire events and the OTel span must come from the same
// instrumentation point (CLAUDE.md principle 3): one StartModelRequest/End
// pair yields exactly one exported OTel span AND the start/end event pair,
// linked by model_request_start_id.
func TestModelRequestSameSourceEmission(t *testing.T) {
	pool := newPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = tp.Shutdown(ctx) }()

	// Route the helper through the recording provider via the global, as
	// production wiring does.
	restore := swapTracerProvider(tp)
	defer restore()

	spanCtx, mr, err := log.StartModelRequest(ctx, sid)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !oteltrace.SpanContextFromContext(spanCtx).IsValid() {
		t.Error("returned context carries no span")
	}
	speed := "standard"
	if err := mr.End(spanCtx, true, domain.ModelUsage{
		InputTokens: 100, OutputTokens: 25, CacheReadInputTokens: 7, Speed: &speed,
	}); err != nil {
		t.Fatalf("end: %v", err)
	}

	// Exactly one OTel span left the process.
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	if spans[0].Name() != "model_request" {
		t.Errorf("span name = %q", spans[0].Name())
	}

	// And exactly the two wire events landed in the log, linked.
	list, err := log.List(ctx, sid, events.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("log has %d events, want start+end", len(list))
	}
	start, end := list[0], list[1]
	if start.Type != domain.EventSpanModelRequestStart || end.Type != domain.EventSpanModelRequestEnd {
		t.Fatalf("event types = %s, %s", start.Type, end.Type)
	}
	if start.ProcessedAt == nil || end.ProcessedAt == nil {
		t.Error("span events must carry processed_at")
	}
	if mr.StartEventID() != start.ID {
		t.Errorf("StartEventID = %s, want %s", mr.StartEventID(), start.ID)
	}

	var payload struct {
		IsError             bool   `json:"is_error"`
		ModelRequestStartID string `json:"model_request_start_id"`
		ModelUsage          struct {
			InputTokens              int64   `json:"input_tokens"`
			OutputTokens             int64   `json:"output_tokens"`
			CacheReadInputTokens     int64   `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64   `json:"cache_creation_input_tokens"`
			Speed                    *string `json:"speed"`
		} `json:"model_usage"`
	}
	if err := json.Unmarshal(end.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.IsError {
		t.Error("is_error not recorded")
	}
	if payload.ModelRequestStartID != start.ID.String() {
		t.Errorf("model_request_start_id = %q, want %q", payload.ModelRequestStartID, start.ID)
	}
	if payload.ModelUsage.InputTokens != 100 || payload.ModelUsage.OutputTokens != 25 ||
		payload.ModelUsage.CacheReadInputTokens != 7 || payload.ModelUsage.CacheCreationInputTokens != 0 {
		t.Errorf("model_usage = %+v", payload.ModelUsage)
	}
	if payload.ModelUsage.Speed == nil || *payload.ModelUsage.Speed != "standard" {
		t.Errorf("speed = %v", payload.ModelUsage.Speed)
	}

	// Failure path: a start against a dead session emits no span leak.
	if _, _, err := log.StartModelRequest(ctx, domain.NewID("sesn")); err == nil {
		t.Error("start on unknown session should fail")
	}
}
