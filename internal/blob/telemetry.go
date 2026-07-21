package blob

import (
	"context"
	"errors"
	"io"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName is this package's OTel instrumentation scope.
const meterName = "github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"

// MetricOpDuration and MetricOpBytes are platform-native names (dotted,
// lowercase, unit in the Unit field) recorded at the Store seam, so every
// backend and every consumer shares one view of object-storage health.
// Exported so the telemetry contract test can assert they reach a collector.
const (
	MetricOpDuration = "blob.op.duration"
	MetricOpBytes    = "blob.op.bytes"
)

// Attribute values are bounded — op is one of put/get/delete and outcome one
// of ok/not_found/error. Keys never become metric labels: a label per key is
// a cardinality leak (ids belong in logs and span attributes).
const (
	outcomeOK       = "ok"
	outcomeNotFound = "not_found"
	outcomeError    = "error"
)

// WithMetrics wraps a Store so every operation records its duration (by op
// and outcome) and payload size (by op). Get's duration covers the call that
// opens the object, not the caller's subsequent reads.
func WithMetrics(s Store) Store { return metered{next: s} }

type metered struct{ next Store }

func (m metered) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	start := time.Now()
	err := m.next.Put(ctx, key, r, size, contentType)
	record(ctx, "put", time.Since(start), size, err)
	return err
}

func (m metered) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	start := time.Now()
	rc, size, err := m.next.Get(ctx, key)
	record(ctx, "get", time.Since(start), size, err)
	return rc, size, err
}

func (m metered) Delete(ctx context.Context, key string) error {
	start := time.Now()
	err := m.next.Delete(ctx, key)
	record(ctx, "delete", time.Since(start), -1, err)
	return err
}

// record resolves the meter per call rather than caching instruments, for the
// reason internal/toolset documents: a cached instrument pins whichever
// MeterProvider was installed first. Telemetry failure is never worth failing
// a storage call over. A negative size means "no payload reading" — absent is
// not zero.
func record(ctx context.Context, op string, d time.Duration, size int64, err error) {
	meter := otel.GetMeterProvider().Meter(meterName)
	dur, derr := meter.Float64Histogram(
		MetricOpDuration,
		metric.WithDescription("Duration of one object-store operation, by op and outcome."),
		metric.WithUnit("s"),
	)
	if derr != nil {
		return
	}
	outcome := outcomeOK
	switch {
	case errors.Is(err, ErrNotFound):
		outcome = outcomeNotFound
	case err != nil:
		outcome = outcomeError
	}
	dur.Record(ctx, d.Seconds(), metric.WithAttributes(
		attribute.String("blob.op", op),
		attribute.String("outcome", outcome),
	))
	if err != nil || size < 0 {
		return
	}
	bytes, berr := meter.Int64Histogram(
		MetricOpBytes,
		metric.WithDescription("Payload size of one successful object-store put or get."),
		metric.WithUnit("By"),
	)
	if berr != nil {
		return
	}
	bytes.Record(ctx, size, metric.WithAttributes(attribute.String("blob.op", op)))
}
