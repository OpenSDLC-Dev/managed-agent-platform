package telemetry

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// propagator is fixed to W3C trace context: traceparent/tracestate is this
// platform's cross-process contract (HTTP request/response headers and a work
// item's stored trace context alike), so Inject/Extract behave identically
// whether or not Init has run and regardless of any richer propagator an
// embedding process installs for its own outbound calls.
var propagator = propagation.TraceContext{}

// Inject writes the trace context from ctx into carrier as W3C
// traceparent / tracestate entries, replacing existing entries in any key
// casing. carrier is any string map — HTTP headers and a work item's stored
// trace context both flatten to this shape. A nil carrier, or a ctx without a
// span context, leaves everything untouched.
func Inject(ctx context.Context, carrier map[string]string) {
	if carrier == nil || !trace.SpanContextFromContext(ctx).IsValid() {
		return
	}
	// Drop stale entries first (canonically-cased ones included, e.g.
	// "Traceparent" from flattened net/http headers) so the fresh context
	// is the only one left — two case-variant entries would make Extract's
	// winner depend on map iteration order.
	for k := range carrier {
		switch strings.ToLower(k) {
		case "traceparent", "tracestate":
			delete(carrier, k)
		}
	}
	propagator.Inject(ctx, propagation.MapCarrier(carrier))
}

// Extract returns a copy of ctx carrying the remote span context found in
// carrier, or ctx unchanged when carrier holds no valid traceparent.
func Extract(ctx context.Context, carrier map[string]string) context.Context {
	// W3C header names are case-insensitive, and maps flattened from
	// net/http headers arrive canonically cased ("Traceparent"), while
	// MapCarrier looks up verbatim — so match on lowered keys.
	lowered := make(map[string]string, len(carrier))
	for k, v := range carrier {
		lowered[strings.ToLower(k)] = v
	}
	return propagator.Extract(ctx, propagation.MapCarrier(lowered))
}
