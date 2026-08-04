package evals

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The grader-verdict evals (#262): the outcomes grader is a model call whose
// prompt assembly and verdict protocol are ours (docs/DIVERGENCES.md, "The
// outcome grader"), and nothing measured it. These two tasks drive outcomes
// whose satisfaction is code-checkable — one file, one exact nonce'd token —
// so ground truth is mechanical and the grader's verdict can be classed
// against it: a should-satisfy case where anything but satisfied is a strict
// failure, and a should-need-revision case where an iteration-0 satisfied is
// a lenient one.
//
// The verdict-vs-ground-truth graders are class Either, not Model: a wrong
// verdict may be the grader model drifting, but it may equally be our prompt
// assembly starving it (a harvest that published nothing, a rubric snapshot
// that failed to load grades from the description alone) — the transcript
// cannot separate the two, and the Platform checks alongside them pin the
// halves that are unambiguously ours.

// outcomeSatisfy is the should-satisfy half. The description and the rubric
// demand the same deterministic deliverable, the trial code-checks it, and if
// the deliverable is objectively correct the loop must settle satisfied.
func outcomeSatisfy() Task {
	const deliverable = "/mnt/session/outputs/result-{{NONCE}}.txt"
	const token = "RESULT-{{NONCE}}-COMPLETE"
	return Task{
		ID: "outcome-satisfy",
		Turns: []Turn{{Outcome: &Outcome{
			Description: "Create the file " + deliverable + " containing exactly the single line " +
				token + " and nothing else. That file is this outcome's only deliverable.",
			// The rubric restates the description rather than referencing it, so
			// the grader can verify from the rubric alone.
			RubricText: "The deliverable result-{{NONCE}}.txt must exist and its content must " +
				"contain the exact token " + token + ". Nothing else is required.",
			// Headroom for one revision if the model fumbles the first write;
			// ground truth is judged on the terminal verdict.
			MaxIterations: 2,
		}}},
		Graders: []Grader{
			OutcomeCycleRan(Platform),
			// The model's half: it must actually write the deliverable. Either,
			// as every artifact check is — a wrong file is as likely its mistake
			// as ours.
			FileLines(deliverable, []string{token}, Either),
			HarvestedDeliverable("result-{{NONCE}}.txt", Platform),
			GraderVerdictMatchesDeliverable(deliverable, token, Either),
			OutcomeProjectionSettled(Platform),
		},
	}
}

// outcomeRevise is the should-need-revision half. The rubric is a FILE rubric
// — grader-only by design — requiring a token the description never mentions,
// so the iteration-0 deliverable cannot satisfy it and the only correct first
// verdict is needs_revision. The rubric tells the grader to name the missing
// token in its feedback, so a terminal satisfied additionally proves the
// feedback made it back to the agent (the revision-prompt injection working
// end to end); a grader that withholds the token ends max_iterations_reached
// instead, which is why the terminal check accepts both.
func outcomeRevise() Task {
	const deliverable = "/mnt/session/outputs/report-{{NONCE}}.txt"
	const token = "RUBRIC-{{RECALL}}"
	return Task{
		ID: "outcome-revise",
		Turns: []Turn{{Outcome: &Outcome{
			Description: "Write a one-line status report for this task to " + deliverable + ". " +
				"A separate rubric governs what the report must contain; if the evaluation asks " +
				"for revisions, follow its feedback exactly.",
			RubricFile: &FileFixture{
				Name: "rubric-{{NONCE}}.txt",
				Content: "The deliverable report-{{NONCE}}.txt must contain the exact token " +
					token + ". A report missing that token is not satisfied and needs revision — " +
					"name the missing token " + token + " in your feedback so the agent can add " +
					"it. Only when the deliverable contains the token is the outcome satisfied.",
			},
			// Exactly two cycles: one graded miss, one graded fix. A third would
			// only blur which cycle a verdict belongs to.
			MaxIterations: 2,
		}}},
		Graders: []Grader{
			OutcomeCycleRan(Platform),
			RubricConfidential(token, Platform),
			// Ground truth for cycle 0: the agent cannot know the token, so
			// satisfied is a lenient failure and failed a strict one (the rubric
			// is perfectly applicable — it just is not met yet).
			OutcomeCycleResult(0, "needs_revision", Either),
			OutcomeTerminalIn([]string{"satisfied", "max_iterations_reached"}, Either),
			OutcomeProjectionSettled(Platform),
		},
	}
}

// turnEvent renders one turn's inbound event: a plain user.message, or the
// turn's user.define_outcome — uploading a file rubric first so the event can
// reference it by id. The rubric file is deliberately not added as a session
// resource (see Outcome.RubricFile).
func (s *stack) turnEvent(t *testing.T, turn Turn, tr *Trial) map[string]any {
	t.Helper()
	if turn.Outcome == nil {
		return userMessage(tr.fill(turn.Message))
	}
	o := turn.Outcome
	ev := map[string]any{
		"type":        "user.define_outcome",
		"description": tr.fill(o.Description),
	}
	if o.MaxIterations > 0 {
		ev["max_iterations"] = o.MaxIterations
	}
	if o.RubricFile != nil {
		fileID := s.uploadFile(t, *o.RubricFile, tr)
		ev["rubric"] = map[string]any{"type": "file", "file_id": fileID}
	} else {
		ev["rubric"] = map[string]any{"type": "text", "content": tr.fill(o.RubricText)}
	}
	return ev
}

// OutcomeCycleRan asserts at least one complete grading cycle is on the log —
// a span.outcome_evaluation_start, and a span.outcome_evaluation_end that
// references a committed start and numbers the first cycle iteration 0. The
// loop is platform machinery end to end (the settlement that schedules it,
// the harvest chaining, the grader call), so a missing or malformed cycle is
// a product fault whatever the model did.
func OutcomeCycleRan(class Class) Grader {
	return Grader{
		Name:  "outcome-cycle-ran",
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			starts := eventsOfType(tr, "span.outcome_evaluation_start")
			if len(starts) == 0 {
				return fmt.Errorf("no span.outcome_evaluation_start on the log: no grading cycle ever began")
			}
			ends := eventsOfType(tr, "span.outcome_evaluation_end")
			if len(ends) == 0 {
				return fmt.Errorf("%d start(s) but no span.outcome_evaluation_end: a cycle began and never settled", len(starts))
			}
			startIDs := map[string]bool{}
			for _, s := range starts {
				id, _ := s["id"].(string)
				startIDs[id] = true
			}
			for _, e := range ends {
				ref, _ := e["outcome_evaluation_start_id"].(string)
				if !startIDs[ref] {
					return fmt.Errorf("end event references start %q, which is not on the log", ref)
				}
			}
			if it := endIteration(ends[0]); it != 0 {
				return fmt.Errorf("first evaluation cycle is numbered iteration %d, want 0", it)
			}
			return nil
		},
	}
}

