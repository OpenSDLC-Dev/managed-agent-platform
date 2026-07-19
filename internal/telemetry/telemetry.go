// Package telemetry initializes OpenTelemetry tracing and metrics for one
// process and provides W3C trace-context propagation helpers. Every binary
// (controlplane, brain, executor, worker) starts through Run, which calls Init
// for it and owns the exit sequence that Init's shutdown has to come last in;
// the event log later emits span.* domain events from the same spans started
// here, so the two views never drift.
package telemetry

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
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
	// SampleRatio is the fraction of new root traces sampled, in [0, 1];
	// 0 samples nothing (metrics still flow). nil defaults to 1 (sample
	// everything). Child spans always follow their parent's decision, so
	// a trace is never torn.
	SampleRatio *float64
}

// Init installs the global W3C trace-context propagator and, when an
// endpoint is configured, global OTLP-exporting tracer and meter providers.
// On error no global state has been touched. The returned shutdown flushes
// buffered telemetry; call it once at process exit.
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("telemetry: ServiceName is required")
	}
	// Negated form so NaN is rejected too.
	if cfg.SampleRatio != nil && !(*cfg.SampleRatio >= 0 && *cfg.SampleRatio <= 1) {
		return nil, fmt.Errorf("telemetry: SampleRatio %v outside [0, 1]", *cfg.SampleRatio)
	}

	if cfg.Endpoint == "" {
		otel.SetTextMapPropagator(propagator)
		return func(context.Context) error { return nil }, nil
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
	))
	// After an SDK bump, resource.Default's schema URL can trail the
	// semconv version imported here; Merge then reports a conflict but
	// still returns a usable merged resource, so only real failures are
	// fatal.
	if err != nil && !errors.Is(err, resource.ErrSchemaURLConflict) {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
	logOpts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
		logOpts = append(logOpts, otlploggrpc.WithInsecure())
	}

	traceExporter, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: create trace exporter: %w", err)
	}
	metricExporter, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		return nil, fmt.Errorf("telemetry: create metric exporter: %w", err)
	}
	logExporter, err := otlploggrpc.New(ctx, logOpts...)
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		_ = metricExporter.Shutdown(ctx)
		return nil, fmt.Errorf("telemetry: create log exporter: %w", err)
	}

	sampleRatio := 1.0
	if cfg.SampleRatio != nil {
		sampleRatio = *cfg.SampleRatio
	}
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio))),
		sdktrace.WithBatcher(traceExporter),
	)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
	)
	loggerProvider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
	)

	// Globals are only touched once nothing can fail anymore.
	otel.SetTextMapPropagator(propagator)
	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	// The bridge is handed the provider directly rather than reached through
	// otel/log/global. The global buys nothing here — otelslog takes the
	// provider as an option, so the indirection would only add a process-wide
	// variable and a second way for two Inits to disagree — and otel/log is
	// still pre-1.0.
	installLogBridge(cfg.ServiceName, loggerProvider)

	// Logs drain first, and the order is load-bearing rather than tidy. All
	// three shutdowns share one deadline and run in argument order, and the
	// last record queued before this runs is the fatal-exit log Run just
	// emitted — the one an operator is looking for. A meter provider's
	// Shutdown exports unconditionally once a reader is registered, even for a
	// service that recorded nothing, so a collector that accepts logs but
	// stalls on metrics could spend the whole budget before the log flush was
	// reached; BatchProcessor.Shutdown then takes its ctx.Done branch and
	// returns without draining the queue. Draining logs first costs the traces
	// and metrics nothing they can complain about — they are already the
	// telemetry an operator can afford to lose at exit.
	return func(ctx context.Context) error {
		return errors.Join(
			loggerProvider.Shutdown(ctx),
			tracerProvider.Shutdown(ctx),
			meterProvider.Shutdown(ctx),
		)
	}, nil
}
