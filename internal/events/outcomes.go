package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// maxRubricFileBytes caps a file rubric's stored size, mirroring the text
// rubric's 262144-character bound (ours, INFERRED — docs/DIVERGENCES.md).
const maxRubricFileBytes = 256 * 1024

// DefineOutcome is the normalized payload of a user.define_outcome event, as
// normalizeDefineOutcome stored it.
type DefineOutcome struct {
	OutcomeID     domain.ID
	Description   string
	MaxIterations int64
	RubricType    string // "text" | "file"
	RubricContent string // text rubric
	RubricFileID  string // file rubric
}

// ParseDefineOutcome decodes a normalized user.define_outcome payload.
func ParseDefineOutcome(payload json.RawMessage) (DefineOutcome, error) {
	var p struct {
		OutcomeID     string `json:"outcome_id"`
		Description   string `json:"description"`
		MaxIterations int64  `json:"max_iterations"`
		Rubric        struct {
			Type    string `json:"type"`
			Content string `json:"content"`
			FileID  string `json:"file_id"`
		} `json:"rubric"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return DefineOutcome{}, err
	}
	return DefineOutcome{
		OutcomeID:     domain.ID(p.OutcomeID),
		Description:   p.Description,
		MaxIterations: p.MaxIterations,
		RubricType:    p.Rubric.Type,
		RubricContent: p.Rubric.Content,
		RubricFileID:  p.Rubric.FileID,
	}, nil
}

// DefineOutcomes returns the batch's define_outcome payloads in order.
func DefineOutcomes(evs []NewEvent) ([]DefineOutcome, error) {
	var out []DefineOutcome
	for _, ev := range evs {
		if ev.Type != domain.EventUserDefineOutcome {
			continue
		}
		d, err := ParseDefineOutcome(ev.Payload)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// ValidateDefineOutcomes enforces the DB-backed halves of accepting a
// user.define_outcome, under the send transaction's session row lock:
//
//   - one active outcome at a time — the reference documents chaining only
//     "after the terminal span.outcome_evaluation_end event of the previous
//     outcome", so a batch with more than one, or one arriving while a stored
//     entry is non-terminal, is rejected (the 400's shape is ours, INFERRED);
//   - a file rubric's file_id must name a stored file within the org scope
//     (v1's single-tenant boundary — the registry itself) whose size fits the
//     rubric cap. The row is taken FOR SHARE so a concurrent DELETE /v1/files
//     cannot remove the row and object between this check and the snapshot —
//     the deleter blocks until this transaction commits.
//
// batchInterrupts reports a user.interrupt in the same batch: the interrupt
// settles the active outcome as `interrupted` in the same transaction — the
// documented way to chain outcomes in one send — so it clears the
// stored-entry half of the check.
func ValidateDefineOutcomes(ctx context.Context, tx pgx.Tx, sessionID domain.ID, evs []NewEvent, batchInterrupts bool) error {
	defs, err := DefineOutcomes(evs)
	if err != nil {
		return err
	}
	if len(defs) == 0 {
		return nil
	}
	if len(defs) > 1 {
		return fmt.Errorf("only one outcome is supported at a time: send the next user.define_outcome after the previous outcome's terminal span.outcome_evaluation_end")
	}
	if !batchInterrupts {
		var raw []byte
		if err := tx.QueryRow(ctx,
			`SELECT outcome_evaluations FROM sessions WHERE id = $1`, sessionID.String()).Scan(&raw); err != nil {
			return err
		}
		var evals []domain.OutcomeEvaluation
		if err := json.Unmarshal(raw, &evals); err != nil {
			return fmt.Errorf("decode stored outcome_evaluations: %w", err)
		}
		for _, e := range evals {
			if !domain.OutcomeResultTerminal(e.Result) {
				return fmt.Errorf("only one outcome is supported at a time: outcome %s is still %s", e.OutcomeID, e.Result)
			}
		}
	}
	d := defs[0]
	if d.RubricType == "file" {
		var sizeBytes int64
		err := tx.QueryRow(ctx,
			`SELECT size_bytes FROM files WHERE id = $1 FOR SHARE`, d.RubricFileID).Scan(&sizeBytes)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("rubric file %s not found", d.RubricFileID)
		}
		if err != nil {
			return err
		}
		if sizeBytes > maxRubricFileBytes {
			return fmt.Errorf("rubric file %s is %d bytes; the rubric cap is %d bytes", d.RubricFileID, sizeBytes, maxRubricFileBytes)
		}
	}
	return nil
}

// NewOutcomeEntry is the outcome_evaluations entry a freshly accepted
// define_outcome writes: born pending ("pending before the agent begins
// work"), completed_at null until a terminal result (ours, INFERRED).
func NewOutcomeEntry(d DefineOutcome) domain.OutcomeEvaluation {
	return domain.OutcomeEvaluation{
		Type:        "outcome_evaluation",
		OutcomeID:   d.OutcomeID,
		Description: d.Description,
		Result:      domain.OutcomeResultPending,
	}
}

// RubricSnapshotKey is the outcome-owned blob key a file rubric's bytes are
// copied to at acceptance, so deleting the source file mid-outcome cannot
// break replay or grading.
func RubricSnapshotKey(outcomeID domain.ID) string {
	return "outcomes/" + outcomeID.String() + "/rubric"
}

// InterruptedExplanation is the verdict text an interrupt writes into the
// outcome entry and its terminal end event (ours, INFERRED — the reference
// documents the interrupted result, not its explanation).
const InterruptedExplanation = "The outcome was interrupted by a user.interrupt before evaluation completed."

// InterruptOutcomes builds the terminal span.outcome_evaluation_end for every
// non-terminal outcome entry — the docs: an interrupt marks the result
// interrupted "even if evaluation hadn't started yet", with
// outcome_evaluation_start_id an empty string when no start fired. Returns
// the end events and whether any entry needs flipping (the caller composes
// the matching MutateOutcomes under the same lock).
func InterruptOutcomes(ctx context.Context, tx pgx.Tx, sessionID domain.ID) ([]NewEvent, bool, error) {
	var raw []byte
	if err := tx.QueryRow(ctx,
		`SELECT outcome_evaluations FROM sessions WHERE id = $1`, sessionID.String()).Scan(&raw); err != nil {
		return nil, false, err
	}
	var evals []domain.OutcomeEvaluation
	if err := json.Unmarshal(raw, &evals); err != nil {
		return nil, false, fmt.Errorf("decode stored outcome_evaluations: %w", err)
	}
	var out []NewEvent
	for _, e := range evals {
		if domain.OutcomeResultTerminal(e.Result) {
			continue
		}
		payload, err := json.Marshal(map[string]any{
			"outcome_id":                  e.OutcomeID,
			"outcome_evaluation_start_id": "",
			"iteration":                   e.Iteration,
			"result":                      domain.OutcomeResultInterrupted,
			"explanation":                 InterruptedExplanation,
			"usage":                       domain.ModelUsage{},
		})
		if err != nil {
			return nil, false, err
		}
		out = append(out, NewEvent{Type: domain.EventSpanOutcomeEvalEnd, Payload: payload})
	}
	return out, len(out) > 0, nil
}

// FlipNonTerminalOutcomes is the MutateOutcomes half of an interrupt: every
// non-terminal entry goes terminal as interrupted, completed_at stamped now.
func FlipNonTerminalOutcomes(now time.Time) func([]domain.OutcomeEvaluation) ([]domain.OutcomeEvaluation, error) {
	return func(evals []domain.OutcomeEvaluation) ([]domain.OutcomeEvaluation, error) {
		for i := range evals {
			if !domain.OutcomeResultTerminal(evals[i].Result) {
				evals[i].Result = domain.OutcomeResultInterrupted
				evals[i].Explanation = InterruptedExplanation
				t := now
				evals[i].CompletedAt = &t
			}
		}
		return evals, nil
	}
}
