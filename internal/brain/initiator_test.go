package brain_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

func TestRefusedInitiatorDoesNoModelTurnWork(t *testing.T) {
	for _, mode := range []string{"direct provider", "MCP discovery", "grading", "missing initiator"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, [][]provider.Chunk{{done("end_turn", 1)}, {done("end_turn", 1)}}, nil)
			ctx := t.Context()
			if _, err := h.pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject,status) VALUES ('principal_removed','test','removed','deactivated');
				UPDATE sessions SET created_by='principal_removed',initiator_principal_id=NULL,initiator_api_key_id=NULL`); err != nil {
				t.Fatal(err)
			}
			if mode == "missing initiator" {
				if _, err := h.pool.Exec(ctx, `UPDATE sessions SET created_by=NULL`); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "MCP discovery" {
				mcpAgent(t, h, `{"type":"mcp_toolset","mcp_server_name":"docs"}`)
			}
			if mode == "grading" {
				h.wakeOutcome(t, "finish the work", 1)
				if _, err := h.pool.Exec(ctx, `UPDATE sessions SET outcome_evaluations=jsonb_set(outcome_evaluations,'{0,result}','"evaluating"')`); err != nil {
					t.Fatal(err)
				}
			} else {
				h.wake(t, "first input")
			}
			spans := recordBrainSpans(t)
			h.runOnce(t)
			if len(h.provider.calls) != 0 || h.liveOf(t, queue.MCPExec) != 0 {
				t.Fatal("refused turn reached a provider or MCP discovery")
			}
			if h.countType(t, "session.error") != 1 || h.countType(t, "span.model_request_start") != 0 || h.retryStatus(t) != "exhausted" {
				t.Fatalf("refusal events: %v", h.types(t))
			}
			if h.status(t) != "idle" {
				t.Fatalf("refused session stayed %s", h.status(t))
			}
			if mode == "grading" && h.outcomes(t)[0].Result != domain.OutcomeResultRunning {
				t.Fatalf("refused grader left outcome %s", h.outcomes(t)[0].Result)
			}
			for _, s := range spans() {
				if s.Name() == "model_turn" || strings.Contains(s.Name(), "model_request") {
					t.Fatalf("refused turn opened span %s", s.Name())
				}
			}
			h.wake(t, "another input")
			h.runOnce(t)
			if len(h.provider.calls) != 0 || h.countType(t, "session.error") != 2 {
				t.Fatal("subsequent turn escaped the membership check")
			}
			for _, m := range h.messages(t) {
				if m.ProcessedAt == nil {
					t.Fatal("refusal left its input unprocessed")
				}
			}
			if found, err := h.brain.RunOnce(ctx); err != nil || found {
				t.Fatalf("refusal looped without another input: %t %v", found, err)
			}
		})
	}
}

func TestMemberDemotionDoesNotStopModelTurns(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{done("end_turn", 1)}, {done("end_turn", 1)}}, nil)
	if _, err := h.pool.Exec(t.Context(), `INSERT INTO principals(id,issuer,subject,role) VALUES ('principal_member','test','member','developer');
		UPDATE sessions SET initiator_principal_id='principal_member',initiator_api_key_id=NULL`); err != nil {
		t.Fatal(err)
	}
	h.wake(t, "before demotion")
	h.runOnce(t)
	if _, err := h.pool.Exec(t.Context(), `UPDATE principals SET role='user' WHERE id='principal_member'`); err != nil {
		t.Fatal(err)
	}
	h.wake(t, "after demotion")
	h.runOnce(t)
	if len(h.provider.calls) != 2 || h.countType(t, "session.error") != 0 {
		t.Fatalf("demotion stopped existing work: %v", h.types(t))
	}
}

func TestRemovedInitiatorCanFinishQueuedToolsBeforeRefusedTurn(t *testing.T) {
	h := newHarnessEnv(t, "self_hosted", [][]provider.Chunk{{
		{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{ID: "toolu_queued", Name: "bash", Input: json.RawMessage(`{"command":"true"}`)}},
		done("tool_use", 1),
	}}, nil)
	if _, err := h.pool.Exec(t.Context(), `INSERT INTO principals(id,issuer,subject,role) VALUES ('principal_member','test','member','developer');
		UPDATE sessions SET initiator_principal_id='principal_member',initiator_api_key_id=NULL,
		resolved_agent=jsonb_set(resolved_agent,'{tools}','[{"type":"agent_toolset_20260401"}]')`); err != nil {
		t.Fatal(err)
	}
	h.wake(t, "queue a tool")
	h.runOnce(t)
	uses, err := h.log.List(t.Context(), h.sessionID, events.ListQuery{Types: []string{"agent.tool_use"}})
	if err != nil || len(uses) != 1 {
		t.Fatalf("tool intent: %d %v", len(uses), err)
	}
	if _, err := h.pool.Exec(t.Context(), `UPDATE principals SET status='deactivated' WHERE id='principal_member'`); err != nil {
		t.Fatal(err)
	}
	work, err := h.queue.Poll(t.Context(), h.envID, time.Minute)
	if err != nil || work == nil {
		t.Fatalf("removed member's queued tool: %v", err)
	}
	h.postToolResult(t, domain.EventUserToolResult, map[string]any{"tool_use_id": uses[0].ID.String(), "content": []map[string]string{{"type": "text", "text": "completed"}}})
	h.runOnce(t)
	if len(h.provider.calls) != 1 || h.countType(t, "user.tool_result") != 1 || h.countType(t, "session.error") != 1 {
		t.Fatalf("tool result/refusal: %v", h.types(t))
	}
}
