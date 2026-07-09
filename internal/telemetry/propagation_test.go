package telemetry_test

import (
	"context"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
)

func TestInjectWritesW3CTraceparent(t *testing.T) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19},
		SpanID:     trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	carrier := map[string]string{}
	telemetry.Inject(ctx, carrier)

	want := fmt.Sprintf("00-%s-%s-01", sc.TraceID(), sc.SpanID())
	if got := carrier["traceparent"]; got != want {
		t.Errorf("carrier[traceparent] = %q, want %q", got, want)
	}
}

func TestInjectExtractRoundTrip(t *testing.T) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a},
		SpanID:     trace.SpanID{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88},
		TraceFlags: trace.FlagsSampled,
		TraceState: mustTraceState(t, "vendor=value"),
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	carrier := map[string]string{}
	telemetry.Inject(ctx, carrier)

	got := trace.SpanContextFromContext(telemetry.Extract(context.Background(), carrier))
	if got.TraceID() != sc.TraceID() {
		t.Errorf("TraceID = %s, want %s", got.TraceID(), sc.TraceID())
	}
	if got.SpanID() != sc.SpanID() {
		t.Errorf("SpanID = %s, want %s", got.SpanID(), sc.SpanID())
	}
	if !got.IsSampled() {
		t.Errorf("sampled flag lost in round trip")
	}
	if !got.IsRemote() {
		t.Errorf("extracted context must be marked remote")
	}
	if got.TraceState().String() != "vendor=value" {
		t.Errorf("TraceState = %q, want %q", got.TraceState().String(), "vendor=value")
	}
}

func TestInjectNilCarrierDoesNotPanic(t *testing.T) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
	})
	// A nil carrier (e.g. a work item whose metadata decoded from JSON as
	// null) must be a no-op, not a nil-map-write panic.
	telemetry.Inject(trace.ContextWithSpanContext(context.Background(), sc), nil)
}

func TestInjectReplacesStaleCaseVariantEntries(t *testing.T) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19},
		SpanID:     trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		TraceFlags: trace.FlagsSampled,
	})
	// A forwarding hop reuses a carrier flattened from net/http headers:
	// stale upstream entries arrive canonically cased. Inject must replace
	// them, not leave two case-variant traceparent entries whose winner
	// depends on map iteration order at Extract time.
	carrier := map[string]string{
		"Traceparent": "00-99999999999999999999999999999999-9999999999999999-01",
		"Tracestate":  "up=stale",
		"X-Custom":    "kept",
	}
	telemetry.Inject(trace.ContextWithSpanContext(context.Background(), sc), carrier)

	want := fmt.Sprintf("00-%s-%s-01", sc.TraceID(), sc.SpanID())
	if got := carrier["traceparent"]; got != want {
		t.Errorf("carrier[traceparent] = %q, want %q", got, want)
	}
	if _, ok := carrier["Traceparent"]; ok {
		t.Errorf("stale Traceparent entry survived Inject")
	}
	if _, ok := carrier["Tracestate"]; ok {
		t.Errorf("stale Tracestate entry survived Inject")
	}
	if v, ok := carrier["tracestate"]; ok {
		t.Errorf("tracestate = %q, want absent (fresh context carries none)", v)
	}
	if carrier["X-Custom"] != "kept" {
		t.Errorf("unrelated carrier entries must be preserved")
	}
}

func TestInjectWithoutSpanContextLeavesCarrierUntouched(t *testing.T) {
	carrier := map[string]string{
		"Traceparent": "00-aabbccddeeff0102030405060708090a-1122334455667788-01",
	}
	telemetry.Inject(context.Background(), carrier)
	if got := carrier["Traceparent"]; got != "00-aabbccddeeff0102030405060708090a-1122334455667788-01" {
		t.Errorf("passthrough entry modified: %q", got)
	}
	if len(carrier) != 1 {
		t.Errorf("carrier = %v, want single passthrough entry", carrier)
	}
}

func TestExtractIsCaseInsensitive(t *testing.T) {
	// Maps flattened from net/http headers carry Go's canonical key form;
	// W3C header names are case-insensitive.
	carrier := map[string]string{
		"Traceparent": "00-aabbccddeeff0102030405060708090a-1122334455667788-01",
		"Tracestate":  "vendor=value",
	}
	sc := trace.SpanContextFromContext(telemetry.Extract(context.Background(), carrier))
	if !sc.IsValid() {
		t.Fatalf("Extract missed canonically-cased Traceparent key")
	}
	if got, want := sc.TraceID().String(), "aabbccddeeff0102030405060708090a"; got != want {
		t.Errorf("TraceID = %s, want %s", got, want)
	}
	if sc.TraceState().String() != "vendor=value" {
		t.Errorf("TraceState = %q, want %q", sc.TraceState().String(), "vendor=value")
	}
}

func TestExtractWithoutTraceparentIsInvalid(t *testing.T) {
	ctx := telemetry.Extract(context.Background(), map[string]string{})
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		t.Errorf("extract from empty carrier produced valid span context %+v", sc)
	}
}

func mustTraceState(t *testing.T, raw string) trace.TraceState {
	t.Helper()
	ts, err := trace.ParseTraceState(raw)
	if err != nil {
		t.Fatalf("ParseTraceState(%q): %v", raw, err)
	}
	return ts
}