// OutcomeCycleResult asserts the end event for one iteration exists and
// carries the wanted result — the per-cycle half of verdict-vs-ground-truth.
func OutcomeCycleResult(iteration int, want string, class Class) Grader {
	return Grader{
		Name:  fmt.Sprintf("outcome-cycle-%d-%s", iteration, want),
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			for _, e := range eventsOfType(tr, "span.outcome_evaluation_end") {
				if endIteration(e) != iteration {
					continue
				}
				if got := endResult(e); got != want {
					return fmt.Errorf("iteration %d graded %q, want %q (explanation: %.200s)",
						iteration, got, want, endExplanation(e))
				}
				return nil
			}
			return fmt.Errorf("no span.outcome_evaluation_end for iteration %d", iteration)
		},
	}
}

// OutcomeTerminalIn asserts the last end event's result is one of the
// accepted terminals — the whole-outcome half of verdict-vs-ground-truth.
func OutcomeTerminalIn(results []string, class Class) Grader {
	return Grader{
		Name:  "outcome-terminal-in-" + strings.Join(results, "|"),
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			ends := eventsOfType(tr, "span.outcome_evaluation_end")
			if len(ends) == 0 {
				// OutcomeCycleRan owns "a cycle must run"; nothing to judge here.
				return nil
			}
			last := ends[len(ends)-1]
			if got := endResult(last); !slices.Contains(results, got) {
				return fmt.Errorf("outcome settled %q, want one of %v (explanation: %.200s)",
					got, results, endExplanation(last))
			}
			return nil
		},
	}
}

// GraderVerdictMatchesDeliverable is the should-satisfy ground truth: when
// the deliverable objectively carries its required token, the outcome must
// have settled satisfied — anything else is the grader failing strict. It is
// vacuous when the deliverable is wrong or absent (the model never earned a
// satisfied; the file grader alongside reds that half).
func GraderVerdictMatchesDeliverable(path, token string, class Class) Grader {
	return Grader{
		Name:  "grader-verdict-matches-deliverable",
		Class: class,
		Check: func(t *testing.T, tr *Trial) error {
			b, err := tr.readFile(t, tr.fill(path))
			if err != nil || !strings.Contains(string(b), tr.fill(token)) {
				return nil
			}
			ends := eventsOfType(tr, "span.outcome_evaluation_end")
			if len(ends) == 0 {
				return nil
			}
			last := ends[len(ends)-1]
			if got := endResult(last); got != "satisfied" {
				return fmt.Errorf("the deliverable carries its required token, yet the outcome settled %q (explanation: %.200s)",
					got, endExplanation(last))
			}
			return nil
		},
	}
}

