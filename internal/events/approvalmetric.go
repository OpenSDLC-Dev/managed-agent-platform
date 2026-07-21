package events

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// MetricApprovalWait is the human-in-the-loop approval latency histogram: how
// long a session sat suspended on requires_action before a confirmation cleared
// the gate. Exported so the telemetry contract test can assert this exact name
// reaches an OTLP collector.
const MetricApprovalWait = "approval.wait.duration"

// RecordApprovalWait records one approval wait, in seconds. The interval is
// measured in the database (clock_timestamp() minus the requires_action idle
// event's created_at) so both ends read the same clock, and recorded here after
// the resuming transaction commits — a confirmation whose commit rolled back did
// not resume anything.
//
// It resolves the meter per call rather than caching an instrument that would pin
// whichever MeterProvider was installed first, and never fails the caller: a
// telemetry error just drops the reading.
func RecordApprovalWait(ctx context.Context, seconds float64) {
	hist, err := otel.GetMeterProvider().Meter(meterName).Float64Histogram(
		MetricApprovalWait,
		metric.WithUnit("s"),
		metric.WithDescription("Time a session waited on a requires_action approval gate before it resumed."))
	if err != nil {
		return
	}
	hist.Record(ctx, seconds)
}
