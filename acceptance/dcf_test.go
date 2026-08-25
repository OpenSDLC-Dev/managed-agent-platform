package acceptance

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/respjson"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// The reference example's constants, byte-for-byte from the define-outcomes
// page's Go tab (fetched 2026-08-02; pinned in docs/plan/21_outcomes.md).
const (
	dcfRubric = `# DCF Model Rubric

## Revenue Projections
- Uses historical revenue data from the last 5 fiscal years
- Projects revenue for at least 5 years forward

## Output Quality
- All figures are in a single .xlsx file with clearly labeled sheets
`
	dcfDescription = "Build a DCF model for Costco in .xlsx"
	dcfTitle       = "Financial analysis on Costco"
)

// The rehearsal's deliverable: what the scripted agent writes and what the
// download must return byte-identically.
const (
	deliverableName  = "costco_dcf.xlsx"
	deliverableBytes = "PK-fake-costco-dcf-workbook-rehearsal"
)

// dcfRun is everything one replay of the example observed, for the caller's
// leg-specific assertions on top of runDCF's structural ones.
type dcfRun struct {
	SessionID   string
	OutcomeID   string
	Terminal    string   // the terminal outcome_evaluations[].result
	Explanation string   // the grader's verdict text from the session resource
	EndResults  []string // span.outcome_evaluation_end results in stream order
	SawOngoing  bool     // whether a span.outcome_evaluation_ongoing heartbeat arrived
	Files       []anthropic.BetaFileMetadata
	Contents    map[string][]byte // filename -> downloaded bytes
}

// terminalResults are the outcome_evaluations[].result values that end an
// outcome (the SDK's documented terminal set).
var terminalResults = map[string]bool{
	"satisfied": true, "max_iterations_reached": true, "failed": true, "interrupted": true,
}

