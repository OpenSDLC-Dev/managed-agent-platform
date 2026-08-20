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
	"mime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
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
	// graderExplanationBudget caps the verdict explanation: it persists into
	// the end event and the sessions projection — re-decoded on every later
	// claim — and is re-fed to the agent each revision cycle, so an unbounded
	// grader reply must not become permanent per-claim state.
	graderExplanationBudget = 20_000
	// graderDeliverablesBudget caps the total bytes of harvested deliverables
	// inlined into the grader's message (plan 21, Decision 5): small
	// text-like files ride whole, greedily in filename order; the rest are
	// named in the listing only.
	graderDeliverablesBudget = 64 << 10
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
	// The head this cycle read: what lands past it arrived while the grader
	// ran, and this cycle's settlement is the one that must not idle past it.
	var watermark int64
	if n := len(history); n > 0 {
		watermark = history[n-1].Seq
	}
	startID := events.LatestOutcomeStartID(history, active.OutcomeID)
	d, found := events.FindDefineOutcome(history, active.OutcomeID)
	if !found {
		// Deterministic corruption: the entry exists but its definition is
		// not on the log. Retrying cannot fix it; settle the outcome failed.
		return b.settleVerdict(ctx, item, nil, active, startID,
			domain.OutcomeResultFailed,
			"The outcome's definition event is missing from the session log; the rubric cannot be applied.",
			domain.ModelUsage{}, watermark)
	}

	p, err := b.registry.Provider(agent.Model.ID)
	if err != nil {
		return b.settleGraderError(ctx, item, active,
			fmt.Sprintf("no provider for grader model %q: %v", agent.Model.ID, err), watermark)
	}
	desc, _ := b.registry.Describe(agent.Model.ID)

	// Read the harvested snapshot before the start event: a transient registry
	// read faults the item here and the reclaim retries a cycle that has not
	// visibly begun.
	deliverables, err := b.deliverablesSection(ctx, sid)
	if err != nil {
		return err
	}

	sctx, oe := b.log.StartOutcomeEvaluation(ctx, sid, active.OutcomeID, active.Iteration, startID,
		events.Backend{Provider: desc.Protocol, Model: desc.Model})

	req := provider.Request{
		System: graderSystem + "\n\n# Rubric\n\n" + b.rubricText(sctx, d),
		Messages: []provider.Message{{
			Role: "user",
			Content: mustTextContent("# Outcome\n\n" + d.Description +
				deliverables +
				"\n\n# Agent transcript\n\n" + renderTranscript(history)),
		}},
	}

	kctx, keeper := b.queue.KeepLease(sctx, item, b.cfg.LeaseTTL, 0)
	hbStop := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
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
	// Join before settling: a tick in flight as the stream ended must commit
	// (or be fenced) before the end event does, never after it.
	<-hbDone
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
		err := b.settleGraderError(ctx, item, active, streamErr.Error(), watermark)
		// The span must carry the grader's failure even when the settlement
		// committed cleanly (err nil would export a healthy cycle); Join
		// keeps a settlement failure visible alongside it.
		oe.Finish("", errors.Join(streamErr, err))
		return err
	}

	// Sanitize before parsing: the explanation lands in two jsonb sinks (the
	// end event and the entry), and Postgres jsonb rejects NUL — one stray
	// byte would fault the settlement and loop the reclaim (#228's failure
	// mode, re-opened on this path if skipped).
	verdict, explanation := parseVerdict(toolset.SanitizeText(text))
	if len(explanation) > graderExplanationBudget {
		// Cut on a rune boundary: a byte slice can split a multibyte
		// character, and the mangled tail would persist into the entry (and
		// every later revision prompt) as a replacement character.
		cut := graderExplanationBudget
		for cut > 0 && !utf8.RuneStart(explanation[cut]) {
			cut--
		}
		explanation = explanation[:cut] + "\n[truncated]"
	}
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
	err = b.settleVerdict(ctx, item, oe, active, startID, result, explanation, u, watermark)
	oe.Finish(result, err)
	return err
}

