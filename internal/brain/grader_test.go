package brain_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/jackc/pgx/v5"
)

// wakeOutcome mimics the control plane's define_outcome trigger: the event,
// the running flip, the born-pending entry, and the queued turn — one
// transaction, as the API commits it.
func (h *harness) wakeOutcome(t *testing.T, description string, maxIterations int64) domain.ID {
	t.Helper()
	outcomeID := domain.NewID(domain.PrefixOutcome)
	payload, _ := json.Marshal(map[string]any{
		"description":    description,
		"rubric":         map[string]any{"type": "text", "content": "# Rubric\n- complete"},
		"max_iterations": maxIterations,
		"outcome_id":     outcomeID,
	})
	running := domain.SessionRunning
	_, err := h.log.AppendWith(context.Background(), h.sessionID, []events.NewEvent{
		{Type: domain.EventUserDefineOutcome, Payload: payload},
		{Type: domain.EventSessionStatusRunning},
	}, events.AppendOptions{
		SetStatus: &running,
		MutateOutcomes: func(evals []domain.OutcomeEvaluation) ([]domain.OutcomeEvaluation, error) {
			return append(evals, domain.OutcomeEvaluation{
				Type: "outcome_evaluation", OutcomeID: outcomeID,
				Description: description, Result: domain.OutcomeResultPending,
			}), nil
		},
		Then: func(ctx context.Context, tx pgx.Tx) error {
			_, err := h.queue.Enqueue(ctx, tx, h.envID, h.sessionID, queue.ModelTurn)
			return err
		},
	})
	if err != nil {
		t.Fatalf("wakeOutcome: %v", err)
	}
	return outcomeID
}

// drain runs the brain until the queue is empty.
func (h *harness) drain(t *testing.T) {
	t.Helper()
	for i := 0; i < 20; i++ {
		found, err := h.brain.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if !found {
			return
		}
	}
	t.Fatalf("queue never drained")
}

func (h *harness) outcomes(t *testing.T) []domain.OutcomeEvaluation {
	t.Helper()
	var raw []byte
	if err := h.pool.QueryRow(context.Background(),
		`SELECT outcome_evaluations FROM sessions WHERE id = $1`, h.sessionID.String()).Scan(&raw); err != nil {
		t.Fatalf("read outcomes: %v", err)
	}
	var evals []domain.OutcomeEvaluation
	if err := json.Unmarshal(raw, &evals); err != nil {
		t.Fatalf("decode outcomes: %v", err)
	}
	return evals
}

func (h *harness) eventsOfType(t *testing.T, typ domain.EventType) []domain.Event {
	t.Helper()
	evs, err := h.log.List(context.Background(), h.sessionID, events.ListQuery{Types: []string{string(typ)}})
	if err != nil {
		t.Fatalf("list %s: %v", typ, err)
	}
	return evs
}

func agentReply(text string) []provider.Chunk {
	return []provider.Chunk{textChunk(0, text), done("end_turn", 5)}
}

func graderReply(assessment, verdict string) []provider.Chunk {
	return []provider.Chunk{textChunk(0, assessment+"\nVERDICT: "+verdict), done("end_turn", 3)}
}

func TestOutcomeSatisfied(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		agentReply("built the model"),
		graderReply("all criteria met", "satisfied"),
	}, nil)
	outcomeID := h.wakeOutcome(t, "Build a DCF model", 3)
	h.drain(t)

	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle (satisfied outcome idles the session)", got)
	}
	evals := h.outcomes(t)
	if len(evals) != 1 || evals[0].Result != domain.OutcomeResultSatisfied {
		t.Fatalf("outcomes = %+v, want one satisfied entry", evals)
	}
	if evals[0].CompletedAt == nil {
		t.Errorf("completed_at nil on a terminal entry")
	}
	if evals[0].Explanation != "all criteria met" {
		t.Errorf("explanation = %q", evals[0].Explanation)
	}

	starts := h.eventsOfType(t, domain.EventSpanOutcomeEvalStart)
	ends := h.eventsOfType(t, domain.EventSpanOutcomeEvalEnd)
	if len(starts) != 1 || len(ends) != 1 {
		t.Fatalf("span events = %d starts / %d ends, want 1/1", len(starts), len(ends))
	}
	var end struct {
		OutcomeID string            `json:"outcome_id"`
		StartID   string            `json:"outcome_evaluation_start_id"`
		Iteration int64             `json:"iteration"`
		Result    string            `json:"result"`
		Usage     domain.ModelUsage `json:"usage"`
	}
	if err := json.Unmarshal(ends[0].Body, &end); err != nil {
		t.Fatal(err)
	}
	if end.OutcomeID != outcomeID.String() || end.StartID != starts[0].ID.String() ||
		end.Iteration != 0 || end.Result != "satisfied" {
		t.Errorf("end event = %+v (start id %s)", end, starts[0].ID)
	}
	if end.Usage.OutputTokens != 3 {
		t.Errorf("end usage = %+v, want the grader call's", end.Usage)
	}

	// The grader ran in a separate context: its request carries the grader
	// system (with the rubric), no tools, and the transcript — not the
	// agent's own system prompt.
	if len(h.provider.calls) != 2 {
		t.Fatalf("provider calls = %d, want 2 (agent + grader)", len(h.provider.calls))
	}
	g := h.provider.calls[1]
	if !strings.Contains(g.System, "outcome grader") || !strings.Contains(g.System, "# Rubric") {
		t.Errorf("grader system = %q", g.System)
	}
	if len(g.Tools) != 0 {
		t.Errorf("grader got %d tools, want none", len(g.Tools))
	}
	if !strings.Contains(string(g.Messages[0].Content), "built the model") {
		t.Errorf("grader transcript missing the agent's work: %s", g.Messages[0].Content)
	}
}