// runDCF replays the define-outcomes example step-for-step through the SDK:
// upload rubric.md, create environment + agent + session, subscribe to the
// event stream, send user.define_outcome, watch the span trio to a terminal
// evaluation, poll the session resource to its terminal result, then list and
// download the session-scoped deliverables. It asserts every structural
// invariant common to both legs; leg-specific expectations (exact bytes, a
// specific verdict) belong to the caller. track, when non-nil, receives the
// session id for teardown.
func runDCF(t *testing.T, client anthropic.Client, track func(string), agentModel string, fileRubric bool, deadline time.Duration) *dcfRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	// Step 1 — upload the rubric through the Files API (the doc's "Create a
	// rubric" step; both variants upload, only the file variant references it).
	rubricPath := filepath.Join(t.TempDir(), "rubric.md")
	if err := os.WriteFile(rubricPath, []byte(dcfRubric), 0o644); err != nil {
		t.Fatalf("write rubric.md: %v", err)
	}
	rf, err := os.Open(rubricPath)
	if err != nil {
		t.Fatalf("open rubric.md: %v", err)
	}
	defer rf.Close()
	uploaded, err := client.Beta.Files.Upload(ctx, anthropic.BetaFileUploadParams{
		File: anthropic.File(rf, "rubric.md", "text/markdown"),
	})
	if err != nil {
		t.Fatalf("upload rubric: %v", err)
	}
	if !strings.HasPrefix(uploaded.ID, "file_") {
		t.Fatalf("uploaded rubric id = %q, want a file_ id", uploaded.ID)
	}

	// The example assumes an agent and an environment "created separately" —
	// create them the same way, through the SDK.
	env, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
		Name: "acceptance-cloud",
	})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
		Name:  "dcf-analyst",
		Model: anthropic.BetaManagedAgentsModelConfigParams{ID: agentModel},
		Tools: []anthropic.BetaAgentNewParamsToolUnion{{
			OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
				Type: anthropic.BetaManagedAgentsAgentToolset20260401ParamsTypeAgentToolset20260401,
			},
		}},
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	// Step 2 — create the session (doc-verbatim params).
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfString: anthropic.String(agent.ID),
		},
		EnvironmentID: env.ID,
		Title:         anthropic.String(dcfTitle),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if track != nil {
		track(session.ID)
	}

	// Subscribe before sending: the stream is a live tail (the handler
	// snapshots the log position at connect), so a stream opened after the
	// define_outcome could miss the whole run including the idle.
	watch := watchStream(ctx, client, session.ID)

	// Step 3 — send user.define_outcome; the agent starts on receipt.
	rubric := anthropic.BetaManagedAgentsUserDefineOutcomeEventParamsRubricUnion{
		OfText: &anthropic.BetaManagedAgentsTextRubricParams{
			Type:    anthropic.BetaManagedAgentsTextRubricParamsTypeText,
			Content: dcfRubric,
		},
	}
	if fileRubric {
		rubric = anthropic.BetaManagedAgentsUserDefineOutcomeEventParamsRubricUnion{
			OfFile: &anthropic.BetaManagedAgentsFileRubricParams{
				Type:   anthropic.BetaManagedAgentsFileRubricParamsTypeFile,
				FileID: uploaded.ID,
			},
		}
	}
	sent, err := client.Beta.Sessions.Events.Send(ctx, session.ID, anthropic.BetaSessionEventSendParams{
		Events: []anthropic.BetaManagedAgentsEventParamsUnion{{
			OfUserDefineOutcome: &anthropic.BetaManagedAgentsUserDefineOutcomeEventParams{
				Type:          anthropic.BetaManagedAgentsUserDefineOutcomeEventParamsTypeUserDefineOutcome,
				Description:   dcfDescription,
				Rubric:        rubric,
				MaxIterations: anthropic.Int(5),
			},
		}},
	})
	if err != nil {
		t.Fatalf("send user.define_outcome: %v", err)
	}
	if len(sent.Data) != 1 {
		t.Fatalf("send echoed %d events, want 1", len(sent.Data))
	}
	echo := sent.Data[0]
	if echo.Type != "user.define_outcome" {
		t.Fatalf("echo type = %q, want user.define_outcome", echo.Type)
	}
	if !strings.HasPrefix(echo.OutcomeID, "outc_") {
		t.Fatalf("echo outcome_id = %q, want an outc_ id", echo.OutcomeID)
	}
	if echo.Description != dcfDescription || echo.MaxIterations != 5 {
		t.Fatalf("echo carried description %q / max_iterations %d, want %q / 5",
			echo.Description, echo.MaxIterations, dcfDescription)
	}

	// Step 4 — watch the stream to the terminal evaluation and the idle that
	// follows it.
	select {
	case <-watch.done:
	case <-ctx.Done():
		t.Fatalf("no terminal outcome evaluation within %s; frames seen: %s", deadline, watch.typesSeen())
	}
	if err := watch.streamErr(); err != nil {
		t.Fatalf("event stream failed: %v", err)
	}
	run := &dcfRun{SessionID: session.ID, OutcomeID: echo.OutcomeID, Contents: map[string][]byte{}}
	watch.verifySpans(t, run)

	// Step 5 — poll the session resource to the terminal result (the doc's
	// "Check outcome status" step).
	eval := pollTerminal(ctx, t, client, session.ID, echo.OutcomeID)
	run.Terminal = eval.Result
	run.Explanation = eval.Explanation

	// Step 6 — list and download the session-scoped deliverables (the doc's
	// "Retrieve deliverables" step, beta header and all).
	page, err := client.Beta.Files.List(ctx, anthropic.BetaFileListParams{
		ScopeID: anthropic.String(session.ID),
		Betas:   []anthropic.AnthropicBeta{anthropic.AnthropicBetaManagedAgents2026_04_01},
	})
	if err != nil {
		t.Fatalf("list session files: %v", err)
	}
	for _, fm := range page.Data {
		assertNoExtras(t, "file "+fm.ID, fm.JSON.ExtraFields)
		if !fm.Downloadable {
			t.Errorf("deliverable %s (%s) is not downloadable", fm.ID, fm.Filename)
		}
		resp, err := client.Beta.Files.Download(ctx, fm.ID, anthropic.BetaFileDownloadParams{})
		if err != nil {
			t.Fatalf("download %s: %v", fm.ID, err)
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("read %s: %v", fm.ID, err)
		}
		if int64(len(data)) != fm.SizeBytes {
			t.Errorf("download %s returned %d bytes, metadata says %d", fm.ID, len(data), fm.SizeBytes)
		}
		run.Files = append(run.Files, fm)
		run.Contents[fm.Filename] = data
	}
	// A satisfied outcome with nothing harvested is a hollow pass: the grader
	// judged a deliverable the platform never collected (the first live run
	// did exactly that — the agent wrote outside /mnt/session/outputs/ and
	// "satisfied" arrived with zero files).
	if run.Terminal == "satisfied" && len(run.Files) == 0 {
		t.Errorf("outcome satisfied but no deliverables were harvested for session %s", session.ID)
	}
	return run
}

