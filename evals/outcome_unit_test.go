package evals

import (
	"strings"
	"testing"
)

// Offline tests for the outcome graders' transcript-only halves — same
// contract as grade_unit_test.go: no model, no Postgres, no Docker, and each
// grader proven able to fail on the thing it names. The sandbox- and
// API-backed checks (the file reads, the registry list, the projection) only
// run live, like every other container grader in the suite; their vacuous
// branches return before touching the stack and are pinned here.

func evalStart(id string) map[string]any {
	return map[string]any{"type": "span.outcome_evaluation_start", "id": id}
}

func evalEnd(startID string, iteration int, result, explanation string) map[string]any {
	return map[string]any{
		"type":                        "span.outcome_evaluation_end",
		"outcome_evaluation_start_id": startID,
		"iteration":                   float64(iteration),
		"result":                      result,
		"explanation":                 explanation,
	}
}

func TestOutcomeCycleRan(t *testing.T) {
	g := OutcomeCycleRan(Platform)
	cases := []struct {
		name    string
		events  []map[string]any
		wantErr string // empty means pass
	}{
		{"well-formed cycle", []map[string]any{
			evalStart("s1"), evalEnd("s1", 0, "satisfied", ""),
		}, ""},
		{"no start", []map[string]any{
			evalEnd("s1", 0, "satisfied", ""),
		}, "no span.outcome_evaluation_start"},
		{"start never settled", []map[string]any{
			evalStart("s1"),
		}, "never settled"},
		{"dangling reference", []map[string]any{
			evalStart("s1"), evalEnd("s9", 0, "satisfied", ""),
		}, "not on the log"},
		// A start that lost its id and an end that lost its reference must
		// each red on their own — not pair up through the empty string.
		{"start without id", []map[string]any{
			evalStart(""), evalEnd("", 0, "satisfied", ""),
		}, "carries no id"},
		{"end without reference", []map[string]any{
			evalStart("s1"), evalEnd("", 0, "satisfied", ""),
		}, "carries no outcome_evaluation_start_id"},
		{"first end without iteration field", []map[string]any{
			evalStart("s1"),
			{"type": "span.outcome_evaluation_end", "outcome_evaluation_start_id": "s1", "result": "satisfied"},
		}, "no iteration field"},
		{"first cycle not iteration 0", []map[string]any{
			evalStart("s1"), evalEnd("s1", 1, "satisfied", ""),
		}, "numbered iteration 1"},
	}
	for _, c := range cases {
		err := g.Check(t, trialWith(c.events))
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", c.name, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: error %v, want it to contain %q", c.name, err, c.wantErr)
		}
	}
}

func TestOutcomeCycleResult(t *testing.T) {
	g := OutcomeCycleResult(0, "needs_revision", Either)
	tr := trialWith([]map[string]any{
		evalEnd("s1", 0, "needs_revision", "missing the token"),
		evalEnd("s2", 1, "satisfied", ""),
	})
	if err := g.Check(t, tr); err != nil {
		t.Errorf("matching iteration-0 result: unexpected error %v", err)
	}
	wrong := trialWith([]map[string]any{evalEnd("s1", 0, "satisfied", "looks fine")})
	if err := g.Check(t, wrong); err == nil || !strings.Contains(err.Error(), `graded "satisfied"`) {
		t.Errorf("wrong result: error %v, want the graded verdict named", err)
	}
	missing := trialWith([]map[string]any{evalEnd("s1", 1, "needs_revision", "")})
	if err := g.Check(t, missing); err == nil || !strings.Contains(err.Error(), "iteration 0") {
		t.Errorf("missing iteration: error %v, want the missing iteration named", err)
	}
}

func TestOutcomeTerminalIn(t *testing.T) {
	g := OutcomeTerminalIn([]string{"satisfied", "max_iterations_reached"}, Either)
	// Vacuous with no end events: OutcomeCycleRan owns "a cycle must run".
	if err := g.Check(t, trialWith(nil)); err != nil {
		t.Errorf("no ends: unexpected error %v", err)
	}
	// The LAST end is judged, not any earlier cycle's.
	ok := trialWith([]map[string]any{
		evalEnd("s1", 0, "needs_revision", ""),
		evalEnd("s2", 1, "max_iterations_reached", ""),
	})
	if err := g.Check(t, ok); err != nil {
		t.Errorf("terminal in set: unexpected error %v", err)
	}
	bad := trialWith([]map[string]any{evalEnd("s1", 0, "failed", "gave up")})
	if err := g.Check(t, bad); err == nil || !strings.Contains(err.Error(), `settled "failed"`) {
		t.Errorf("terminal out of set: error %v, want the settled verdict named", err)
	}
}

func TestRubricConfidential(t *testing.T) {
	g := RubricConfidential("RUBRIC-{{RECALL}}", Platform)
	// trialWith fixes Recall to r0, so the filled token is RUBRIC-r0 — the
	// grader must substitute, not scan for the placeholder.
	leaked := trialWith([]map[string]any{
		{"type": "agent.message", "content": textBlocks("the rubric wants RUBRIC-r0")},
		evalEnd("s1", 0, "needs_revision", ""),
	})
	if err := g.Check(t, leaked); err == nil || !strings.Contains(err.Error(), "agent.message") {
		t.Errorf("pre-end leak: error %v, want the leaking event type named", err)
	}
	// From the first end on, the token is legitimately in play (feedback).
	legit := trialWith([]map[string]any{
		{"type": "agent.message", "content": textBlocks("working on it")},
		evalEnd("s1", 0, "needs_revision", "add RUBRIC-r0"),
		{"type": "agent.message", "content": textBlocks("added RUBRIC-r0")},
	})
	if err := g.Check(t, legit); err != nil {
		t.Errorf("post-end mention: unexpected error %v", err)
	}
}

