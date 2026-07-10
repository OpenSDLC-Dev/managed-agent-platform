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

// StartModelRequest emits the span.model_request_start event and opens the
// matching OTel client span from one instrumentation point, so the wire
// events and the OTel trace can never drift (CLAUDE.md principle 3). The
// returned context carries the span for downstream propagation.
func (l *Log) StartModelRequest(ctx context.Context, sessionID domain.ID) (context.Context, *ModelRequest, error) {
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
	return ctx, &ModelRequest{log: l, sessionID: sessionID, startID: evs[0].ID, span: span}, nil
}

// ModelRequest is one in-flight model call being traced.
type ModelRequest struct {
	log       *Log
	sessionID domain.ID
	startID   domain.ID
	span      trace.Span
}

// StartEventID is the id of the span.model_request_start event, which the
// end event references as model_request_start_id.
func (m *ModelRequest) StartEventID() domain.ID { return m.startID }

// End emits span.model_request_end with the token accounting and closes the
// OTel span — again both from this single point. The wire event is appended
// first: if it cannot land, the span still closes but carries an explicit
// error status naming the missing event, so the trace records the drift
// instead of masking it as a clean request.
func (m *ModelRequest) End(ctx context.Context, isError bool, usage domain.ModelUsage) error {
	payload, err := json.Marshal(map[string]any{
		"is_error":               isError,
		"model_request_start_id": m.startID.String(),
		"model_usage":            usage,
	})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, appendErr := m.log.Append(ctx, m.sessionID, []NewEvent{{
		Type:        domain.EventSpanModelRequestEnd,
		Payload:     payload,
		ProcessedAt: &now,
	}})

	switch {
	case appendErr != nil:
		m.span.SetStatus(codes.Error, "failed to append span.model_request_end: "+appendErr.Error())
	case isError:
		m.span.SetStatus(codes.Error, "model request failed")
	}
	m.span.SetAttributes(
		attribute.Int64("model.input_tokens", usage.InputTokens),
		attribute.Int64("model.output_tokens", usage.OutputTokens),
	)
	m.span.End()
	return appendErr
}
