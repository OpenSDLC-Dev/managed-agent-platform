package brain

// The outcome grader (plan 21 slice 3): after an agent work cycle ends, a
// platform-provisioned grader — one model call in a separate context — scores
// the transcript against the outcome's rubric and either settles the outcome
// or feeds its findings back for a revision cycle.
//
// Grading is two-phase through the queue so no model call ever runs under a
// session lock: the agent turn's settlement commits span.outcome_evaluation_start,
// flips the entry to `evaluating`, and requeues its own item; the next claim
// of that item sees the evaluating entry and runs the grader instead of an
// agent turn. A brain that dies mid-grade loses its lease and the reclaim
// re-grades under the same committed start event. Everything the grader is
// shown, its prompt, and its verdict protocol are this platform's own
// (INFERRED — docs/DIVERGENCES.md; the reference's grader is opaque).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/jackc/pgx/v5"
)

const (
	// graderHeartbeatInterval paces span.outcome_evaluation_ongoing while the
	// grader call runs (cadence ours, INFERRED).
	graderHeartbeatInterval = 30 * time.Second
	// graderTranscriptBudget caps the rendered transcript handed to the
	// grader; the head is kept and a truncation note marks the cut (ours).
	graderTranscriptBudget = 200_000
	// graderItemBudget caps any single transcript item (a tool result can be
	// 100 KiB on its own; the grader needs the shape, not every byte).
	graderItemBudget = 4_000
)

// graderSystem is the grader's charge (ours, INFERRED). The verdict protocol
// is deliberately rigid so parsing is deterministic.
const graderSystem = `You are an outcome grader for an autonomous agent platform. You are given an outcome description, a rubric, and the transcript of the agent's work. Judge ONLY whether the outcome's criteria are met by the work shown.

Reply with a concise assessment: for satisfied, explain why the criteria are met; for needs_revision, list concretely what is missing or wrong so the agent can fix it; for failed, explain why the rubric cannot apply to these deliverables at all (for example, the description and rubric contradict each other). Then end your reply with exactly one final line:

VERDICT: satisfied
or
VERDICT: needs_revision
or
VERDICT: failed`

// runGrading executes one evaluation cycle for the session's evaluating
// outcome. The item is the session's own model_turn slot, requeued by the
// settlement that scheduled this cycle.
func (b *Brain) runGrading(ctx context.Context, item *queue.Item, agent domain.ResolvedAgent, evals []domain.OutcomeEvaluation) error {
	sid := item.SessionID
	active, ok := events.ActiveOutcome(evals)
	if !ok || active.Result != domain.OutcomeResultEvaluating {
		// Stale wake: the outcome settled some other way (an interrupt's flip
		// commits with a queue cancel, so this is belt over braces). Nothing
		// to grade; release the slot.
		return b.completeStaleItem(ctx, item)
	}

	history, err := b.log.List(ctx, sid, events.ListQuery{})
	if err != nil {
		return fmt.Errorf("grading replay: %w", err)
	}
	watermark := int64(0)
	if len(history) > 0 {
		watermark = history[len(history)-1].Seq
	}
	d, found := events.FindDefineOutcome(history, active.OutcomeID)
	if !found {
		// Deterministic corruption: the entry exists but its definition is
		// not on the log. Retrying cannot fix it; settle the outcome failed.
		return b.settleVerdict(ctx, item, nil, active, events.DefineOutcome{}, watermark,
			domain.OutcomeResultFailed,
			"The outcome's definition event is missing from the session log; the rubric cannot be applied.",
			domain.ModelUsage{})
	}

	p, err := b.registry.Provider(agent.Model.ID)
	if err != nil {
		return b.settleGraderError(ctx, item, active, watermark, fmt.Sprintf("no provider for grader model %q: %v", agent.Model.ID, err))
	}
	desc, _ := b.registry.Describe(agent.Model.ID)

	startID := events.LatestOutcomeStartID(history, active.OutcomeID)
	sctx, oe := b.log.StartOutcomeEvaluation(ctx, sid, active.OutcomeID, active.Iteration, startID,
		events.Backend{Provider: desc.Protocol, Model: desc.Model})

	req := provider.Request{
		System: graderSystem + "\n\n# Rubric\n\n" + b.rubricText(sctx, d),
		Messages: []provider.Message{{
			Role: "user",
			Content: mustTextContent("# Outcome\n\n" + d.Description +
				"\n\n# Agent transcript\n\n" + renderTranscript(history)),
		}},
	}

	kctx, keeper := b.queue.KeepLease(sctx, item, b.cfg.LeaseTTL)
	hbStop := make(chan struct{})
	go func() {
		t := time.NewTicker(graderHeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-t.C:
				if err := oe.Heartbeat(sctx); err != nil {
					slog.WarnContext(sctx, "brain: outcome heartbeat failed", "session", sid, "error", err)
				}
			}
		}
	}()
	text, usage, streamErr := consumeGraderStream(kctx, p, req)
	close(hbStop)
	if err := keeper.Close(); err != nil {
		// Lease gone: the outcome may already be settled by an interrupt (its
		// flip cancels our item). Nothing of ours may commit.
		oe.Finish("", err)
		return fmt.Errorf("grader lease keeper: %w", err)
	}
	if streamErr != nil {
		var ie infraError
		if errors.As(streamErr, &ie) {
			oe.Finish("", streamErr)
			return streamErr
		}
		err := b.settleGraderError(ctx, item, active, watermark, toolset.SanitizeText(streamErr.Error()))
		oe.Finish("", err)
		return err
	}

	verdict, explanation := parseVerdict(text)
	// The budget: up to max_iterations evaluation cycles total; a would-be
	// needs_revision on the final cycle is reported as max_iterations_reached
	// (boundary reading ours, INFERRED).
	result := verdict
	if verdict == verdictNeedsRevision && active.Iteration+1 >= d.MaxIterations {
		result = domain.OutcomeResultMaxIterationsReached
	}

	u := domain.ModelUsage{}
	if usage != nil {
		u = *usage
	}
	err = b.settleVerdict(ctx, item, oe, active, d, watermark, result, explanation, u)
	oe.Finish(result, err)
	return err
}