// pollTerminal polls GET /v1/sessions/{id} until outcome_evaluations carries a
// terminal result for outcomeID, asserting the typed shape along the way.
func pollTerminal(ctx context.Context, t *testing.T, client anthropic.Client, sessionID, outcomeID string) anthropic.BetaManagedAgentsOutcomeEvaluationResource {
	t.Helper()
	for {
		sess, err := client.Beta.Sessions.Get(ctx, sessionID, anthropic.BetaSessionGetParams{})
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		assertNoExtras(t, "session resource", sess.JSON.ExtraFields)
		if !sess.JSON.OutcomeEvaluations.Valid() {
			t.Fatalf("session resource carries no outcome_evaluations field")
		}
		if n := len(sess.OutcomeEvaluations); n != 1 {
			t.Fatalf("outcome_evaluations has %d entries, want 1", n)
		}
		eval := sess.OutcomeEvaluations[0]
		assertNoExtras(t, "outcome evaluation", eval.JSON.ExtraFields)
		if eval.OutcomeID != outcomeID {
			t.Fatalf("outcome_evaluations[0].outcome_id = %q, want %q", eval.OutcomeID, outcomeID)
		}
		if terminalResults[eval.Result] {
			if string(eval.Type) != "outcome_evaluation" {
				t.Errorf("evaluation entry type = %q, want outcome_evaluation", eval.Type)
			}
			if eval.Explanation == "" {
				t.Errorf("terminal evaluation carries no explanation")
			}
			if !eval.JSON.CompletedAt.Valid() {
				t.Errorf("terminal evaluation carries no completed_at")
			}
			return eval
		}
		select {
		case <-ctx.Done():
			t.Fatalf("outcome_evaluations never reached a terminal result; last saw %q", eval.Result)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// streamWatch tails the session's SSE stream through the SDK's typed decoder,
// completing once a terminal span.outcome_evaluation_end has been followed by
// session.status_idle (max_iterations_reached runs one acknowledgment turn
// between the two; needs_revision ends keep the watch open).
type streamWatch struct {
	mu     sync.Mutex
	frames []anthropic.BetaManagedAgentsStreamSessionEventsUnion
	err    error
	done   chan struct{}
}

func watchStream(ctx context.Context, client anthropic.Client, sessionID string) *streamWatch {
	stream := client.Beta.Sessions.Events.StreamEvents(ctx, sessionID, anthropic.BetaSessionEventStreamParams{})
	w := &streamWatch{done: make(chan struct{})}
	go func() {
		defer close(w.done)
		defer stream.Close()
		terminal := false
		for stream.Next() {
			ev := stream.Current()
			w.mu.Lock()
			w.frames = append(w.frames, ev)
			w.mu.Unlock()
			switch ev.Type {
			case "span.outcome_evaluation_end":
				if ev.Result != "needs_revision" {
					terminal = true
				}
			case "session.status_idle":
				if terminal {
					return
				}
			}
		}
		w.mu.Lock()
		w.err = stream.Err()
		w.mu.Unlock()
	}()
	return w
}

func (w *streamWatch) streamErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// typesSeen summarizes the frame sequence for a timeout message.
func (w *streamWatch) typesSeen() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	types := make([]string, len(w.frames))
	for i, f := range w.frames {
		types[i] = f.Type
	}
	return strings.Join(types, " ")
}

// verifySpans checks the outcome span trio as the stream delivered it: a start
// (iteration 0, the right outcome) before any end, ends whose
// outcome_evaluation_start_id points at an observed start, and records the
// results and whether a heartbeat arrived.
func (w *streamWatch) verifySpans(t *testing.T, run *dcfRun) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	starts := map[string]bool{}
	for _, f := range w.frames {
		switch f.Type {
		case "span.outcome_evaluation_start":
			if f.OutcomeID != run.OutcomeID {
				t.Errorf("evaluation start names outcome %q, want %q", f.OutcomeID, run.OutcomeID)
			}
			if len(starts) == 0 && f.Iteration != 0 {
				t.Errorf("first evaluation start carries iteration %d, want 0", f.Iteration)
			}
			if !strings.HasPrefix(f.ID, "sevt_") {
				t.Errorf("evaluation start id = %q, want a sevt_ id", f.ID)
			}
			starts[f.ID] = true
		case "span.outcome_evaluation_ongoing":
			run.SawOngoing = true
		case "span.outcome_evaluation_end":
			if f.OutcomeID != run.OutcomeID {
				t.Errorf("evaluation end names outcome %q, want %q", f.OutcomeID, run.OutcomeID)
			}
			if f.Result != "interrupted" && !starts[f.OutcomeEvaluationStartID] {
				t.Errorf("evaluation end points at start %q, which the stream never delivered", f.OutcomeEvaluationStartID)
			}
			run.EndResults = append(run.EndResults, f.Result)
		}
	}
	if len(starts) == 0 {
		t.Errorf("the stream delivered no span.outcome_evaluation_start")
	}
	if len(run.EndResults) == 0 {
		t.Errorf("the stream delivered no span.outcome_evaluation_end")
	}
}

// assertNoExtras is the acceptance posture in one helper: a field on the wire
// the SDK's typed struct does not know is a wire-compat failure.
//
// It checks that one direction only. Plan 21's acceptance also called for the
// inverse — that every field the SDK marks api:"required" is present
// (docs/plan/21_outcomes.md) — and that half was never built; nothing here
// asserts it. Said here rather than left in the plan alone, because the gap is
// real: a response omitting a required field passes this suite today.
func assertNoExtras(t *testing.T, what string, extras map[string]respjson.Field) {
	t.Helper()
	for k := range extras {
		t.Errorf("%s: the wire carried a field the SDK does not know: %q", what, k)
	}
}

// Scripted-chunk helpers, mirroring the brain suite's.

func textChunk(idx int64, s string) provider.Chunk {
	return provider.Chunk{Kind: provider.KindTextDelta, Index: idx, Text: s}
}

func toolUseChunk(id, name, input string) provider.Chunk {
	return provider.Chunk{Kind: provider.KindToolUse,
		ToolUse: &provider.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)}}
}

