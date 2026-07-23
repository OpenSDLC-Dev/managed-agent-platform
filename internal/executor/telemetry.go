package executor

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName is the executor's OTel metrics scope. Tool timing deliberately
// lives in internal/toolset (the same instrument serves cloud and BYOC); the
// executor's own instruments cover what only the platform half does —
// materializing skills into sandboxes.
const meterName = "github.com/OpenSDLC-Dev/managed-agent-platform/internal/executor"

const (
	// MetricSkillsMaterialized counts per-skill materialization outcomes.
	MetricSkillsMaterialized = "skills.materialized"
	// MetricSkillsMaterializeDuration is one whole materialization pass.
	MetricSkillsMaterializeDuration = "skills.materialize.duration"
	// MetricFilesMaterialized counts per-file mount materialization outcomes.
	MetricFilesMaterialized = "files.materialized"
	// MetricFilesMaterializeDuration is one whole file-materialization pass.
	MetricFilesMaterializeDuration = "files.materialize.duration"
)

// Bounded outcome values — skill/file ids never label metrics (cardinality
// rule: ids go in logs and span attributes).
const (
	skillOutcomeOK       = "ok"
	skillOutcomeNotFound = "not_found"
	skillOutcomeFailed   = "failed"
	// corrupt: the archive did not match the digest recorded at upload —
	// separable from an ordinary miss because it means storage corruption or
	// substitution, not a dangling reference.
	skillOutcomeCorrupt = "corrupt"

	fileOutcomeOK       = "ok"
	fileOutcomeNotFound = "not_found"
	fileOutcomeFailed   = "failed"
)

// recordSkillMaterialized counts one skill's outcome. The meter is resolved
// per call, like internal/toolset's, so telemetry rewiring in tests never
// pins a dead provider; a metrics failure never fails the run.
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

// recordFileMaterialized counts one file mount's outcome, mirroring the skills
// recorder (per-call meter resolution, telemetry failure never fails the run).
func recordFileMaterialized(ctx context.Context, outcome string) {
	counter, err := otel.GetMeterProvider().Meter(meterName).Int64Counter(
		MetricFilesMaterialized,
		metric.WithDescription("Files materialized into sandboxes, by outcome."))
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
