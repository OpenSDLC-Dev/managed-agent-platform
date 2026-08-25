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
	// The memory-store instruments (plan 36 decision 18), the executor
	// twins' names on the worker meter: per-store materialization outcomes,
	// one materialization pass, what a sync did by action, and one sync.
	MetricMemoryMaterialized        = "memory.materialized"
	MetricMemoryMaterializeDuration = "memory.materialize.duration"
	MetricMemorySyncActions         = "memory.sync.actions"
	MetricMemorySyncDuration        = "memory.sync.duration"
)

// The memory outcomes, the executor's: untrusted is a directory that holds
// files but no marker naming the store — left as found, synced pull-only.
const (
	memoryOutcomeOK        = "ok"
	memoryOutcomeUnchanged = "unchanged"
	memoryOutcomeNotFound  = "not_found"
	memoryOutcomeFailed    = "failed"
	memoryOutcomeUntrusted = "untrusted"
)

// Bounded outcome values — skill and file ids never label metrics.
const (
	skillOutcomeOK       = "ok"
	skillOutcomeNotFound = "not_found"
	skillOutcomeFailed   = "failed"
	// corrupt: the archive did not match the digest the download advertised —
	// separable from an ordinary miss because it means storage corruption or
	// substitution, not a dangling reference.
	skillOutcomeCorrupt = "corrupt"
	fileOutcomeOK       = "ok"
	fileOutcomeNotFound = "not_found"
	fileOutcomeFailed   = "failed"
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
// adds nothing, so a quiet sync leaves no series behind.
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
		metric.WithDescription("Duration of a session's memory-store sync."))
	if err != nil {
		return
	}
	hist.Record(ctx, d.Seconds())
}
