package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"

// Backend names the model backend one request goes to: the protocol its
// endpoint speaks and the model id sent upstream. It is telemetry's view of the
// route the provider registry resolved — never the credential, and never the
// provider itself, which would drag model backends into the event log.
type Backend struct {
	// Provider is the OTel gen_ai.provider.name — "anthropic" or "openai",
	// which are the protocol values the registry routes on.
	Provider string
	// Model is the gen_ai.request.model: what the endpoint was asked for.
	Model string
}

// StartModelRequest emits the span.model_request_start event and opens the
// matching OTel client span from one instrumentation point, so the wire
// events and the OTel trace can never drift (CLAUDE.md principle 3). Finish
// records the turn's metrics from the same point, for the same reason. The
// returned context carries the span for downstream propagation.
func (l *Log) StartModelRequest(ctx context.Context, sessionID domain.ID, backend Backend) (context.Context, *ModelRequest, error) {
	ctx, span := otel.GetTracerProvider().Tracer(tracerName).Start(ctx, "model_request",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("session.id", sessionID.String())))
	now := time.Now().UTC()
	evs, err := l.Append(ctx, sessionID, []NewEvent{{
		Type:        domain.EventSpanModelRequestStart,
		ProcessedAt: &now,
	}})
	if err != nil {
		// No wire event landed, so the exported span must say why it is
		// alone: an errored, immediately-ended span records an aborted
		// attempt rather than silently drifting from the log.
		span.SetStatus(codes.Error, "failed to append span.model_request_start: "+err.Error())
		span.End()
		return ctx, nil, err
	}
	return ctx, &ModelRequest{
		log: l, sessionID: sessionID, startID: evs[0].ID, span: span,
		backend: backend, started: time.Now(),
	}, nil
}

// ModelRequest is one in-flight model call being traced.
type ModelRequest struct {
	log       *Log
	sessionID domain.ID
	startID   domain.ID
	span      trace.Span
	usage     domain.ModelUsage // recorded by EndEvent for Finish's attributes
	// hasUsage records whether EndEvent ran. A turn that died before reporting
	// usage has none, and emitting a pair of zeroes for it would dilute the
	// token histogram with readings no model ever produced.
	hasUsage bool
	backend  Backend
	started  time.Time
}

// StartEventID is the id of the span.model_request_start event, which the
// end event references as model_request_start_id.
func (m *ModelRequest) StartEventID() domain.ID { return m.startID }

// EndEvent renders the span.model_request_end wire event for the caller to
// append — the turn's settlement commits it atomically with the rest of the
// turn's output, so an uncommitted turn leaves no half-told span on the log.
// Finish then closes the OTel side; both halves live on ModelRequest so the
// wire event and the OTel span still come from one instrumentation point
// (CLAUDE.md principle 3).
func (m *ModelRequest) EndEvent(isError bool, usage domain.ModelUsage) (NewEvent, error) {
	payload, err := json.Marshal(map[string]any{
		"is_error":               isError,
		"model_request_start_id": m.startID.String(),
		"model_usage":            usage,
	})
	if err != nil {
		return NewEvent{}, err
	}
	m.usage, m.hasUsage = usage, true
	now := time.Now().UTC()
	return NewEvent{
		Type:        domain.EventSpanModelRequestEnd,
		Payload:     payload,
		ProcessedAt: &now,
	}, nil
}

// Finish closes the OTel span and records the turn's metrics. commitErr is the
// fate of the transaction that carried the EndEvent (or the reason no end event
// was attempted): non-nil records the drift explicitly, so the trace never masks
// an aborted request as a clean one.
func (m *ModelRequest) Finish(ctx context.Context, isError bool, commitErr error) {
	switch {
	case commitErr != nil:
		m.span.SetStatus(codes.Error, "span.model_request_end not committed: "+commitErr.Error())
	case isError:
		m.span.SetStatus(codes.Error, "model request failed")
	}
	m.span.SetAttributes(
		attribute.Int64("model.input_tokens", m.usage.InputTokens),
		attribute.Int64("model.output_tokens", m.usage.OutputTokens),
	)
	m.recordMetrics(ctx, isError, commitErr)
	m.span.End()
}
