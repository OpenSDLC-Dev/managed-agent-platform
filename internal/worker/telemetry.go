package worker

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName is the worker's OTel metrics scope. The instruments mirror the
// executor's skills.materialize.* by name — same materialization, two
// deployment points — distinguished by this scope.
const meterName = "github.com/OpenSDLC-Dev/managed-agent-platform/internal/worker"

const (
	// MetricSkillsMaterialized counts per-skill materialization outcomes.
	MetricSkillsMaterialized = "skills.materialized"
	// MetricSkillsMaterializeDuration is one whole materialization pass.
	MetricSkillsMaterializeDuration = "skills.materialize.duration"
)

// Bounded outcome values — skill ids never label metrics.
const (
	skillOutcomeOK       = "ok"
	skillOutcomeNotFound = "not_found"
	skillOutcomeFailed   = "failed"
)

// recordSkillMaterialized counts one skill's outcome; the meter is resolved
// per call (internal/toolset's rationale) and a metrics failure never fails
// the run.
func recordSkillMaterialized(ctx context.Context, outcome string) {
	counter, err := otel.GetMeterProvider().Meter(meterName).Int64Counter(
		MetricSkillsMaterialized,
		metric.WithDescription("Skills materialized into sandboxes, by outcome."))
	if err != nil {
		return
	}
	counter.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// recordSkillsMaterializeDuration records one materialization pass.
func recordSkillsMaterializeDuration(ctx context.Context, d time.Duration) {
	hist, err := otel.GetMeterProvider().Meter(meterName).Float64Histogram(
		MetricSkillsMaterializeDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of a session's skills-materialization pass."))
	if err != nil {
		return
	}
	hist.Record(ctx, d.Seconds())
}