// verdictNeedsRevision is the end event's needs_revision result — a cycle
// verdict, never an entry state (the entry goes back to running).
const verdictNeedsRevision = "needs_revision"

// settleVerdict commits one evaluation cycle's outcome: the end event, the
// entry mutation, and the item's fate — one transaction under the session
// row lock, mirroring the agent turn's settlement discipline.
func (b *Brain) settleVerdict(ctx context.Context, item *queue.Item, oe *events.OutcomeEvaluation,
	active domain.OutcomeEvaluation, d events.DefineOutcome, watermark int64,
	result, explanation string, usage domain.ModelUsage) error {

	sid := item.SessionID
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var outcomesJSON []byte
	if err := tx.QueryRow(ctx,
		`SELECT outcome_evaluations FROM sessions WHERE id = $1 FOR UPDATE`,
		sid.String()).Scan(&outcomesJSON); err != nil {
		return err
	}
	var current []domain.OutcomeEvaluation
	if err := json.Unmarshal(outcomesJSON, &current); err != nil {
		return fmt.Errorf("decode stored outcome_evaluations: %w", err)
	}
	for _, e := range current {
		if e.OutcomeID == active.OutcomeID && e.Result != domain.OutcomeResultEvaluating {
			// Settled underneath us (interrupt). The end event is already on
			// the log from that path; release the slot and change nothing.
			if err := b.queue.Complete(ctx, tx, item); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
	}

	var endEv events.NewEvent
	if oe != nil {
		if endEv, err = oe.EndEvent(result, explanation, usage); err != nil {
			return err
		}
	} else {
		// The definition-missing edge settles without a traced cycle: render
		// the end event directly against the latest start (possibly absent).
		payload, merr := json.Marshal(map[string]any{
			"outcome_id":                  active.OutcomeID,
			"outcome_evaluation_start_id": "",
			"iteration":                   active.Iteration,
			"result":                      result,
			"explanation":                 explanation,
			"usage":                       usage,
		})
		if merr != nil {
			return merr
		}
		now := time.Now().UTC()
		endEv = events.NewEvent{Type: domain.EventSpanOutcomeEvalEnd, Payload: payload, ProcessedAt: &now}
	}
	batch := []events.NewEvent{endEv}

	now := time.Now().UTC()
	terminal := result == domain.OutcomeResultSatisfied || result == domain.OutcomeResultFailed ||
		result == domain.OutcomeResultMaxIterationsReached
	opts := events.AppendOptions{
		AddUsage: &usage,
		MutateOutcomes: func(evals []domain.OutcomeEvaluation) ([]domain.OutcomeEvaluation, error) {
			for i := range evals {
				if evals[i].OutcomeID != active.OutcomeID {
					continue
				}
				evals[i].Explanation = explanation
				if terminal {
					evals[i].Result = result
					t := now
					evals[i].CompletedAt = &t
				} else { // needs_revision: another revision cycle follows
					evals[i].Result = domain.OutcomeResultRunning
					evals[i].Iteration++
				}
			}
			return evals, nil
		},
	}

	switch {
	case result == verdictNeedsRevision:
		// Another agent cycle: the session stays running and this item is the
		// revision turn's slot. Replay injects the feedback from the end event.
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			return b.queue.Requeue(ctx, tx, item)
		}
	case result == domain.OutcomeResultMaxIterationsReached:
		// Terminal, but "one final acknowledgment turn follows before the
		// session goes idle" — the requeued item runs it; replay injects the
		// acknowledgment prompt from this terminal end event.
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			return b.queue.Requeue(ctx, tx, item)
		}
	default: // satisfied | failed: the session idles — unless input arrived
		chained := false
		if watermark > 0 {
			if chained, err = pendingInput(ctx, tx, sid, watermark); err != nil {
				return err
			}
		}
		if chained {
			opts.Then = func(ctx context.Context, tx pgx.Tx) error {
				return b.queue.Requeue(ctx, tx, item)
			}
		} else {
			stop, merr := json.Marshal(map[string]any{"stop_reason": map[string]any{"type": "end_turn"}})
			if merr != nil {
				return merr
			}
			batch = append(batch, events.NewEvent{Type: domain.EventSessionStatusIdle, Payload: stop})
			idle := domain.SessionIdle
			opts.SetStatus = &idle
			opts.Then = func(ctx context.Context, tx pgx.Tx) error {
				return b.queue.Complete(ctx, tx, item)
			}
		}
	}

	if _, err := b.log.AppendInTx(ctx, tx, sid, batch, opts); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if opts.SetStatus != nil {
		events.RecordSessionStatus(ctx, *opts.SetStatus)
	}
	return nil
}

