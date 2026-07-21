package events

import (
	"context"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// MetricSessionStatus counts session status transitions, keyed by the status a
// session moved into (session.status attribute). It is exported so the telemetry
// contract test can assert this exact name reaches an OTLP collector.
const MetricSessionStatus = "session.status.transitions"

// RecordSessionStatus counts one session status transition. Callers invoke it
// AFTER the transaction that moved sessions.status has committed, never before: a
// status change that rolled back — a lost lease, an aborted settle — did not
// happen, and counting the attempt would inflate the metric on exactly the infra
// churn an operator is trying to read. The status column is written in one place
// (AppendInTx's SetStatus), but committed in several, so the recording lives at
// each commit site rather than beside the write.
//
// It resolves the meter per call rather than caching an instrument that would pin
// whichever MeterProvider was installed first, and never fails the caller: a
// telemetry error just drops the count.
func RecordSessionStatus(ctx context.Context, status domain.SessionStatus) {
	counter, err := otel.GetMeterProvider().Meter(meterName).Int64Counter(
		MetricSessionStatus,
		metric.WithUnit("{transition}"),
		metric.WithDescription("Session status transitions, counted by the status entered."))
	if err != nil {
		return
	}
	counter.Add(ctx, 1, metric.WithAttributes(attribute.String("session.status", string(status))))
}
