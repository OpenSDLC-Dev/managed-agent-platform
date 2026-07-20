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
	usage     domain.ModelUsage // recorded by ModelDone for Finish's attributes
	// hasUsage records whether ModelDone was given a reading. A turn that died
	// before reporting usage has none, and neither has one whose endpoint
	// reported none (#90); emitting a pair of zeroes for either would dilute
	// the token histogram with readings no model ever produced.
	hasUsage bool
	backend  Backend
	started  time.Time
	// modelElapsed is how long the call to the provider took, stamped by
	// ModelDone. Zero until then; see ModelDone for why Finish cannot measure
	// this itself.
	modelElapsed time.Duration
}

// ModelDone records what the call to the model provider cost: how long it took,
// and the usage it reported (nil when it reported none, or when the stream
// failed before saying). The caller must invoke it as soon as the model's
// stream ends — before settling the turn.
//
// Both are facts of the model's call, known exactly here, which is why they are
// taken here rather than at Finish. The span and the wire events deliberately
// stay open past this point: Finish runs after the settlement transaction so it
// can record whether the end event actually committed. But settlement is a
// session-locked Postgres transaction the model had nothing to do with, so
// measuring the duration to Finish would file database contention under a
// model-latency instrument — and only on the paths that reach settlement, since
// the abandon paths finish straight after the stream, leaving the metric
// inconsistent with itself. Usage sourced from settlement would be worse: a
// turn that streams a full answer and then loses its lease renders no end event
// at all, so tokens the model really spent and really billed would go
// unrecorded on exactly the paths that already cost money for nothing.
//
// Repeat calls keep the first reading.
func (m *ModelRequest) ModelDone(usage *domain.ModelUsage) {
	if m.modelElapsed != 0 {
		return
	}
	m.modelElapsed = time.Since(m.started)
	if usage != nil {
		m.usage, m.hasUsage = *usage, true
	}
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
//
// usage is what the wire event reports; a failing turn passes the zero value,
// because the schema wants a model_usage object whether or not a model ever
// produced one. It deliberately does not feed the token metric — that reads the
// usage ModelDone took from the stream itself. Settlement is the wrong place to
// learn what the model spent: it renders this event on some paths and not
// others, so sourcing the metric here would both invent zeroes for turns the
// model never costed and drop real spend for turns that never settle.
func (m *ModelRequest) EndEvent(isError bool, usage domain.ModelUsage) (NewEvent, error) {
	payload, err := json.Marshal(map[string]any{
		"is_error":               isError,
		"model_request_start_id": m.startID.String(),
		"model_usage":            usage,
	})
	if err != nil {
		return NewEvent{}, err
	}
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
