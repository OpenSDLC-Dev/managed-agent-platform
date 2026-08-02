package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Heartbeat appends span.outcome_evaluation_ongoing carrying the outcome id
// and iteration; a Finish whose settlement transaction failed marks the OTel
// span as an error, so the trace never shows an aborted cycle as clean.
func TestOutcomeEvaluationHeartbeatAndFailedFinish(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = tp.Shutdown(ctx) }()
	restore := swapTracerProvider(tp)
	defer restore()

	_, oe := log.StartOutcomeEvaluation(ctx, sid, "outc_hb", 2, "sevt_start",
		events.Backend{Provider: "anthropic", Model: "claude-x"})
	if err := oe.Heartbeat(ctx); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	oe.Finish(domain.OutcomeResultFailed, errors.New("tx aborted"))

	var payload []byte
	err := pool.QueryRow(ctx,
		`SELECT payload FROM events WHERE session_id = $1 AND type = $2`,
		sid.String(), domain.EventSpanOutcomeEvalOngoing).Scan(&payload)
	if err != nil {
		t.Fatalf("ongoing event not appended: %v", err)
	}
	var p struct {
		OutcomeID string `json:"outcome_id"`
		Iteration int64  `json:"iteration"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.OutcomeID != "outc_hb" || p.Iteration != 2 {
		t.Errorf("ongoing payload = %s, want outcome_id=outc_hb iteration=2", payload)
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	if got := spans[0].Status().Code; got != codes.Error {
		t.Errorf("span status = %v, want Error for an uncommitted end", got)
	}
}
