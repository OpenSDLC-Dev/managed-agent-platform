package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Heartbeat appends span.outcome_evaluation_ongoing carrying the outcome id
// and iteration — but only while the entry is still `evaluating` (a cycle
// settled underneath the grader must not signal liveness after its end
// event); a Finish whose settlement transaction failed marks the OTel span
// as an error, so the trace never shows an aborted cycle as clean.
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

	seed := `[{"type":"outcome_evaluation","outcome_id":"outc_hb","description":"d","explanation":"","iteration":2,"result":"evaluating","completed_at":null}]`
	if _, err := pool.Exec(ctx,
		`UPDATE sessions SET outcome_evaluations = $2 WHERE id = $1`,
		sid.String(), []byte(seed)); err != nil {
		t.Fatal(err)
	}

	_, oe := log.StartOutcomeEvaluation(ctx, sid, "outc_hb", 2, "sevt_start",
		events.Backend{Provider: "anthropic", Model: "claude-x"})
	if err := oe.Heartbeat(ctx); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	// The fence: once the entry leaves evaluating (an interrupt settled the
	// cycle), a late heartbeat appends nothing and is not an error.
	if _, err := pool.Exec(ctx,
		`UPDATE sessions SET outcome_evaluations = $2 WHERE id = $1`,
		sid.String(), []byte(strings.Replace(seed, "evaluating", "interrupted", 1))); err != nil {
		t.Fatal(err)
	}
	if err := oe.Heartbeat(ctx); err != nil {
		t.Fatalf("stale heartbeat: %v", err)
	}

	oe.Finish(domain.OutcomeResultFailed, errors.New("tx aborted"))

	rows, err := pool.Query(ctx,
		`SELECT payload FROM events WHERE session_id = $1 AND type = $2`,
		sid.String(), domain.EventSpanOutcomeEvalOngoing)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var payloads [][]byte
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
	}
	if len(payloads) != 1 {
		t.Fatalf("ongoing events = %d, want 1 (live heartbeat appended, stale one fenced)", len(payloads))
	}
	var p struct {
		OutcomeID string `json:"outcome_id"`
		Iteration int64  `json:"iteration"`
	}
	if err := json.Unmarshal(payloads[0], &p); err != nil {
		t.Fatal(err)
	}
	if p.OutcomeID != "outc_hb" || p.Iteration != 2 {
		t.Errorf("ongoing payload = %s, want outcome_id=outc_hb iteration=2", payloads[0])
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	if got := spans[0].Status().Code; got != codes.Error {
		t.Errorf("span status = %v, want Error for an uncommitted end", got)
	}

	// A failed verdict that committed is a judgment about the deliverables,
	// not a platform fault: the span stays clean and carries the result.
	_, clean := log.StartOutcomeEvaluation(ctx, sid, "outc_hb", 2, "sevt_start",
		events.Backend{Provider: "anthropic", Model: "claude-x"})
	clean.Finish(domain.OutcomeResultFailed, nil)
	spans = recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("exported %d spans, want 2", len(spans))
	}
	if got := spans[1].Status().Code; got != codes.Unset {
		t.Errorf("span status = %v, want Unset for a committed failed verdict", got)
	}
	var result string
	for _, kv := range spans[1].Attributes() {
		if string(kv.Key) == "outcome.result" {
			result = kv.Value.AsString()
		}
	}
	if result != domain.OutcomeResultFailed {
		t.Errorf("outcome.result attribute = %q, want failed", result)
	}
}