func TestOutcomeNeedsRevisionThenSatisfied(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		agentReply("first draft"),
		graderReply("missing sensitivity analysis", "needs_revision"),
		agentReply("added sensitivity analysis"),
		graderReply("now complete", "satisfied"),
	}, nil)
	h.wakeOutcome(t, "Build it", 3)
	h.drain(t)

	evals := h.outcomes(t)
	if evals[0].Result != domain.OutcomeResultSatisfied || evals[0].Iteration != 1 {
		t.Fatalf("entry = %+v, want satisfied at iteration 1", evals[0])
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle", got)
	}
	// Two full cycles on the log, iterations 0 and 1.
	ends := h.eventsOfType(t, domain.EventSpanOutcomeEvalEnd)
	if len(ends) != 2 {
		t.Fatalf("end events = %d, want 2", len(ends))
	}
	// The revision turn saw the grader's feedback.
	if len(h.provider.calls) != 4 {
		t.Fatalf("provider calls = %d, want 4", len(h.provider.calls))
	}
	revision := h.provider.calls[2]
	var found bool
	for _, m := range revision.Messages {
		if strings.Contains(string(m.Content), "missing sensitivity analysis") {
			found = true
		}
	}
	if !found {
		t.Errorf("revision turn did not carry the grader feedback")
	}
}

func TestOutcomeMaxIterationsReached(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		agentReply("only attempt"),
		graderReply("still missing things", "needs_revision"), // budget 1: becomes max_iterations_reached
		agentReply("acknowledged: X done, Y remains"),         // the final acknowledgment turn
	}, nil)
	h.wakeOutcome(t, "Build it", 1)
	h.drain(t)

	evals := h.outcomes(t)
	if evals[0].Result != domain.OutcomeResultMaxIterationsReached {
		t.Fatalf("entry = %+v, want max_iterations_reached", evals[0])
	}
	if evals[0].CompletedAt == nil {
		t.Errorf("terminal entry has nil completed_at")
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle after the acknowledgment turn", got)
	}
	// Exactly one evaluation ran; the third call is the acknowledgment turn
	// prompted by the terminal end event, and no further grading follows it.
	ends := h.eventsOfType(t, domain.EventSpanOutcomeEvalEnd)
	if len(ends) != 1 {
		t.Fatalf("end events = %d, want 1", len(ends))
	}
	if len(h.provider.calls) != 3 {
		t.Fatalf("provider calls = %d, want 3 (agent, grader, acknowledgment)", len(h.provider.calls))
	}
	ack := h.provider.calls[2]
	var found bool
	for _, m := range ack.Messages {
		if strings.Contains(string(m.Content), "budget is exhausted") {
			found = true
		}
	}
	if !found {
		t.Errorf("acknowledgment turn did not carry the exhaustion prompt")
	}
}

func TestOutcomeFailed(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		agentReply("did something else"),
		graderReply("the rubric does not apply to these deliverables", "failed"),
	}, nil)
	h.wakeOutcome(t, "Build it", 3)
	h.drain(t)

	evals := h.outcomes(t)
	if evals[0].Result != domain.OutcomeResultFailed || evals[0].CompletedAt == nil {
		t.Fatalf("entry = %+v, want terminal failed", evals[0])
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle", got)
	}
}

func TestOutcomeGraderError(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		agentReply("work"),
		{}, // grader stream fails
		agentReply("more work"),
		graderReply("done now", "satisfied"),
	}, []error{nil, contextualError("model endpoint 500"), nil, nil})
	h.wakeOutcome(t, "Build it", 3)
	h.drain(t)

	// The failed grader call ends the wake like a failed model turn: the
	// entry reverts to running, the session idles retries_exhausted.
	evals := h.outcomes(t)
	if evals[0].Result != domain.OutcomeResultRunning {
		t.Fatalf("entry after grader error = %+v, want running", evals[0])
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle (retries_exhausted)", got)
	}
	errsEvs := h.eventsOfType(t, domain.EventSessionError)
	if len(errsEvs) != 1 {
		t.Fatalf("session.error events = %d, want 1", len(errsEvs))
	}
	// No end event was rendered — a cycle with no verdict leaves the start
	// open; the next wake re-grades under it.
	if ends := h.eventsOfType(t, domain.EventSpanOutcomeEvalEnd); len(ends) != 0 {
		t.Fatalf("end events after grader error = %d, want 0", len(ends))
	}

	// The next wake resumes the outcome: agent turn, then a clean grade.
	h.wake(t, "continue")
	h.drain(t)
	evals = h.outcomes(t)
	if evals[0].Result != domain.OutcomeResultSatisfied {
		t.Fatalf("entry after resume = %+v, want satisfied", evals[0])
	}
}

// contextualError is a plain error value for scripting stream failures.
type contextualError string

func (e contextualError) Error() string { return string(e) }