// settleGraderError ends a cycle whose grader call failed: no verdict exists,
// so no end event is rendered — the entry reverts to running and the session
// settles exactly like a failed model turn (session.error + retries_exhausted;
// the platform's no-automatic-retry posture). The outcome resumes with the
// session's next wake. Ours, INFERRED.
func (b *Brain) settleGraderError(ctx context.Context, item *queue.Item, active domain.OutcomeEvaluation, watermark int64, msg string) error {
	sid := item.SessionID
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid.String()); err != nil {
		return err
	}

	chained := false
	if watermark > 0 {
		if chained, err = pendingInput(ctx, tx, sid, watermark); err != nil {
			return err
		}
	}
	retry := "exhausted"
	if chained {
		retry = "retrying"
	}
	errPayload, err := json.Marshal(map[string]any{"error": map[string]any{
		"type": "model_request_failed_error", "message": msg,
		"retry_status": map[string]any{"type": retry},
	}})
	if err != nil {
		return err
	}
	batch := []events.NewEvent{{Type: domain.EventSessionError, Payload: errPayload}}
	opts := events.AppendOptions{
		MutateOutcomes: func(evals []domain.OutcomeEvaluation) ([]domain.OutcomeEvaluation, error) {
			for i := range evals {
				if evals[i].OutcomeID == active.OutcomeID && evals[i].Result == domain.OutcomeResultEvaluating {
					evals[i].Result = domain.OutcomeResultRunning
				}
			}
			return evals, nil
		},
	}
	if chained {
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			return b.queue.Requeue(ctx, tx, item)
		}
	} else {
		stop, merr := json.Marshal(map[string]any{"stop_reason": map[string]any{"type": "retries_exhausted"}})
		if merr != nil {
			return merr
		}
		batch = append(batch, events.NewEvent{Type: domain.EventSessionStatusIdle, Payload: stop})
		idle := domain.SessionIdle
		opts.SetStatus = &idle
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			return b.queue.Complete(ctx, tx, item)
		}
	}
	if _, err := b.log.AppendInTx(ctx, tx, sid, batch, opts); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if opts.SetStatus != nil {
		events.RecordSessionStatus(ctx, *opts.SetStatus)
	}
	return nil
}

