package brain_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
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
	return h.wakeOutcomeRubric(t, description, maxIterations,
		map[string]any{"type": "text", "content": "# Rubric\n- complete"},
		domain.NewID(domain.PrefixOutcome))
}

// wakeOutcomeRubric is wakeOutcome with a caller-supplied rubric and outcome
// id (a file rubric's snapshot must be seeded under the id before the wake).
func (h *harness) wakeOutcomeRubric(t *testing.T, description string, maxIterations int64,
	rubric map[string]any, outcomeID domain.ID) domain.ID {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"description":    description,
		"rubric":         rubric,
		"max_iterations": maxIterations,
		"outcome_id":     outcomeID,
	})
	_, err := h.log.AppendTransition(context.Background(), h.sessionID,
		[]events.NewEvent{{Type: domain.EventUserDefineOutcome, Payload: payload}},
		[]events.ThreadTransition{{Status: domain.SessionRunning}},
		events.AppendOptions{
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

// TestOutcomeCloudSettlementChainsHarvest pins slice 4's settlement routing
// (plan 21, Decision 8): on a cloud environment an end_turn settlement with an
// active outcome commits the start event and the evaluating flip exactly as on
// self_hosted — but instead of requeueing its own turn item it enqueues an
// outputs_harvest work item and completes itself. The grading turn is enqueued
// by the harvest's completion, so the grader always sees the cycle's current
// deliverables and never runs against a stale snapshot.
func TestOutcomeCloudSettlementChainsHarvest(t *testing.T) {
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{
		agentReply("built the model"),
		graderReply("all criteria met", "satisfied"),
	}, nil)
	h.wakeOutcome(t, "Build a DCF model", 3)
	h.runOnce(t) // the agent turn; its settlement schedules the harvest

	evals := h.outcomes(t)
	if len(evals) != 1 || evals[0].Result != domain.OutcomeResultEvaluating {
		t.Fatalf("outcomes = %+v, want one evaluating entry", evals)
	}
	if got := h.eventsOfType(t, domain.EventSpanOutcomeEvalStart); len(got) != 1 {
		t.Fatalf("start events = %d, want 1 (committed by the scheduling settlement)", len(got))
	}
	if n := h.liveOf(t, queue.OutputsHarvest); n != 1 {
		t.Errorf("live outputs_harvest items = %d, want 1", n)
	}
	if !h.harvestChainGrading(t) {
		t.Error("chain_grading = false on a grading cycle's own harvest; its settlement must chain the grading turn")
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 0 {
		t.Errorf("live model_turn items = %d, want 0 (the harvest chains the grading turn)", n)
	}
	if got := h.status(t); got != "running" {
		t.Errorf("status = %q, want running while the cycle harvests", got)
	}

	// The brain has nothing to claim until the harvest completes: grading
	// must not start before the deliverables snapshot lands.
	if found, err := h.brain.RunOnce(context.Background()); err != nil || found {
		t.Fatalf("RunOnce before harvest completion = %v, %v; want no work", found, err)
	}

	// Simulate the executor's harvest settlement: enqueue the grading turn
	// and complete the item in one transaction, as processHarvest commits it.
	ctx := context.Background()
	item, err := h.queue.Claim(ctx, queue.OutputsHarvest, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim harvest item: %+v, %v", item, err)
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := h.queue.Enqueue(ctx, tx, h.envID, h.sessionID, queue.ModelTurn); err != nil {
		t.Fatal(err)
	}
	if err := h.queue.Complete(ctx, tx, item); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	h.drain(t)
	evals = h.outcomes(t)
	if len(evals) != 1 || evals[0].Result != domain.OutcomeResultSatisfied {
		t.Fatalf("outcomes after grading = %+v, want one satisfied entry", evals)
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle after the verdict", got)
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

func TestOutcomeInterruptDuringGrading(t *testing.T) {
	// An interrupt lands while the grader call is in flight: the API path
	// flips the entry terminal in its own transaction. The verdict
	// settlement re-reads the entry under the lock and, finding it no longer
	// evaluating, discards the verdict — the interrupt's end event is the
	// only one on the log.
	h := newHarness(t, [][]provider.Chunk{
		agentReply("work"),
		graderReply("looks fine", "satisfied"),
	}, nil)
	h.wakeOutcome(t, "Build it", 3)

	h.provider.onGenerate = func(call int) {
		if call != 1 {
			return
		}
		// Mimic the API interrupt's flip (its cancel is what usually kills
		// this item's lease; racing past it exercises the settlement's own
		// stale check).
		_, err := h.log.AppendWith(context.Background(), h.sessionID, nil, events.AppendOptions{
			MutateOutcomes: events.FlipNonTerminalOutcomes(time.Now().UTC()),
		})
		if err != nil {
			t.Errorf("mid-grade flip: %v", err)
		}
	}
	h.drain(t)

	evals := h.outcomes(t)
	if evals[0].Result != domain.OutcomeResultInterrupted {
		t.Fatalf("entry = %+v, want interrupted (the API's flip wins)", evals[0])
	}
	// The discarded verdict rendered no end event of its own.
	if ends := h.eventsOfType(t, domain.EventSpanOutcomeEvalEnd); len(ends) != 0 {
		t.Fatalf("end events = %d, want 0 (the flip here bypassed the API's own end append)", len(ends))
	}
}

func TestOutcomeMidScheduleMessageChains(t *testing.T) {
	// A user.message that lands between the settlement that schedules grading
	// and the grading claim sits below every seq the grading wake reads.
	// Grading marks nothing processed, so the verdict settlement must chain a
	// turn for any unprocessed input — a seq-filtered probe would idle past
	// this message and strand it unprocessed forever.
	h := newHarness(t, [][]provider.Chunk{
		agentReply("work"),
		graderReply("fine", "satisfied"),
		agentReply("answering the late message"),
	}, nil)
	h.wakeOutcome(t, "Build it", 3)
	// Exactly the agent turn: its settlement commits the start event, flips
	// the entry to evaluating, and requeues the item for grading.
	if _, err := h.brain.RunOnce(context.Background()); err != nil {
		t.Fatalf("agent turn: %v", err)
	}
	if _, err := h.log.Append(context.Background(), h.sessionID, []events.NewEvent{{
		Type: domain.EventUserMessage, Payload: json.RawMessage(`{"content":"one more thing"}`),
	}}); err != nil {
		t.Fatalf("late message: %v", err)
	}
	h.drain(t)

	if len(h.provider.calls) != 3 {
		t.Fatalf("provider calls = %d, want 3 (agent, grader, chained answer)", len(h.provider.calls))
	}
	var stranded bool
	if err := h.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM events
		  WHERE session_id = $1 AND type = 'user.message' AND processed_at IS NULL)`,
		h.sessionID.String()).Scan(&stranded); err != nil {
		t.Fatal(err)
	}
	if stranded {
		t.Error("late user.message left unprocessed")
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle", got)
	}
}

func TestGraderReplyNULSanitized(t *testing.T) {
	// One NUL byte in the grader's reply must not fault the settlement's two
	// jsonb writes (#228's reclaim loop, re-opened on this path if the model
	// text went in raw).
	h := newHarness(t, [][]provider.Chunk{
		agentReply("work"),
		graderReply("contains a NUL \x00 byte", "satisfied"),
	}, nil)
	h.wakeOutcome(t, "Build it", 3)
	h.drain(t)

	evals := h.outcomes(t)
	if evals[0].Result != domain.OutcomeResultSatisfied {
		t.Fatalf("entry = %+v, want satisfied", evals[0])
	}
	if evals[0].Explanation != "contains a NUL  byte" {
		t.Errorf("explanation = %q, want the NUL stripped", evals[0].Explanation)
	}
}

func TestOutcomePendingFlipsRunningAtTurnStart(t *testing.T) {
	// "pending" is before the agent begins work; "running" while producing.
	// The flip commits when the turn claims, so a client polling mid-turn
	// sees running, not pending.
	h := newHarness(t, [][]provider.Chunk{
		agentReply("work"),
		graderReply("fine", "satisfied"),
	}, nil)
	h.wakeOutcome(t, "Build it", 3)
	var during string
	h.provider.onGenerate = func(call int) {
		if call == 0 {
			during = h.outcomes(t)[0].Result
		}
	}
	h.drain(t)
	if during != domain.OutcomeResultRunning {
		t.Errorf("entry during the first agent turn = %q, want running", during)
	}
}

// blobHarness is the harness plus the in-memory object store its brain reads.
type blobHarness struct {
	*harness
	blobs *blobtest.MemStore
}

func newHarnessWithBlobs(t *testing.T, scripts [][]provider.Chunk, errs []error) *blobHarness {
	t.Helper()
	h := newHarness(t, scripts, errs)
	blobs := blobtest.Mem()
	// Rebuild the brain with the store, reusing the harness's registry so the
	// fake provider keeps newHarness's exact wiring.
	h.brain = brain.New(h.pool, h.registry, blobs, brain.Config{})
	return &blobHarness{harness: h, blobs: blobs}
}

func TestOutcomeFileRubricSnapshot(t *testing.T) {
	// A file rubric grades from its acceptance snapshot in the blob store.
	h := newHarnessWithBlobs(t, [][]provider.Chunk{
		agentReply("work"),
		graderReply("meets the filed rubric", "satisfied"),
	}, nil)
	outcomeID := domain.NewID(domain.PrefixOutcome)
	if err := h.blobs.Put(context.Background(), events.RubricSnapshotKey(outcomeID),
		strings.NewReader("# Snapshotted rubric\n- from the file"), int64(len("# Snapshotted rubric\n- from the file")), "text/markdown"); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	h.wakeOutcomeRubric(t, "Build it", 3,
		map[string]any{"type": "file", "file_id": "file_0123456789abcdefghjkmnpq"}, outcomeID)
	h.drain(t)

	if evals := h.outcomes(t); evals[0].Result != domain.OutcomeResultSatisfied {
		t.Fatalf("entry = %+v, want satisfied", evals[0])
	}
	g := h.provider.calls[1]
	if !strings.Contains(g.System, "Snapshotted rubric") {
		t.Errorf("grader system missing the snapshot bytes: %q", g.System)
	}
}

// seedDeliverable inserts one harvested-snapshot registry row for the harness
// session (as the executor's outputs harvest leaves them) and, when the
// harness carries a store, its blob bytes.
func seedDeliverable(t *testing.T, h *harness, blobs *blobtest.MemStore, name, mimeType, content string) {
	t.Helper()
	id := domain.NewID(domain.PrefixFile)
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
		 VALUES ($1, $2, $3, $4, true, 'session', $5)`,
		id.String(), name, mimeType, len(content), h.sessionID.String()); err != nil {
		t.Fatal(err)
	}
	if blobs != nil {
		if err := blobs.Put(context.Background(), blob.FilesKey(id.String()),
			strings.NewReader(content), int64(len(content)), mimeType); err != nil {
			t.Fatal(err)
		}
	}
}

// graderUserText decodes the grader call's user message down to its text.
func graderUserText(t *testing.T, h *harness) string {
	t.Helper()
	if len(h.provider.calls) < 2 {
		t.Fatalf("provider calls = %d, want the agent turn and the grader call", len(h.provider.calls))
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(h.provider.calls[1].Messages[0].Content, &blocks); err != nil || len(blocks) == 0 {
		t.Fatalf("decode grader user content: %v", err)
	}
	return blocks[0].Text
}

func TestOutcomeGraderSeesDeliverables(t *testing.T) {
	h := newHarnessWithBlobs(t, [][]provider.Chunk{
		agentReply("work"),
		graderReply("deliverables meet the rubric", "satisfied"),
	}, nil)
	seedDeliverable(t, h.harness, h.blobs, "report.txt", "text/plain; charset=utf-8", "quarterly npv is 42")
	seedDeliverable(t, h.harness, h.blobs, "data.json", "application/json", `{"npv": 42}`)
	seedDeliverable(t, h.harness, h.blobs, "model.bin", "application/octet-stream", "\x01\x02raw-weights")

	h.wakeOutcome(t, "Ship the report", 3)
	h.drain(t)
	if evals := h.outcomes(t); evals[0].Result != domain.OutcomeResultSatisfied {
		t.Fatalf("entry = %+v, want satisfied", evals[0])
	}

	user := graderUserText(t, h.harness)
	di := strings.Index(user, "# Deliverables")
	if di < 0 {
		t.Fatalf("grader user message has no deliverables section: %q", user)
	}
	// Between the outcome and the transcript, so the grader reads what was
	// delivered before how it was made.
	if oi, ti := strings.Index(user, "# Outcome"), strings.Index(user, "# Agent transcript"); !(oi < di && di < ti) {
		t.Errorf("section order outcome=%d deliverables=%d transcript=%d, want outcome < deliverables < transcript", oi, di, ti)
	}
	// Every file is listed with its mime and size; text-like ones are inlined
	// whole, the binary one is listed only.
	for _, want := range []string{
		"report.txt (text/plain; charset=utf-8, 19 bytes)",
		"data.json (application/json, 11 bytes)",
		"model.bin (application/octet-stream, 13 bytes)",
		"quarterly npv is 42",
		`{"npv": 42}`,
	} {
		if !strings.Contains(user, want) {
			t.Errorf("grader user message missing %q", want)
		}
	}
	if strings.Contains(user, "raw-weights") {
		t.Errorf("binary deliverable's bytes were inlined")
	}
}

func TestOutcomeGraderDeliverablesBudgetIsGreedy(t *testing.T) {
	h := newHarnessWithBlobs(t, [][]provider.Chunk{
		agentReply("work"),
		graderReply("within budget", "satisfied"),
	}, nil)
	// Greedy whole-file inlining in filename order: a (60 KiB) fits the
	// 64 KiB budget, b (10 KiB) no longer fits, c (3 KiB) still does.
	big := strings.Repeat("a", 60<<10) + "MARKER-A"
	mid := strings.Repeat("b", 10<<10) + "MARKER-B"
	small := strings.Repeat("c", 3<<10) + "MARKER-C"
	seedDeliverable(t, h.harness, h.blobs, "a.txt", "text/plain", big)
	seedDeliverable(t, h.harness, h.blobs, "b.txt", "text/plain", mid)
	seedDeliverable(t, h.harness, h.blobs, "c.txt", "text/plain", small)

	h.wakeOutcome(t, "Ship it", 3)
	h.drain(t)

	user := graderUserText(t, h.harness)
	if !strings.Contains(user, "MARKER-A") || !strings.Contains(user, "MARKER-C") {
		t.Errorf("in-budget deliverables not inlined")
	}
	if strings.Contains(user, "MARKER-B") {
		t.Errorf("over-budget deliverable was inlined")
	}
	if !strings.Contains(user, "b.txt (text/plain, ") {
		t.Errorf("over-budget deliverable missing from the listing")
	}
}

func TestOutcomeGraderDeliverablesWithoutBlobStoreListsOnly(t *testing.T) {
	// No blob store (the default harness): the registry listing still reaches
	// the grader, contents do not.
	h := newHarness(t, [][]provider.Chunk{
		agentReply("work"),
		graderReply("judged from the listing", "satisfied"),
	}, nil)
	seedDeliverable(t, h, nil, "report.txt", "text/plain", "quarterly npv is 42")

	h.wakeOutcome(t, "Ship the report", 3)
	h.drain(t)

	user := graderUserText(t, h)
	if !strings.Contains(user, "report.txt (text/plain, 19 bytes)") {
		t.Errorf("listing missing without a blob store: %q", user)
	}
	if strings.Contains(user, "quarterly npv is 42") {
		t.Errorf("content inlined despite no blob store")
	}
}

func TestOutcomeGraderNoDeliverablesNoSection(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		agentReply("work"),
		graderReply("fine", "satisfied"),
	}, nil)
	h.wakeOutcome(t, "Ship it", 3)
	h.drain(t)

	if user := graderUserText(t, h); strings.Contains(user, "# Deliverables") {
		t.Errorf("empty registry produced a deliverables section: %q", user)
	}
}

func TestOutcomeGraderExplanationTruncatesOnRuneBoundary(t *testing.T) {
	// 21000 bytes of 3-byte runes: the 20 KiB explanation cut lands mid-rune,
	// and a byte-sliced tail would persist as a replacement character.
	long := strings.Repeat("中", 7000)
	h := newHarness(t, [][]provider.Chunk{
		agentReply("work"),
		graderReply(long, "satisfied"),
	}, nil)
	h.wakeOutcome(t, "Ship it", 3)
	h.drain(t)

	e := h.outcomes(t)[0]
	if e.Result != domain.OutcomeResultSatisfied {
		t.Fatalf("entry = %+v, want satisfied", e)
	}
	if !strings.HasSuffix(e.Explanation, "[truncated]") {
		t.Errorf("explanation not truncated (len %d)", len(e.Explanation))
	}
	if !utf8.ValidString(e.Explanation) {
		t.Errorf("truncated explanation is not valid UTF-8")
	}
}

func TestOutcomeGraderDeliverableSizeMismatchListsOnly(t *testing.T) {
	// A blob whose length disagrees with its registry row is listed but never
	// inlined: the row's size bounds the read (the rubricText posture), so a
	// lying row can neither balloon memory nor inline a file truncated.
	h := newHarnessWithBlobs(t, [][]provider.Chunk{
		agentReply("work"),
		graderReply("judged from the listing", "satisfied"),
	}, nil)
	id := domain.NewID(domain.PrefixFile)
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable, scope_type, scope_id)
		 VALUES ($1, 'liar.txt', 'text/plain', 5, true, 'session', $2)`,
		id.String(), h.sessionID.String()); err != nil {
		t.Fatal(err)
	}
	longer := "0123456789-far-longer-than-five-bytes"
	if err := h.blobs.Put(context.Background(), blob.FilesKey(id.String()),
		strings.NewReader(longer), int64(len(longer)), "text/plain"); err != nil {
		t.Fatal(err)
	}

	h.wakeOutcome(t, "Ship the report", 3)
	h.drain(t)

	user := graderUserText(t, h.harness)
	if !strings.Contains(user, "liar.txt (text/plain, 5 bytes)") {
		t.Fatalf("mismatched deliverable not listed: %q", user)
	}
	if strings.Contains(user, "## liar.txt") || strings.Contains(user, longer) {
		t.Errorf("mismatched deliverable was inlined: %q", user)
	}
}

// The idle trigger's first site (docs/plan/38, #263): a cloud session that ends
// its turn and folds idle harvests its deliverables with no outcome anywhere in
// the picture — which is what the reference docs describe, and what a session
// that never sent user.define_outcome got nothing of before.
func TestEndTurnWithNoOutcomeHarvestsAtIdle(t *testing.T) {
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{agentReply("wrote the deck")}, nil)
	h.wake(t, "build me a deck")
	h.runOnce(t)

	if got := h.status(t); got != "idle" {
		t.Fatalf("status = %q, want idle", got)
	}
	if n := h.liveOf(t, queue.OutputsHarvest); n != 1 {
		t.Fatalf("live outputs_harvest = %d, want 1", n)
	}
	if h.harvestChainGrading(t) {
		t.Error("chain_grading = true; an idle with no outcome chains no grading turn")
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 0 {
		t.Errorf("live model_turn = %d, want 0 (nothing is waiting on this snapshot)", n)
	}
}

// self_hosted harvests on neither trigger: the platform cannot reach a BYOC
// sandbox, whatever scheduled the pass (docs/DIVERGENCES.md's self_hosted entry).
func TestEndTurnOnSelfHostedNeverHarvests(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{agentReply("wrote the deck")}, nil)
	h.wake(t, "build me a deck")
	h.runOnce(t)

	if got := h.status(t); got != "idle" {
		t.Fatalf("status = %q, want idle", got)
	}
	if n := h.liveOf(t, queue.OutputsHarvest); n != 0 {
		t.Errorf("live outputs_harvest = %d, want 0 (no file lane on self_hosted)", n)
	}
}

// A woken ending is not an idle one: a child that ends without reporting wakes
// its coordinator in the same commit, so the session folds to running and
// nothing is harvested. The gate needs no separate guard for this — `woke` and
// a session-level fold to idle are structurally exclusive — and this pins it.
func TestChildEndingThatWakesItsCoordinatorSkipsHarvest(t *testing.T) {
	h := newHarnessEnv(t, "cloud", [][]provider.Chunk{agentReply("I looked, and found nothing")}, nil)
	child := h.childTurn(t, "find the papers")
	h.runOnce(t)

	if s := h.threadStatus(t, child); s != "idle" {
		t.Errorf("child = %q, want idle on its own end_turn", s)
	}
	if s := h.status(t); s != "running" {
		t.Fatalf("session = %q, want running with its woken coordinator", s)
	}
	if n := h.liveOf(t, queue.OutputsHarvest); n != 0 {
		t.Errorf("live outputs_harvest = %d, want 0 (the session did not idle)", n)
	}
}
