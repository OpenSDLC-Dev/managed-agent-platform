package brain_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/jackc/pgx/v5"
)

// --- the brain's half of user.interrupt (#68) ---

// interrupt mimics the control plane's user.interrupt trigger, the way wake
// mimics its user.message one: under the session row lock it answers every
// outstanding tool call, ends the turn on a session.status_idle carrying
// end_turn, settles the session idle, and cancels the session's work items — all
// in the one transaction, so a claimant serialized behind the same lock sees the
// whole decision or none of it.
//
// The answers come from the same events.UnansweredToolUses/InterruptResults pair
// the API calls, not a copy: a stand-in that synthesized its own results would
// keep passing through a regression in the family matching or the stamping.
func (h *harness) interrupt(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, h.sessionID.String()); err != nil {
		t.Fatalf("interrupt: lock session: %v", err)
	}
	abandoned, err := events.UnansweredToolUses(ctx, tx, h.sessionID, nil)
	if err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	results, err := events.InterruptResults(abandoned)
	if err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	batch := []events.NewEvent{{Type: domain.EventUserInterrupt, Payload: []byte(`{"session_thread_id":null}`)}}
	batch = append(batch, results...)
	batch = append(batch, events.StatusChange(h.sessionID, domain.SessionIdle, &domain.StopReason{Type: domain.StopEndTurn})...)
	idle := domain.SessionIdle
	if _, err := h.log.AppendInTx(ctx, tx, h.sessionID, batch, events.AppendOptions{
		SetStatus: &idle,
		Then: func(ctx context.Context, tx pgx.Tx) error {
			return h.queue.CancelSession(ctx, tx, h.sessionID)
		},
	}); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
}

func TestInterruptedTurnCommitsNothing(t *testing.T) {
	// The integrity half of the escape hatch, and the reason it can cancel work
	// out from under a live claimant at all: a brain whose item an interrupt
	// took away settles into a lost lease and rolls its whole turn back. Were it
	// to land, the settlement would re-open exactly the dead end the interrupt
	// just closed — tool intents committed onto an idle session, with nothing
	// scheduled that could ever answer them.
	h := newHarness(t, [][]provider.Chunk{
		{toolUseChunk("toolu_x", "lookup"), done("tool_use", 3)},
	}, nil)
	h.provider.onGenerate = func(int) { h.interrupt(t) }

	h.wake(t, "start something long")
	found, err := h.brain.RunOnce(context.Background())
	if !found || err == nil {
		t.Fatalf("RunOnce = (%v, %v), want found with a lost-lease error", found, err)
	}

	// span.model_request_start is the turn's only trace on the log: it commits
	// before the model is called, outside the settlement the interrupt undoes.
	want := []string{
		"user.message", "session.status_running", "span.model_request_start",
		"user.interrupt", "session.status_idle",
	}
	if got := h.types(t); !typesEqual(got, want) {
		t.Errorf("interrupted turn committed output:\n got %v\nwant %v", got, want)
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle — the interrupt settled the session", got)
	}
	if got := h.liveWork(t); got != 0 {
		t.Errorf("live work items = %d, want 0", got)
	}
}

func TestReplayAfterAnInterruptIsValid(t *testing.T) {
	// The claim the synthesized results exist for: the log is append-only, so an
	// abandoned tool call would ride every future replay as a tool_use no
	// tool_result answers — a request the model protocol rejects. After an
	// interrupt the next turn's request must pair each call with its answer.
	h := newHarness(t, [][]provider.Chunk{
		{toolUseChunk("toolu_x", "bash"), done("tool_use", 3)},
		{textChunk(0, "on it"), done("end_turn", 2)},
	}, nil)
	agentJSON := `{"type":"agent","id":"agent_x","version":1,"name":"n",
		"model":{"id":"fixture-model"},"system":"do the task","description":"",
		"tools":[{"type":"agent_toolset_20260401"}],
		"mcp_servers":[],"skills":[],"multiagent":null}`
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = $2 WHERE id = $1`, h.sessionID.String(), agentJSON); err != nil {
		t.Fatal(err)
	}

	h.wake(t, "list the files")
	h.runOnce(t)
	// Suspended on a platform tool, with a tool_exec item nothing will run.
	if got := h.liveOf(t, queue.ToolExec); got != 1 {
		t.Fatalf("tool_exec items = %d, want 1", got)
	}
	evs, err := h.log.List(context.Background(), h.sessionID, events.ListQuery{Types: []string{"agent.tool_use"}})
	if err != nil || len(evs) != 1 {
		t.Fatalf("agent.tool_use events = %d (%v)", len(evs), err)
	}
	toolEventID := evs[0].ID.String()

	h.interrupt(t)
	if got := h.liveOf(t, queue.ToolExec); got != 0 {
		t.Errorf("tool_exec items after interrupt = %d, want 0", got)
	}

	// The session is resumable: a new message runs a turn whose replay is whole.
	h.wake(t, "do something else instead")
	h.runOnce(t)

	req := h.provider.calls[1]
	if len(req.Messages) != 3 {
		t.Fatalf("resumed request has %d messages: %+v", len(req.Messages), req.Messages)
	}
	var assistant []map[string]any
	_ = json.Unmarshal(req.Messages[1].Content, &assistant)
	if req.Messages[1].Role != "assistant" || assistant[0]["type"] != "tool_use" || assistant[0]["id"] != toolEventID {
		t.Fatalf("assistant turn = %v", assistant)
	}
	// The tool_result answering it sorts ahead of the redirect text, which is
	// what the Messages API requires of a user turn.
	var user []map[string]any
	_ = json.Unmarshal(req.Messages[2].Content, &user)
	if req.Messages[2].Role != "user" || len(user) != 2 {
		t.Fatalf("user turn = %v", user)
	}
	if user[0]["type"] != "tool_result" || user[0]["tool_use_id"] != toolEventID || user[0]["is_error"] != true {
		t.Errorf("tool_result block = %v", user[0])
	}
	if user[1]["type"] != "text" || user[1]["text"] != "do something else instead" {
		t.Errorf("redirect block = %v", user[1])
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status after the redirect turn = %q, want idle", got)
	}
}
