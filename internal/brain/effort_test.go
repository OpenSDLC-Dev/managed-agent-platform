package brain_test

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

func TestEffortPrimaryChildAndGrader(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		agentReply("coordinator done"), agentReply("child done"), graderReply("all done", "satisfied"),
	}, nil)
	ctx := context.Background()
	// Each turn must read its own persisted snapshot, never the coordinator's
	// setting for a child, nor an unconfigured request for the grader.
	if _, err := h.pool.Exec(ctx, `UPDATE sessions SET resolved_agent = jsonb_set(resolved_agent, '{model}', '{"id":"custom","effort":{"type":"high"}}') WHERE id=$1`, h.sessionID.String()); err != nil {
		t.Fatal(err)
	}
	h.wakeOutcome(t, "finish", 2)
	child := h.childTurn(t, "finish child")
	if _, err := h.pool.Exec(ctx, `UPDATE session_threads SET agent = jsonb_set(agent, '{model}', '{"id":"custom","effort":{"type":"low"}}') WHERE id=$1`, child.String()); err != nil {
		t.Fatal(err)
	}
	h.drain(t)
	if len(h.provider.calls) != 3 {
		t.Fatalf("calls = %d, want primary + child + grader", len(h.provider.calls))
	}
	for i, want := range []domain.ModelEffort{"high", "low", "high"} {
		if got := h.provider.calls[i].Effort; got != want {
			t.Errorf("call %d effort = %q, want %q", i, got, want)
		}
	}
}