// The sandbox-reading graders' vacuous branches return before touching the
// stack, so a nil-stack trial pins exactly the gate that keeps them from
// red-ing behavior the platform got right.
func TestRevisionFeedbackDeliveredVacuousWithoutNamedToken(t *testing.T) {
	g := RevisionFeedbackDelivered("/mnt/session/outputs/r.txt", "RUBRIC-{{RECALL}}", Either)
	// No needs_revision end at all.
	if err := g.Check(t, trialWith([]map[string]any{evalEnd("s1", 0, "satisfied", "")})); err != nil {
		t.Errorf("no needs_revision: unexpected error %v", err)
	}
	// A needs_revision whose explanation withheld the token: delivery cannot
	// be judged. (A max_iterations_reached explanation naming it does not
	// count either — no revision turn follows that verdict.)
	tr := trialWith([]map[string]any{
		evalEnd("s1", 0, "needs_revision", "not quite right yet"),
		evalEnd("s2", 1, "max_iterations_reached", "still missing RUBRIC-r0"),
	})
	if err := g.Check(t, tr); err != nil {
		t.Errorf("token never named in a needs_revision: unexpected error %v", err)
	}
}

func TestHarvestedDeliverableVacuousUnlessSatisfied(t *testing.T) {
	g := HarvestedDeliverable("result-{{NONCE}}.txt", Platform)
	if err := g.Check(t, trialWith(nil)); err != nil {
		t.Errorf("no ends: unexpected error %v", err)
	}
	// A non-satisfied terminal leaves the post-terminal acknowledgment window
	// open (a file written there correctly has no registry row), so the check
	// must not proceed to the sandbox.
	tr := trialWith([]map[string]any{evalEnd("s1", 1, "max_iterations_reached", "")})
	if err := g.Check(t, tr); err != nil {
		t.Errorf("non-satisfied terminal: unexpected error %v", err)
	}
}

// sumTokens must count the grader's spend, which rides span.outcome_evaluation_end
// under "usage" — not "model_usage", and with no span.model_request_end of its own.
func TestSumTokensCountsGraderUsage(t *testing.T) {
	tr := trialWith([]map[string]any{
		{"type": "span.model_request_end", "model_usage": map[string]any{
			"input_tokens": float64(100), "cache_read_input_tokens": float64(20), "output_tokens": float64(30),
		}},
		{"type": "span.outcome_evaluation_end", "usage": map[string]any{
			"input_tokens": float64(1000), "output_tokens": float64(5),
		}},
	})
	got := sumTokens(tr)
	if got.Input != 1120 || got.Output != 35 {
		t.Errorf("sumTokens = %d in / %d out, want 1120 / 35", got.Input, got.Output)
	}
}

// turnEvent's offline-renderable branches: the wire field names are the
// contract (internal/events/inbound.go normalizeDefineOutcome), and a typo in
// one would otherwise first surface as a wasted paid live run.
func TestTurnEventShapes(t *testing.T) {
	s := &stack{}
	tr := &Trial{Nonce: "n0", Recall: "r0"}

	msg := s.turnEvent(t, Turn{Message: "hello {{NONCE}}"}, tr)
	if msg["type"] != "user.message" {
		t.Errorf("message turn type = %v, want user.message", msg["type"])
	}
	blocks, _ := msg["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("message turn content = %v, want one text block", msg["content"])
	}
	if b, _ := blocks[0].(map[string]any); b["text"] != "hello n0" {
		t.Errorf("message text = %v, want the nonce filled", b["text"])
	}

	out := s.turnEvent(t, Turn{Outcome: &Outcome{
		Description:   "write {{NONCE}}",
		MaxIterations: 2,
		RubricText:    "must say {{RECALL}}",
	}}, tr)
	if out["type"] != "user.define_outcome" {
		t.Errorf("outcome turn type = %v, want user.define_outcome", out["type"])
	}
	if out["description"] != "write n0" {
		t.Errorf("description = %v, want the nonce filled", out["description"])
	}
	if out["max_iterations"] != 2 {
		t.Errorf("max_iterations = %v, want 2", out["max_iterations"])
	}
	rubric, _ := out["rubric"].(map[string]any)
	if rubric["type"] != "text" || rubric["content"] != "must say r0" {
		t.Errorf("rubric = %v, want {type: text, content filled}", out["rubric"])
	}

	// Zero MaxIterations omits the field so the server's default (3) governs.
	defaulted := s.turnEvent(t, Turn{Outcome: &Outcome{Description: "d", RubricText: "r"}}, tr)
	if _, present := defaulted["max_iterations"]; present {
		t.Errorf("zero MaxIterations still rendered: %v", defaulted["max_iterations"])
	}
}