// completeStaleItem releases a work slot whose grading wake found nothing to
// grade, without touching the log.
func (b *Brain) completeStaleItem(ctx context.Context, item *queue.Item) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := b.queue.Complete(ctx, tx, item); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// rubricText resolves the outcome's rubric to the text the grader reads: a
// text rubric inline from the definition, a file rubric from its acceptance
// snapshot (the source file may be gone; the snapshot cannot be).
func (b *Brain) rubricText(ctx context.Context, d events.DefineOutcome) string {
	if d.RubricType != "file" {
		return d.RubricContent
	}
	if b.blobs == nil {
		slog.WarnContext(ctx, "brain: file rubric but no blob store configured", "outcome", d.OutcomeID)
		return "(The rubric was provided as a file, but no object store is configured to read it. Grade against the outcome description alone.)"
	}
	rc, _, err := b.blobs.Get(ctx, events.RubricSnapshotKey(d.OutcomeID))
	if err != nil {
		slog.WarnContext(ctx, "brain: rubric snapshot read failed", "outcome", d.OutcomeID, "error", err)
		return "(The rubric file could not be read. Grade against the outcome description alone.)"
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, 256*1024))
	if err != nil {
		slog.WarnContext(ctx, "brain: rubric snapshot read failed", "outcome", d.OutcomeID, "error", err)
		return "(The rubric file could not be read. Grade against the outcome description alone.)"
	}
	return string(raw)
}

// consumeGraderStream drives the grader's stream quietly: no previews, no
// thinking events — the docs are explicit that the grader's reasoning is
// opaque to the session ("you see that it's working, not what it's thinking").
func consumeGraderStream(ctx context.Context, p provider.Provider, req provider.Request) (string, *domain.ModelUsage, error) {
	stream, err := p.Generate(ctx, req)
	if err != nil {
		return "", nil, fmt.Errorf("grader request: %w", err)
	}
	defer func() { _ = stream.Close() }()
	var sb strings.Builder
	var usage *domain.ModelUsage
	for stream.Next() {
		c := stream.Chunk()
		switch c.Kind {
		case provider.KindTextDelta:
			sb.WriteString(c.Text)
		case provider.KindDone:
			usage = c.Usage
		}
	}
	if err := stream.Err(); err != nil {
		return "", usage, err
	}
	return sb.String(), usage, nil
}

// parseVerdict extracts the grader's final VERDICT line. A reply without one
// is read as needs_revision with the full text as the explanation — the
// tolerant reading; the budget bound still terminates the loop (ours,
// INFERRED).
func parseVerdict(text string) (result, explanation string) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		v, ok := strings.CutPrefix(line, "VERDICT:")
		if !ok {
			continue
		}
		explanation = strings.TrimSpace(strings.Join(append(lines[:i:i], lines[i+1:]...), "\n"))
		switch strings.TrimSpace(v) {
		case domain.OutcomeResultSatisfied:
			return domain.OutcomeResultSatisfied, explanation
		case verdictNeedsRevision:
			return verdictNeedsRevision, explanation
		case domain.OutcomeResultFailed:
			return domain.OutcomeResultFailed, explanation
		}
		break
	}
	return verdictNeedsRevision, strings.TrimSpace(text)
}

