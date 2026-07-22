package api

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Files registry instruments (docs/plan/08_files.md observability table),
// mirroring the skills registry names. Attribute cardinality is bounded:
// outcome only — file ids belong in logs and span attributes, never in metric
// labels. Exported so filesmetric_test.go can assert the exact names and labels.
const (
	MetricFileUploads       = "files.uploads"
	MetricFileUploadBytes   = "files.upload.bytes"
	MetricFileDownloadBytes = "files.download.bytes"
)

// Upload outcomes, the same vocabulary as the skills registry.
const (
	fileOutcomeOK      = "ok"
	fileOutcomeInvalid = "invalid"
	fileOutcomeError   = "error"
)

// recordFileUpload counts one upload attempt and, when it stored an object, its
// size. The meter is resolved per call so it never pins a MeterProvider
// installed after startup; telemetry failure is never worth failing the request
// over.
func recordFileUpload(ctx context.Context, outcome string, storedBytes int64) {
	meter := otel.GetMeterProvider().Meter(apiMeterName)
	c, err := meter.Int64Counter(MetricFileUploads,
		metric.WithDescription("File upload attempts by outcome."))
	if err != nil {
		return
	}
	c.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	if outcome != fileOutcomeOK {
		return
	}
	h, err := meter.Int64Histogram(MetricFileUploadBytes,
		metric.WithUnit("By"),
		metric.WithDescription("Stored file size per successful upload."))
	if err != nil {
		return
	}
	h.Record(ctx, storedBytes)
}

// recordFileDownload records one served file download.
func recordFileDownload(ctx context.Context, bytes int64) {
	meter := otel.GetMeterProvider().Meter(apiMeterName)
	h, err := meter.Int64Histogram(MetricFileDownloadBytes,
		metric.WithUnit("By"),
		metric.WithDescription("Served file size per download."))
	if err != nil {
		return
	}
	h.Record(ctx, bytes)
}