func doneChunk(stop string, out int64) provider.Chunk {
	return provider.Chunk{Kind: provider.KindDone, StopReason: stop,
		Usage: &domain.ModelUsage{InputTokens: 10, OutputTokens: out}}
}

// The rehearsal: the full pipeline — REST accept, wake, model turn, bash in a
// real Docker sandbox, the outputs harvest, the chained grading pass, the
// projection flip, the Files API — with only the model scripted. One test per
// rubric variant, each on a fresh stack.

func TestRehearsalDefineOutcomesTextRubric(t *testing.T) { rehearse(t, false) }
func TestRehearsalDefineOutcomesFileRubric(t *testing.T) { rehearse(t, true) }

func rehearse(t *testing.T, fileRubric bool) {
	scripts := [][]provider.Chunk{
		// The agent's working turn: write the deliverable, stop on the tool call.
		{toolUseChunk("toolu_dcf_01", "bash",
			`{"command":"mkdir -p /mnt/session/outputs && printf '%s' '`+deliverableBytes+`' > /mnt/session/outputs/`+deliverableName+`"}`),
			doneChunk("tool_use", 40)},
		// The closing turn: end_turn settlement chains the harvest, then grading.
		{textChunk(0, "The DCF workbook is at /mnt/session/outputs/"+deliverableName+"."),
			doneChunk("end_turn", 25)},
		// The grader, speaking the platform's verdict protocol.
		{textChunk(0, "Revenue projections and output quality are covered by the delivered workbook.\n\nVERDICT: satisfied"),
			doneChunk("end_turn", 30)},
	}
	s := newStack(t, scripts)
	client := anthropic.NewClient(option.WithAPIKey(acceptanceKey), option.WithBaseURL(s.url))

	run := runDCF(t, client, s.track, "acceptance-scripted", fileRubric, 2*time.Minute)

	if run.Terminal != "satisfied" {
		t.Fatalf("terminal result = %q (explanation %q), want satisfied", run.Terminal, run.Explanation)
	}
	if len(run.EndResults) != 1 || run.EndResults[0] != "satisfied" {
		t.Errorf("stream end results = %v, want exactly [satisfied]", run.EndResults)
	}
	if run.SawOngoing {
		t.Errorf("scripted grading is instant; no ongoing heartbeat should have fired")
	}
	if !strings.Contains(run.Explanation, "Revenue projections") {
		t.Errorf("explanation %q does not carry the grader's verdict text", run.Explanation)
	}
	if len(run.Files) != 1 {
		t.Fatalf("session-scoped files = %d, want exactly the one deliverable; files: %v", len(run.Files), run.Files)
	}
	if run.Files[0].Filename != deliverableName {
		t.Errorf("deliverable filename = %q, want %q", run.Files[0].Filename, deliverableName)
	}
	if got := string(run.Contents[deliverableName]); got != deliverableBytes {
		t.Errorf("downloaded deliverable = %q, want the byte-identical %q", got, deliverableBytes)
	}
}
