package domain

import "time"

// Outcome-evaluation results, mirroring BetaManagedAgentsOutcomeEvaluationResource
// (anthropic-sdk-go v1.66.0 betasession.go): pending before the agent begins
// work, running while producing or revising, evaluating while the grader
// scores; the other four are terminal.
const (
	OutcomeResultPending              = "pending"
	OutcomeResultRunning              = "running"
	OutcomeResultEvaluating           = "evaluating"
	OutcomeResultSatisfied            = "satisfied"
	OutcomeResultMaxIterationsReached = "max_iterations_reached"
	OutcomeResultFailed               = "failed"
	OutcomeResultInterrupted          = "interrupted"
)

// OutcomeResultTerminal reports whether an evaluation result ends the outcome:
// no further evaluation cycles follow and a new user.define_outcome may be
// accepted.
func OutcomeResultTerminal(result string) bool {
	switch result {
	case OutcomeResultSatisfied, OutcomeResultMaxIterationsReached,
		OutcomeResultFailed, OutcomeResultInterrupted:
		return true
	}
	return false
}

// OutcomeEvaluation is one entry of the session's outcome_evaluations list —
// the wire shape of BetaManagedAgentsOutcomeEvaluationResource, stored
// verbatim as jsonb on the sessions row and mutated in place per evaluation
// cycle (one entry per define_outcome event, `explanation` from the most
// recent evaluation). CompletedAt is null until the result is terminal (ours,
// INFERRED — docs/DIVERGENCES.md).
type OutcomeEvaluation struct {
	Type        string     `json:"type"` // "outcome_evaluation"
	OutcomeID   ID         `json:"outcome_id"`
	Description string     `json:"description"`
	Explanation string     `json:"explanation"`
	Iteration   int64      `json:"iteration"`
	Result      string     `json:"result"`
	CompletedAt *time.Time `json:"completed_at"`
}