// verdictNeedsRevision is the end event's needs_revision result — a cycle
// verdict, never an entry state (the entry goes back to running).
const verdictNeedsRevision = "needs_revision"

// gradingChain is a grading cycle's chain-or-idle test on the primary thread,
// and it is the union of the two an agent turn uses — because its halves pull
// in opposite directions. Inbound input is unfiltered (pendingInput from zero):
// grading marks nothing processed, the grader consuming no user input, so an
// event that landed between the scheduling commit and the grading claim — its
// seq below everything this cycle read — must still chain, where a
// seq-filtered probe would idle past it and strand it forever. A report is
// seq-filtered (chainInput from the head this cycle read) because it can be
// found no other way, an agent.* event being stamped processed_at at write,
// and because from zero it would match every report the session ever carried.
//
// So the halves leave a seam between them — the pre-claim stretch, where a
// report would be below the watermark and stamped, matched by neither — and
// what closes it is not this probe but the rule that schedules a cycle at
// all (settleEndTurn, decision 15): grading starts only where the fold moves
// the SESSION to idle/end_turn, and requires_action and retries_exhausted
// both outrank end_turn in that pick, so every live thread is idle on
// end_turn when the cycle is scheduled. No child is running to report, and a
// client cannot start one — user.message addresses the primary (decision 9),
// which is the thread now grading. The one agent.thread_message_received
// that can still land there is the platform's own ending notice, from a
// child archived while the cycle runs, and it is bounded rather than
// stranded: a needs_revision verdict replays the whole log into the
// coordinator's revision turn, and a terminal one idles the primary with the
// notice unread on the log, where the next user.message replays it.
func gradingChain(ctx context.Context, tx pgx.Tx, sid domain.ID, watermark int64) (bool, error) {
	pending, err := pendingInput(ctx, tx, sid, "", 0)
	if err != nil || pending {
		return pending, err
	}
	return chainInput(ctx, tx, sid, "", watermark)
}

