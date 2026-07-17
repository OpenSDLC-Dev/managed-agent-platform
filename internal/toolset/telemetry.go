package toolset

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// meterName is this package's OTel instrumentation scope.
const meterName = "github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"

// toolDurationName is deliberately not one of OTel's gen_ai.* metrics. Those
// describe a client's call to a GenAI provider and require gen_ai.provider.name;
// running bash in a container is not that, and inventing a provider value to
// satisfy the convention would make the metric lie about what it measured. So
// the name is the platform's own, following OTel's naming rules (dotted,
// lowercase, unit in the Unit field rather than the name), while the attributes
// reuse the semconv keys that genuinely apply — gen_ai.tool.name is the same
// tool the model named, and error.type is the standard failure dimension.
const toolDurationName = "tool.execution.duration"

// errorTypeTool marks a failure the model can read and recover from — a missing
// file, a nonzero exit. It is deliberately distinct from a backend fault, which
// carries the Go error's type: a suite full of tool_error is the agent doing
// agent things, while a backend fault is the platform breaking.
const errorTypeTool = "tool_error"

// recordToolRun records one tool call's duration. It resolves the meter per
// call rather than caching an instrument at package scope: a tool call costs a
// sandbox round trip, which dwarfs this, and a cached instrument would pin
// whichever MeterProvider happened to be installed first — leaving the metric
// silently wired to a dead provider in any process that configures telemetry
// after the first call, and untestable besides.
func recordToolRun(ctx context.Context, name string, d time.Duration, res Result, err error) {
	hist, herr := otel.GetMeterProvider().Meter(meterName).Float64Histogram(
		toolDurationName,
		metric.WithDescription("Duration of one built-in tool call, measured in the sandbox."),
		metric.WithUnit("s"),
	)
	if herr != nil {
		// Telemetry is never worth failing a tool call over; the event log
		// still records what happened.
		return
	}
	attrs := []attribute.KeyValue{semconv.GenAIToolName(name)}
	switch {
	case err != nil:
		attrs = append(attrs, semconv.ErrorType(err))
	case res.IsError:
		attrs = append(attrs, semconv.ErrorTypeKey.String(errorTypeTool))
	}
	hist.Record(ctx, d.Seconds(), metric.WithAttributes(attrs...))
}
