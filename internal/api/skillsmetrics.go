package api

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// apiMeterName is this package's OTel instrumentation scope.
const apiMeterName = "github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"

// Skill registry instruments (docs/plan/06_skills.md observability table).
// Attribute cardinality is bounded: outcome only — skill ids belong in logs
// and span attributes, never in metric labels. Exported so skillsmetric_test.go
// can assert the exact names and labels.
const (
	MetricSkillUploads       = "skills.uploads"
	MetricSkillUploadBytes   = "skills.upload.bytes"
	MetricSkillDownloadBytes = "skills.download.bytes"
)

// Upload outcomes.
const (
	skillOutcomeOK      = "ok"
	skillOutcomeInvalid = "invalid"
	skillOutcomeError   = "error"
)

// recordSkillUpload counts one upload attempt (skill create or version
// create) and, when it stored an archive, its size. The meter is resolved per
// call so it never pins a MeterProvider installed after startup; telemetry
// failure is never worth failing the request over.
func recordSkillUpload(ctx context.Context, outcome string, storedBytes int64) {
	meter := otel.GetMeterProvider().Meter(apiMeterName)
	c, err := meter.Int64Counter(MetricSkillUploads,
		metric.WithDescription("Skill archive upload attempts by outcome."))
	if err != nil {
		return
	}
	c.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	if outcome != skillOutcomeOK {
		return
	}
	h, err := meter.Int64Histogram(MetricSkillUploadBytes,
		metric.WithUnit("By"),
		metric.WithDescription("Stored skill archive size per successful upload."))
	if err != nil {
		return
	}
	h.Record(ctx, storedBytes)
}

// recordSkillDownload records one served archive download.
func recordSkillDownload(ctx context.Context, bytes int64) {
	meter := otel.GetMeterProvider().Meter(apiMeterName)
	h, err := meter.Int64Histogram(MetricSkillDownloadBytes,
		metric.WithUnit("By"),
		metric.WithDescription("Served skill archive size per download."))
	if err != nil {
		return
	}
	h.Record(ctx, bytes)
}