// settleVerdict commits one evaluation cycle's outcome: the end event, the
// entry mutation, and the item's fate — one transaction under the session
// row lock, mirroring the agent turn's settlement discipline. watermark is the
// head of the log this cycle read, which is what tells a report that landed
// while the grader ran from the ones already answered.
func (b *Brain) settleVerdict(ctx context.Context, item *queue.Item, oe *events.OutcomeEvaluation,
	active domain.OutcomeEvaluation, startID domain.ID,
	result, explanation string, usage domain.ModelUsage, watermark int64) error {

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
		// the end event directly against the latest start (empty only if the
		// log is so corrupt that even the scheduling start is gone).
		payload, merr := json.Marshal(map[string]any{
			"outcome_id":                  active.OutcomeID,
			"outcome_evaluation_start_id": startID.String(),
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

	switch result {
	case verdictNeedsRevision:
		// Another agent cycle: the session stays running and this item is the
		// revision turn's slot. Replay injects the feedback from the end event.
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			return b.queue.Requeue(ctx, tx, item)
		}
	case domain.OutcomeResultMaxIterationsReached:
		// Terminal, but "one final acknowledgment turn follows before the
		// session goes idle" — the requeued item runs it; replay injects the
		// acknowledgment prompt from this terminal end event.
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			return b.queue.Requeue(ctx, tx, item)
		}
	default: // satisfied | failed: the session idles — unless input arrived
		chained, err := gradingChain(ctx, tx, sid, watermark)
		if err != nil {
			return err
		}
		if chained {
			opts.Then = func(ctx context.Context, tx pgx.Tx) error {
				return b.queue.Requeue(ctx, tx, item)
			}
		} else {
			// The grading turn is the primary's (plan 35 decision 15): its
			// idle folds the session.
			pair, moved, err := events.TransitionThread(ctx, tx, sid, events.ThreadTransition{
				Status: domain.SessionIdle, Stop: &domain.StopReason{Type: domain.StopEndTurn}})
			if err != nil {
				return err
			}
			batch = append(batch, pair...)
			opts.SetStatus = moved
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
// session's next wake. Ours, INFERRED. watermark is settleVerdict's.
func (b *Brain) settleGraderError(ctx context.Context, item *queue.Item, active domain.OutcomeEvaluation, msg string, watermark int64) error {
	// Owned here, not by the callers: msg lands in a jsonb payload, and
	// Postgres jsonb rejects NUL (#228's failure mode on any unsanitized path).
	msg = toolset.SanitizeText(msg)
	sid := item.SessionID
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid.String()); err != nil {
		return err
	}

	chained, err := gradingChain(ctx, tx, sid, watermark)
	if err != nil {
		return err
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
		pair, moved, err := events.TransitionThread(ctx, tx, sid, events.ThreadTransition{
			Status: domain.SessionIdle, Stop: &domain.StopReason{Type: domain.StopRetriesExhausted}})
		if err != nil {
			return err
		}
		batch = append(batch, pair...)
		opts.SetStatus = moved
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

// deliverable is one harvested registry row the grader is shown.
type deliverable struct {
	id       string
	filename string
	mimeType string
	size     int64
}

// deliverablesSection renders the session's harvested outputs snapshot for
// the grader (plan 21, Decision 5; shape ours, INFERRED): a listing of every
// deliverable's path, mime, and size, then small text-like files inlined
// whole — greedy in filename order under graderDeliverablesBudget, a file
// that no longer fits is listed only, never truncated mid-file. An empty
// registry (no harvest ran, or the agent left nothing) renders nothing; a
// storage-less deploy (nil blobs) gets the listing without contents.
func (b *Brain) deliverablesSection(ctx context.Context, sid domain.ID) (string, error) {
	rows, err := b.pool.Query(ctx,
		`SELECT id, filename, mime_type, size_bytes FROM files
		  WHERE scope_type = 'session' AND scope_id = $1 ORDER BY filename`,
		sid.String())
	if err != nil {
		return "", fmt.Errorf("list deliverables: %w", err)
	}
	defer rows.Close()
	var files []deliverable
	for rows.Next() {
		var f deliverable
		if err := rows.Scan(&f.id, &f.filename, &f.mimeType, &f.size); err != nil {
			return "", fmt.Errorf("list deliverables: %w", err)
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("list deliverables: %w", err)
	}
	if len(files) == 0 {
		return "", nil
	}

	var sb strings.Builder
	sb.WriteString("\n\n# Deliverables\n\nThe files the agent left in the outputs directory — the work product under judgment:\n")
	for _, f := range files {
		fmt.Fprintf(&sb, "\n- %s (%s, %d bytes)", f.filename, f.mimeType, f.size)
	}
	remaining := int64(graderDeliverablesBudget)
	for _, f := range files {
		if b.blobs == nil || !inlineableMime(f.mimeType) || f.size > remaining {
			continue
		}
		content, err := b.readDeliverable(ctx, f.id, f.size)
		if err != nil {
			// The row committed with its blob, so this is residue or a
			// transient; the listing already names the file — grade on
			// rather than fault the cycle over an optional inline.
			slog.WarnContext(ctx, "brain: could not read a deliverable for the grader; listed only",
				"session", sid, "file", f.id, "error", err)
			continue
		}
		remaining -= f.size
		// Sanitized at this boundary like every other foreign text: the
		// bytes are agent-produced and ride into a provider request.
		fmt.Fprintf(&sb, "\n\n## %s\n\n%s", f.filename, toolset.SanitizeText(content))
	}
	return sb.String(), nil
}

// inlineableMime reports whether a deliverable's registry mime marks it
// text-like enough to inline for the grader: any text/* plus
// application/json.
func inlineableMime(m string) bool {
	mt, _, err := mime.ParseMediaType(m)
	if err != nil {
		return false
	}
	return strings.HasPrefix(mt, "text/") || mt == "application/json"
}

// readDeliverable fetches one harvested file's bytes from the blob store. The
// registry size bounds the read rather than being trusted (the rubricText
// posture): the read caps one byte past it, and a blob whose length disagrees
// with its row is an error — the caller lists the file instead of inlining it
// truncated, and the budget deduction of f.size stays exact.
func (b *Brain) readDeliverable(ctx context.Context, fileID string, size int64) (string, error) {
	rc, _, err := b.blobs.Get(ctx, blob.FilesKey(fileID))
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, size+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) != size {
		return "", fmt.Errorf("blob length %d disagrees with the registry's %d bytes", len(data), size)
	}
	return string(data), nil
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
//
// The agent-to-agent message pair (plan 35) is deliberately not rendered. The
// grader reads the whole session rather than one thread, so a child's report is
// already here as that child's own submit_result call, and rendering the
// coordinator's copy of it would put the same text in twice.
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
	if text, ok := flattenBlocks(p.Content); ok {
		return text
	}
	return string(p.Content)
}

// flattenBlocks renders a content-block array as plain text. Most blocks
// carry their text at the top level; a search_result block carries its
// evidence as title + source + nested text blocks, so those flatten too —
// web_search answers would otherwise vanish from the transcript.
func flattenBlocks(raw json.RawMessage) (string, bool) {
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Title   string          `json:"title"`
		Source  string          `json:"source"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	var sb strings.Builder
	add := func(text string) {
		if text == "" {
			return
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(text)
	}
	for _, b := range blocks {
		if b.Type == "search_result" {
			var parts []string
			if b.Title != "" {
				parts = append(parts, b.Title)
			}
			if b.Source != "" {
				parts = append(parts, b.Source)
			}
			if nested, ok := flattenBlocks(b.Content); ok && nested != "" {
				parts = append(parts, nested)
			}
			add(strings.Join(parts, "\n"))
			continue
		}
		add(b.Text)
	}
	return sb.String(), true
}

// mustTextContent marshals one text block array; a plain string cannot fail.
func mustTextContent(text string) json.RawMessage {
	raw, err := json.Marshal([]map[string]any{{"type": "text", "text": text}})
	if err != nil {
		panic(err)
	}
	return raw
}

// threadEndedWithoutReport builds the notice a child owes its coordinator
// when its turn ends having called no submit_result — nil for the primary
// thread, which has no parent to tell, and for a thread id no row answers to.
// The text follows ThreadEnded's convention: what happened, then the action
// still open to the coordinator.
//
// The fact is about the turn, not the thread's life, and the wording says so
// because a child can reach here having already reported: a submit_result
// delivers and ends its turn, but a message the coordinator sent while that
// turn ran chains the child one more turn (commitDelegatedTurn's chain
// check), and if the model answers that message in prose the turn ends here.
// Telling the coordinator its child yielded without answering is right —
// nothing else would say so — while "it never reported" would contradict the
// report the coordinator just read.
func threadEndedWithoutReport(ctx context.Context, tx pgx.Tx, sid, threadID domain.ID) (*events.NewEvent, error) {
	return childEndedNotice(ctx, tx, sid, threadID, func(agentName string) string {
		return fmt.Sprintf("[agent %s ended its turn without reporting]\n\n"+
			"Nothing was reported on this turn; anything it reported earlier stands. It is idle "+
			"until you message it — send it what is missing, or archive it to free the slot.", agentName)
	})
}

// childEndedNotice is the read behind every notice a child owes its
// coordinator, and the rule for when one is owed at all: only a child has a
// parent to tell, so the primary gets nothing, and a thread id no row answers
// to names nobody. Both are silences rather than errors — these callers are
// settling a turn, and a settlement that fails deterministically reclaims onto
// the same failure for as long as anyone watches.
//
// The two callers write different notices over the same read, which is the part
// that must not drift: what a child's ending is, and whose it is.
func childEndedNotice(ctx context.Context, tx pgx.Tx, sid, threadID domain.ID,
	text func(agentName string) string) (*events.NewEvent, error) {
	if threadID == "" {
		return nil, nil
	}
	var agentName string
	var child bool
	err := tx.QueryRow(ctx,
		`SELECT agent_name, parent_thread_id IS NOT NULL FROM session_threads WHERE id = $1 AND session_id = $2`,
		threadID.String(), sid.String()).Scan(&agentName, &child)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !child) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	notice, err := events.ThreadEnded(sid, threadID, agentName, text(agentName))
	if err != nil {
		return nil, err
	}
	return &notice, nil
}

// settleEndTurn is the tool-less turn's settlement, outcome-aware: under the
// session row lock it decides chain / evaluate / idle, in that order. Pending
// input chains first (a mid-turn message answers before the grader runs, and
// so does a report a child delivered while this turn was running, which is why
// the test is chainInput's rather than pendingInput's); an
// active outcome then schedules its evaluation cycle — the start event and
// the entry's flip to evaluating commit here, and the model call itself runs
// on the primary's next claim (two-phase, no call under the lock) — but only
// when this thread's end_turn is the session's quiescence (plan 35 decision
// 15: the fold would move to idle end_turn), so a child's own end_turn or
// the coordinator's park never grades mid-delegation; otherwise the thread
// idles with end_turn and the session follows the fold. Either way a child
// that ends here tells its coordinator it stopped without reporting, on the
// same terms as the other endings (below).
func (b *Brain) settleEndTurn(ctx context.Context, sid domain.ID, item *queue.Item, watermark int64,
	opts events.AppendOptions, head []events.NewEvent) error {

	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The environment kind rides the same lock acquisition: the evaluate
	// branch routes on it (plan 21, Decision 8), and a separate read would
	// cost a round trip on every settlement for a value only that branch uses.
	var outcomesJSON []byte
	var envKind string
	if err := tx.QueryRow(ctx,
		`SELECT s.outcome_evaluations, e.kind FROM sessions s
		   JOIN environments e ON e.id = s.environment_id
		  WHERE s.id = $1 FOR UPDATE OF s`,
		sid.String()).Scan(&outcomesJSON, &envKind); err != nil {
		return err
	}

	chained := false
	if watermark > 0 {
		if chained, err = chainInput(ctx, tx, sid, item.ThreadID, watermark); err != nil {
			return err
		}
	}

	batch := head
	endTurn := &domain.StopReason{Type: domain.StopEndTurn}
	switch {
	case chained:
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			return b.queue.Requeue(ctx, tx, item)
		}
	default:
		// This thread's turn is over, so a child's ending is now certain and
		// its coordinator must be told (plan 35 decision 7): a turn that ends
		// without a submit_result reports nothing, and every other ending — out
		// of retries, interrupted, archived — delivers a notice. Without one a
		// coordinator parked on a wait has nothing left to wake it, which is the
		// wedge W1 exists to avoid. A submit_result never reaches here: its turn
		// holds a tool call and settles in commitDelegatedTurn, having reported.
		notice, err := threadEndedWithoutReport(ctx, tx, sid, item.ThreadID)
		if err != nil {
			return err
		}
		var evals []domain.OutcomeEvaluation
		if len(outcomesJSON) > 0 {
			if err := json.Unmarshal(outcomesJSON, &evals); err != nil {
				return fmt.Errorf("decode stored outcome_evaluations: %w", err)
			}
		}
		active, ok := events.ActiveOutcome(evals)
		grading := ok && (active.Result == domain.OutcomeResultPending || active.Result == domain.OutcomeResultRunning)
		if grading {
			// Only at the session's quiescence (decision 15): while a sibling
			// still runs or asks, this thread idles like any other below and
			// grading waits for the last one.
			folded, stop, perr := events.PreviewTransition(ctx, tx, sid, events.ThreadTransition{
				ThreadID: item.ThreadID, Status: domain.SessionIdle, Stop: endTurn})
			if perr != nil {
				return perr
			}
			grading = folded == domain.SessionIdle && stop != nil && stop.Type == domain.StopEndTurn
		}
		if grading {
			startEv, serr := events.NewOutcomeStartEvent(active.OutcomeID, active.Iteration)
			if serr != nil {
				return serr
			}
			batch = append(batch, startEv)
			if item.ThreadID != "" {
				// A child's end_turn is the quiescence: the parked primary
				// wakes for the grading turn first, then the child idles — in
				// that order so the session never idles between, exactly as a
				// single-agent session never does. Its notice rides ahead of
				// the wake, so the turn that wake schedules already has it.
				if notice != nil {
					batch = append(batch, *notice)
				}
				wake, _, werr := events.TransitionThread(ctx, tx, sid, events.ThreadTransition{Status: domain.SessionRunning})
				if werr != nil {
					return werr
				}
				park, _, perr := events.TransitionThread(ctx, tx, sid, events.ThreadTransition{
					ThreadID: item.ThreadID, Status: domain.SessionIdle, Stop: endTurn})
				if perr != nil {
					return perr
				}
				batch = append(append(batch, wake...), park...)
			}
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
			//
			// What runs next routes on the environment kind (Decision 8): a
			// cloud sandbox is the platform's to reach, so the deliverables
			// harvest chains first — this item completes and the harvest's own
			// settlement enqueues the grading turn, which keeps the grader off
			// stale snapshots. A self_hosted sandbox is unreachable (the
			// reference worker has no file lane), so grading stays direct: the
			// item requeues and its next claim runs the grader transcript-only.
			//
			// A child's item never carries the grading turn: it completes, and
			// the primary's turn is enqueued (self_hosted) or left to the
			// harvest's settlement (cloud), which enqueues the session's — the
			// primary's — model_turn.
			opts.Then = func(ctx context.Context, tx pgx.Tx) error {
				if envKind == string(domain.EnvCloud) {
					if _, err := b.queue.Enqueue(ctx, tx, item.EnvironmentID, sid, queue.OutputsHarvest); err != nil {
						return err
					}
					return b.queue.Complete(ctx, tx, item)
				}
				if item.ThreadID != "" {
					if _, err := b.queue.Enqueue(ctx, tx, item.EnvironmentID, sid, queue.ModelTurn); err != nil {
						return err
					}
					return b.queue.Complete(ctx, tx, item)
				}
				return b.queue.Requeue(ctx, tx, item)
			}
			break
		}
		// The notice and its wake, in the order settle's retries-exhausted
		// ending takes and for its reason: the coordinator is woken before the
		// child idles, so a session whose last running thread is this one never
		// folds idle in between. Whether it wakes at all is
		// events.WakeOnThreadEnded's rule, the one every ending shares — a
		// running coordinator chains on the notice by seq instead, and one
		// with a sibling still working has a report coming that will wake it.
		woke := false
		var wokeTo *domain.SessionStatus
		if notice != nil {
			pair, wokeMoved, wokeOK, werr := events.WakeOnThreadEnded(ctx, tx, sid, item.ThreadID)
			if werr != nil {
				return werr
			}
			batch = append(append(batch, *notice), pair...)
			woke, wokeTo = wokeOK, wokeMoved
		}
		pair, moved, err := events.TransitionThread(ctx, tx, sid, events.ThreadTransition{
			ThreadID: item.ThreadID, Status: domain.SessionIdle, Stop: endTurn})
		if err != nil {
			return err
		}
		batch = append(batch, pair...)
		if moved == nil {
			// The net of the two transitions, as settle computes it: the wake
			// may have moved the column this thread's own idle then left alone.
			moved = wokeTo
		}
		opts.SetStatus = moved
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			if err := b.queue.Complete(ctx, tx, item); err != nil {
				return err
			}
			if !woke {
				return nil
			}
			_, err := b.queue.EnqueueThread(ctx, tx, item.EnvironmentID, sid, "", queue.ModelTurn)
			return err
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
