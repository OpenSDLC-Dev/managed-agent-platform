package brain

import (
	"context"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName is this package's OTel instrumentation scope.
const meterName = "github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"

// MetricTimeToFirstToken is the time-to-first-token histogram: the clock starts
// when the brain claims the model turn — replay and request assembly are latency
// the user feels — and stops at the first content the model streams. It is the
// brain's own metric, not a gen_ai.* one, because the interval it measures spans
// more than the provider call. Exported so the telemetry contract test can assert
// this exact name reaches an OTLP collector.
const MetricTimeToFirstToken = "model.time_to_first_token"

// recordTTFT records one turn's time to first token. Like the execution-chain
// metrics, it resolves the meter per turn rather than caching an instrument that
// would pin whichever MeterProvider was installed first, and it never fails a
// turn: a telemetry error just drops the reading, which the event log does not
// depend on. The caller records nothing for a turn that streamed no content —
// there is no first token to measure, and a zero would report an instant
// response that never happened.
func recordTTFT(ctx context.Context, backend events.Backend, d time.Duration) {
	hist, err := otel.GetMeterProvider().Meter(meterName).Float64Histogram(
		MetricTimeToFirstToken,
		metric.WithUnit("s"),
		metric.WithDescription("Time from claiming a model turn to the model's first streamed token."))
	if err != nil {
		return
	}
	hist.Record(ctx, d.Seconds(), metric.WithAttributes(
		attribute.String("gen_ai.provider.name", backend.Provider),
		attribute.String("gen_ai.request.model", backend.Model),
	))
}
