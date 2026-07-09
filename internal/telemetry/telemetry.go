// Package telemetry initializes OpenTelemetry tracing and metrics for one
// process and provides W3C trace-context propagation helpers. Every binary
// (controlplane, brain, executor, worker) calls Init at startup; the event
// log later emits span.* domain events from the same spans started here, so
// the two views never drift.
package telemetry

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// Config controls telemetry for one process. Each binary maps its own
// environment / Helm values onto this struct.
type Config struct {
	// ServiceName identifies the process in traces and metrics
	// (e.g. "controlplane", "brain", "executor", "worker"). Required.
	ServiceName string
	// Endpoint is the OTLP/gRPC collector address (host:port). Empty
	// disables export entirely — no dialing, no background workers — so a
	// deployment without a collector runs offline by default.
	Endpoint string
	// Insecure dials the collector without TLS (local dev, in-cluster
	// collectors behind the service mesh).
	Insecure bool
	// SampleRatio is the fraction of new root traces sampled, in [0, 1].
	// 0 means unset and defaults to 1 (sample everything). Child spans
	// always follow their parent's decision, so a trace is never torn.
	SampleRatio float64
}

// Init installs the global W3C trace-context propagator and, when an
// endpoint is configured, global OTLP-exporting tracer and meter providers.
// The returned shutdown flushes buffered telemetry; call it once at process
// exit.
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("telemetry: ServiceName is required")
	}
	// Negated form so NaN is rejected too.
	if !(cfg.SampleRatio >= 0 && cfg.SampleRatio <= 1) {
		return nil, fmt.Errorf("telemetry: SampleRatio %v outside [0, 1]", cfg.SampleRatio)
	}

	otel.SetTextMapPropagator(propagator)

	if cfg.Endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	// resource.Default's schema URL matches the semconv version imported
	// above; merging with any other version fails at runtime.
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
	}

	traceExporter, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: create trace exporter: %w", err)
	}
	metricExporter, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: create metric exporter: %w", err)
	}

	ratio := cfg.SampleRatio
	if ratio == 0 {
		ratio = 1
	}
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithBatcher(traceExporter),
	)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
	)

	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)

	return func(ctx context.Context) error {
		return errors.Join(tracerProvider.Shutdown(ctx), meterProvider.Shutdown(ctx))
	}, nil
}
