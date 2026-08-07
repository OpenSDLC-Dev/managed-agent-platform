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
	// MetricReposMaterialized counts per-repository materialization outcomes.
	MetricReposMaterialized = "repos.materialized"
	// MetricReposMaterializeDuration is one whole repo-materialization pass.
	MetricReposMaterializeDuration = "repos.materialize.duration"
	// MetricReposMaterializeBytes is one landed repository's shipped size.
	MetricReposMaterializeBytes = "repos.materialize.bytes"
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

	// The repository outcomes are the clone-failure reasons the session.error
	// variant carries (plan 25 decision 4), plus ok and the probe's skip.
	// too_large and timeout are the platform's own budgets, with no wire twin.
	repoOutcomeOK        = "ok"
	repoOutcomeUnchanged = "unchanged"
	repoOutcomeAuth      = "auth"
	repoOutcomeNotFound  = "not_found"
	repoOutcomeNetwork   = "network"
	repoOutcomeCheckout  = "checkout"
	repoOutcomeTooLarge  = "too_large"
	repoOutcomeTimeout   = "timeout"
	repoOutcomeInternal  = "internal"
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

// recordRepoMaterialized counts one repository mount's outcome — ok, the
// probe's unchanged skip, or one of the clone-failure reasons.
func recordRepoMaterialized(ctx context.Context, outcome string) {
	counter, err := otel.GetMeterProvider().Meter(meterName).Int64Counter(
		MetricReposMaterialized,
		metric.WithDescription("Repositories materialized into sandboxes, by outcome."))
	if err != nil {
		return
	}
	counter.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// recordReposMaterializeDuration records one repo-materialization pass.
func recordReposMaterializeDuration(ctx context.Context, d time.Duration) {
	hist, err := otel.GetMeterProvider().Meter(meterName).Float64Histogram(
		MetricReposMaterializeDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of a session's repository-materialization pass."))
	if err != nil {
		return
	}
	hist.Record(ctx, d.Seconds())
}

// recordRepoMaterializeBytes records one landed repository's shipped size.
func recordRepoMaterializeBytes(ctx context.Context, n int64) {
	hist, err := otel.GetMeterProvider().Meter(meterName).Int64Histogram(
		MetricReposMaterializeBytes,
		metric.WithUnit("By"),
		metric.WithDescription("Bytes shipped into a sandbox for one materialized repository."))
	if err != nil {
		return
	}
	hist.Record(ctx, n)
}
