package events

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	genaiconv "go.opentelemetry.io/otel/semconv/v1.41.0/genaiconv"
)

// meterName is this package's OTel instrumentation scope.
const meterName = "github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"

// errorTypeCommit marks a turn whose model call was fine but whose settlement
// never landed — the trace already says so; the metric should too, and it is a
// different problem from the endpoint failing.
const errorTypeCommit = "commit_failed"

// errorTypeModel marks a turn the model itself failed.
const errorTypeModel = "model_error"

// recordMetrics emits the turn's OTel GenAI metrics. A model turn is a client
// call to a GenAI provider, which is precisely what these two conventional
// instruments describe, so the names and attributes are the convention's rather
// than ours. They are recorded here, beside the span and the span.* wire events,
// so all three views of one turn come from the same instrumentation point
// (CLAUDE.md principle 3) and cannot drift.
//
// The meter is resolved per turn rather than cached at package scope: a model
// turn costs a network round trip, which dwarfs this, and a cached instrument
// would pin whichever MeterProvider was installed first.
func (m *ModelRequest) recordMetrics(ctx context.Context, isError bool, commitErr error) {
	meter := otel.GetMeterProvider().Meter(meterName)
	provider := genaiconv.ProviderNameAttr(m.backend.Provider)

	dur, err := genaiconv.NewClientOperationDuration(meter)
	if err != nil {
		// Telemetry is never worth failing a turn over, and the event log has
		// already recorded what happened.
		return
	}
	attrs := []attribute.KeyValue{dur.AttrRequestModel(m.backend.Model)}
	switch {
	case commitErr != nil:
		attrs = append(attrs, dur.AttrErrorType(errorTypeCommit))
	case isError:
		attrs = append(attrs, dur.AttrErrorType(errorTypeModel))
	}
	// The call to the provider, not the turn: ModelDone stamped the boundary
	// when the stream ended. A turn abandoned mid-stream never stamped one, and
	// there the request's whole elapsed IS the attempt.
	elapsed := m.modelElapsed
	if elapsed == 0 {
		elapsed = time.Since(m.started)
	}
	dur.Record(ctx, elapsed.Seconds(), genaiconv.OperationNameChat, provider, attrs...)

	if !m.hasUsage {
		return
	}
	tok, err := genaiconv.NewClientTokenUsage(meter)
	if err != nil {
		return
	}
	model := tok.AttrRequestModel(m.backend.Model)
	// gen_ai.token.type has exactly two values, input and output — the
	// convention has no bucket for a cache read, and describes the instrument as
	// the number of input and output tokens used. Cached and cache-creation
	// tokens ARE prompt tokens; the domain carries them apart only because
	// Anthropic's wire shape does (principle 1), and that split must not leak
	// into a metric whose vocabulary has no room for it. Recording only the
	// fresh remainder would under-report the prompt by an order of magnitude on
	// this platform in particular, where a long-horizon turn replays the whole
	// session and a cache read is the normal case, not the exception.
	input := m.usage.InputTokens + m.usage.CacheReadInputTokens + m.usage.CacheCreationInputTokens
	tok.Record(ctx, input, genaiconv.OperationNameChat, provider, genaiconv.TokenTypeInput, model)
	tok.Record(ctx, m.usage.OutputTokens, genaiconv.OperationNameChat, provider, genaiconv.TokenTypeOutput, model)
}