// RubricConfidential asserts a file rubric's token appears in no event before
// the first span.outcome_evaluation_end. A file rubric is grader-only: the
// conversation gets the outcome description alone, so the token reaching any
// earlier event means the platform leaked the grading criteria to the agent.
// From the first end event on, the token is legitimately in play — the rubric
// instructs the grader to name it in feedback, and the revision cycle may
// echo it anywhere.
func RubricConfidential(token string, class Class) Grader {
	return Grader{
		Name:  "rubric-confidential",
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			tok := tr.fill(token)
			for _, ev := range tr.Events {
				typ, _ := ev["type"].(string)
				if typ == "span.outcome_evaluation_end" {
					return nil
				}
				raw, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				if strings.Contains(string(raw), tok) {
					return fmt.Errorf("the file rubric's token appears in a %s event before the first evaluation ended", typ)
				}
			}
			return nil
		},
	}
}

// HarvestedDeliverable asserts the outputs harvest published the deliverable
// to the files registry — a row whose filename is the outputs-relative path.
// The walk, the registry write, and the GET /v1/files exposure are all
// platform machinery (docs/DIVERGENCES.md, "The outputs harvest"): the file
// sitting in the sandbox with no registry row is ours. Vacuous when the
// sandbox file is absent — the model never wrote it, and the grader always
// runs against whatever the harvest last published.
func HarvestedDeliverable(filename string, class Class) Grader {
	return Grader{
		Name:  "harvested-deliverable",
		Class: class,
		Check: func(t *testing.T, tr *Trial) error {
			name := tr.fill(filename)
			if _, err := tr.readFile(t, "/mnt/session/outputs/"+name); err != nil {
				return nil
			}
			res := tr.stack.do(t, http.MethodGet, "/v1/files?limit=1000", nil)
			data, ok := res["data"].([]any)
			if !ok {
				return fmt.Errorf("files list has no data array: %v", res)
			}
			for _, e := range data {
				m, _ := e.(map[string]any)
				if fn, _ := m["filename"].(string); fn == name {
					return nil
				}
			}
			return fmt.Errorf("deliverable %s exists in the sandbox outputs but the files registry has no row for it (%d rows listed)",
				name, len(data))
		},
	}
}

// OutcomeProjectionSettled asserts the sessions projection agrees with the
// log: exactly one outcome_evaluations entry, terminal, its result matching
// the last end event's, completed_at stamped. The projection is what
// GET /v1/sessions serves to every client, so a log/projection disagreement
// is a wire-visible platform fault.
func OutcomeProjectionSettled(class Class) Grader {
	return Grader{
		Name:  "outcome-projection-settled",
		Class: class,
		Check: func(t *testing.T, tr *Trial) error {
			ends := eventsOfType(tr, "span.outcome_evaluation_end")
			if len(ends) == 0 {
				return nil // OutcomeCycleRan owns this
			}
			last := endResult(ends[len(ends)-1])
			list, _ := tr.stack.getSession(t, tr.SessionID)["outcome_evaluations"].([]any)
			if len(list) != 1 {
				return fmt.Errorf("session projects %d outcome_evaluations entries, want 1", len(list))
			}
			entry, _ := list[0].(map[string]any)
			result, _ := entry["result"].(string)
			if result != last {
				return fmt.Errorf("projection result %q disagrees with the last end event's %q", result, last)
			}
			switch result {
			case "satisfied", "max_iterations_reached", "failed", "interrupted":
			default:
				return fmt.Errorf("projection result %q is not terminal", result)
			}
			if entry["completed_at"] == nil {
				return fmt.Errorf("terminal entry carries no completed_at")
			}
			return nil
		},
	}
}

func endResult(ev map[string]any) string {
	r, _ := ev["result"].(string)
	return r
}

func endIteration(ev map[string]any) int {
	n, _ := ev["iteration"].(float64)
	return int(n)
}

func endExplanation(ev map[string]any) string {
	e, _ := ev["explanation"].(string)
	return e
}
