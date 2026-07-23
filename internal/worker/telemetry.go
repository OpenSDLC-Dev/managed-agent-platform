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
	// MetricFilesMaterialized counts per-file mount materialization outcomes —
	// the executor twin's name on the worker meter.
	MetricFilesMaterialized = "files.materialized"
	// MetricFilesMaterializeDuration is one whole file-materialization pass.
	MetricFilesMaterializeDuration = "files.materialize.duration"
)

// Bounded outcome values — skill and file ids never label metrics.
const (
	skillOutcomeOK       = "ok"
	skillOutcomeNotFound = "not_found"
	skillOutcomeFailed   = "failed"
	fileOutcomeOK        = "ok"
	fileOutcomeNotFound  = "not_found"
	fileOutcomeFailed    = "failed"
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

// recordFileMaterialized counts one file mount's outcome — the skills twin.
func recordFileMaterialized(ctx context.Context, outcome string) {
	counter, err := otel.GetMeterProvider().Meter(meterName).Int64Counter(
		MetricFilesMaterialized,
		metric.WithDescription("File mounts materialized into sandboxes, by outcome."))
	if err != nil {
		return
	}
	counter.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// recordFilesMaterializeDuration records one file-materialization pass.
func recordFilesMaterializeDuration(ctx context.Context, d time.Duration) {
	hist, err := otel.GetMeterProvider().Meter(meterName).Float64Histogram(
		MetricFilesMaterializeDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of a session's file-materialization pass."))
	if err != nil {
		return
	}
	hist.Record(ctx, d.Seconds())
}
