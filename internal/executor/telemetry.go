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
	// MetricMemoryMaterialized counts per-store materialization outcomes
	// (plan 36 slice 4).
	MetricMemoryMaterialized = "memory.materialized"
	// MetricMemoryMaterializeDuration is one whole memory-materialization pass.
	MetricMemoryMaterializeDuration = "memory.materialize.duration"
	// MetricMemorySyncActions counts what a run-end sync did, by action —
	// pulled, pushed, deleted, conflict, refused.
	MetricMemorySyncActions = "memory.sync.actions"
	// MetricMemorySyncDuration is one whole sync, its three phases together.
	MetricMemorySyncDuration = "memory.sync.duration"
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

	// The memory outcomes: landed, already there with its marker intact, a
	// store row that is gone, a write that failed, and a directory with files
	// but no trusted marker — left as found (decision 12).
	memoryOutcomeOK        = "ok"
	memoryOutcomeUnchanged = "unchanged"
	memoryOutcomeNotFound  = "not_found"
	memoryOutcomeFailed    = "failed"
	memoryOutcomeUntrusted = "untrusted"
)

// recordMemoryMaterialized counts one store's outcome, the files recorder's
// twin.
func recordMemoryMaterialized(ctx context.Context, outcome string) {
	counter, err := otel.GetMeterProvider().Meter(meterName).Int64Counter(
		MetricMemoryMaterialized,
		metric.WithDescription("Memory stores materialized into sandboxes, by outcome."))
	if err != nil {
		return
	}
	counter.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// recordMemoryMaterializeDuration records one memory-materialization pass.
func recordMemoryMaterializeDuration(ctx context.Context, d time.Duration) {
	hist, err := otel.GetMeterProvider().Meter(meterName).Float64Histogram(
		MetricMemoryMaterializeDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of a session's memory-materialization pass."))
	if err != nil {
		return
	}
	hist.Record(ctx, d.Seconds())
}

// recordMemorySyncActions counts one store's sync by action; a zero count
// records nothing, so a quiet sync leaves no series behind.
func recordMemorySyncActions(ctx context.Context, c memorySyncCounts) {
	counter, err := otel.GetMeterProvider().Meter(meterName).Int64Counter(
		MetricMemorySyncActions,
		metric.WithDescription("Memory-store sync actions, by action."))
	if err != nil {
		return
	}
	for _, a := range []struct {
		name string
		n    int
	}{{"pulled", c.pulled}, {"pushed", c.pushed}, {"deleted", c.deleted}, {"conflict", c.conflict}, {"refused", c.refused}} {
		if a.n > 0 {
			counter.Add(ctx, int64(a.n), metric.WithAttributes(attribute.String("action", a.name)))
		}
	}
}

// recordMemorySyncDuration records one sync, read to apply.
func recordMemorySyncDuration(ctx context.Context, d time.Duration) {
	hist, err := otel.GetMeterProvider().Meter(meterName).Float64Histogram(
		MetricMemorySyncDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of a session's memory-store sync, its three phases together."))
	if err != nil {
		return
	}
	hist.Record(ctx, d.Seconds())
}

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
