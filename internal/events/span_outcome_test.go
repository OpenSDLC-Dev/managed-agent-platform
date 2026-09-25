package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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

// BeginOutcomeWork is the primary's span start beginning work on every
// pending outcome: it flips each pending entry to running, keeps every other
// entry and the order they are in, and writes nothing at all when no entry
// is pending — which is every start but the one that begins an outcome, so
// the session row it would rewrite, updated_at included, is left alone.
func TestBeginOutcomeWorkFlipsPendingAndWritesNothingOtherwise(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	sid := newSession(t, pool)
	set := func(evals string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE sessions SET outcome_evaluations = $2, updated_at = '2026-01-01T00:00:00Z' WHERE id = $1`,
			sid.String(), evals); err != nil {
			t.Fatal(err)
		}
	}
	begin := func() (evals []domain.OutcomeEvaluation, updated time.Time) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := events.BeginOutcomeWork(ctx, tx, sid); err != nil {
			t.Fatal(err)
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT outcome_evaluations, updated_at FROM sessions WHERE id = $1`, sid.String()).
			Scan(&raw, &updated); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &evals); err != nil {
			t.Fatal(err)
		}
		return evals, updated
	}

	set(`[{"outcome_id":"outc_a","result":"satisfied"},{"outcome_id":"outc_b","result":"interrupted"}]`)
	evals, updated := begin()
	if len(evals) != 2 || evals[0].Result != "satisfied" || evals[1].Result != "interrupted" {
		t.Errorf("entries = %+v, want them untouched", evals)
	}
	if !updated.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("updated_at = %v: a start with nothing pending rewrote the session row", updated)
	}

	set(`[{"outcome_id":"outc_a","result":"satisfied","explanation":"done"},{"outcome_id":"outc_b","result":"pending","description":"next"}]`)
	evals, updated = begin()
	if len(evals) != 2 || evals[0].OutcomeID != "outc_a" || evals[0].Result != "satisfied" || evals[0].Explanation != "done" ||
		evals[1].OutcomeID != "outc_b" || evals[1].Result != "running" || evals[1].Description != "next" {
		t.Errorf("entries = %+v, want outc_a untouched and outc_b running, in order", evals)
	}
	if updated.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("updated_at unmoved by a flip")
	}
}
