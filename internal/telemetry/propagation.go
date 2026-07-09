package telemetry

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/propagation"
)

// propagator is fixed to W3C trace context so Inject/Extract behave
// identically whether or not Init has run (e.g. in a BYOC worker that does
// its own OTel setup).
var propagator = propagation.TraceContext{}

// Inject writes the trace context from ctx into carrier as W3C
// traceparent / tracestate entries. carrier is any string map — HTTP
// headers and work-item metadata both flatten to this shape.
func Inject(ctx context.Context, carrier map[string]string) {
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