// renderTranscript renders the session's conversation-bearing events as the
// role-labeled plain text the grader reads (shape ours, INFERRED). Long items
// truncate at graderItemBudget; the whole transcript at graderTranscriptBudget.
func renderTranscript(history []domain.Event) string {
	var sb strings.Builder
	add := func(role, text string) {
		if sb.Len() >= graderTranscriptBudget {
			return
		}
		if len(text) > graderItemBudget {
			text = text[:graderItemBudget] + "\n[truncated]"
		}
		sb.WriteString("## " + role + "\n" + text + "\n\n")
	}
	for _, ev := range history {
		switch ev.Type {
		case domain.EventUserMessage:
			add("user", contentText(ev.Body))
		case domain.EventSystemMessage:
			add("system", contentText(ev.Body))
		case domain.EventAgentMessage:
			add("agent", contentText(ev.Body))
		case domain.EventAgentToolUse, domain.EventAgentMCPToolUse, domain.EventAgentCustomToolUse:
			var p struct {
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if json.Unmarshal(ev.Body, &p) == nil {
				add("agent tool call", p.Name+" "+string(p.Input))
			}
		case domain.EventUserToolResult, domain.EventUserCustomToolRes,
			domain.EventAgentToolResult, domain.EventAgentMCPToolResult:
			add("tool result", contentText(ev.Body))
		}
	}
	out := sb.String()
	if len(out) > graderTranscriptBudget {
		out = out[:graderTranscriptBudget] + "\n[transcript truncated]"
	}
	return out
}

// contentText flattens an event body's content — a string or a block array —
// into plain text for the transcript.
func contentText(body []byte) string {
	var p struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(body, &p) != nil || len(p.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(p.Content, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(p.Content, &blocks) != nil {
		return string(p.Content)
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// mustTextContent marshals one text block array; a plain string cannot fail.
func mustTextContent(text string) json.RawMessage {
	raw, err := json.Marshal([]map[string]any{{"type": "text", "text": text}})
	if err != nil {
		panic(err)
	}
	return raw
}

// settleEndTurn is the tool-less turn's settlement, outcome-aware: under the
// session row lock it decides chain / evaluate / idle, in that order. Pending
// input chains first (a mid-turn message answers before the grader runs); an
// active outcome then schedules its evaluation cycle — the start event and
// the entry's flip to evaluating commit here, and the model call itself runs
// on this item's next claim (two-phase, no call under the lock); otherwise
// the session idles with end_turn exactly as before.
func (b *Brain) settleEndTurn(ctx context.Context, sid domain.ID, item *queue.Item, watermark int64,
	opts events.AppendOptions, head []events.NewEvent) error {

	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var outcomesJSON []byte
	if err := tx.QueryRow(ctx,
		`SELECT outcome_evaluations FROM sessions WHERE id = $1 FOR UPDATE`,
		sid.String()).Scan(&outcomesJSON); err != nil {
		return err
	}

	chained := false
	if watermark > 0 {
		if chained, err = pendingInput(ctx, tx, sid, watermark); err != nil {
			return err
		}
	}

	batch := head
	switch {
	case chained:
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			return b.queue.Requeue(ctx, tx, item)
		}
	default:
		var evals []domain.OutcomeEvaluation
		if len(outcomesJSON) > 0 {
			if err := json.Unmarshal(outcomesJSON, &evals); err != nil {
				return fmt.Errorf("decode stored outcome_evaluations: %w", err)
			}
		}
		active, ok := events.ActiveOutcome(evals)
		if ok && (active.Result == domain.OutcomeResultPending || active.Result == domain.OutcomeResultRunning) {
			startEv, serr := events.NewOutcomeStartEvent(active.OutcomeID, active.Iteration)
			if serr != nil {
				return serr
			}
			batch = append(batch, startEv)
			opts.MutateOutcomes = func(evals []domain.OutcomeEvaluation) ([]domain.OutcomeEvaluation, error) {
				for i := range evals {
					if evals[i].OutcomeID == active.OutcomeID {
						evals[i].Result = domain.OutcomeResultEvaluating
					}
				}
				return evals, nil
			}
			// The session stays running — evaluating is part of the work
			// cycle; the docs' status enum has no outcome-specific state.
			opts.Then = func(ctx context.Context, tx pgx.Tx) error {
				return b.queue.Requeue(ctx, tx, item)
			}
			break
		}
		payload, merr := json.Marshal(map[string]any{"stop_reason": map[string]any{"type": "end_turn"}})
		if merr != nil {
			return merr
		}
		batch = append(batch, events.NewEvent{Type: domain.EventSessionStatusIdle, Payload: payload})
		idle := domain.SessionIdle
		opts.SetStatus = &idle
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			return b.queue.Complete(ctx, tx, item)
		}
	}

	if _, err := b.log.AppendInTx(ctx, tx, sid, batch, opts); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if opts.SetStatus != nil {
		events.RecordSessionStatus(ctx, *opts.SetStatus)
	}
	return nil
}
