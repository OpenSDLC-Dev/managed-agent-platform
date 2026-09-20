package executor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

func TestCloudResultBeforeLastCustom(t *testing.T) {
	for _, betaFirst := range []bool{false, true} {
		name := "beta_after_cloud"
		if betaFirst {
			name = "beta_before_alpha"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, &fakeSandbox{})
			ctx := context.Background()
			alpha := domain.NewID("sevt")
			beta := domain.NewID("sevt")
			_, err := h.log.AppendWith(ctx, h.sid, []events.NewEvent{
				{ID: alpha, Type: domain.EventAgentCustomToolUse, Payload: json.RawMessage(`{"name":"alpha","input":{}}`)},
				{Type: domain.EventAgentToolUse, Payload: json.RawMessage(writeUse("out.txt", "hello"))},
				{ID: beta, Type: domain.EventAgentCustomToolUse, Payload: json.RawMessage(`{"name":"beta","input":{}}`)},
			}, events.AppendOptions{})
			if err != nil {
				t.Fatal(err)
			}
			tx, err := h.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if betaFirst {
				body, _ := json.Marshal(map[string]any{"custom_tool_use_id": beta, "content": []any{map[string]any{"type": "text", "text": "beta"}}})
				if _, err = h.log.AppendInTx(ctx, tx, h.sid, []events.NewEvent{{Type: domain.EventUserCustomToolRes, Payload: body}}, events.AppendOptions{}); err != nil {
					t.Fatal(err)
				}
				flow, err := h.log.AdvanceThreadTools(ctx, tx, h.sid, "", func(string) bool { return false })
				if err != nil || flow.PlatformRunnable || len(flow.Pending) != 2 {
					t.Fatalf("early beta flow: %+v %v", flow, err)
				}
			}
			body, _ := json.Marshal(map[string]any{"custom_tool_use_id": alpha, "content": []any{map[string]any{"type": "text", "text": "alpha"}}})
			if _, err = h.log.AppendInTx(ctx, tx, h.sid, []events.NewEvent{{Type: domain.EventUserCustomToolRes, Payload: body}}, events.AppendOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err = h.log.AdvanceThreadTools(ctx, tx, h.sid, "", func(string) bool { return false }); err != nil {
				t.Fatal(err)
			}
			if _, err = h.queue.Enqueue(ctx, tx, h.envID, h.sid, queue.ToolExec); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if betaFirst {
				var processed bool
				if err := h.pool.QueryRow(ctx, `SELECT processed_at IS NOT NULL FROM events WHERE session_id=$1 AND payload->>'custom_tool_use_id'=$2`, h.sid.String(), beta.String()).Scan(&processed); err != nil {
					t.Fatal(err)
				}
				if processed {
					t.Fatal("saved beta processed before cloud execution")
				}
			}
			if ok, err := h.exec.step(ctx); err != nil || !ok {
				t.Fatalf("step: %v %v", ok, err)
			}
			if got := len(h.types(t, "agent.tool_result")); got != 1 {
				t.Fatalf("published cloud results=%d", got)
			}
			var status string
			if err = h.pool.QueryRow(ctx, `SELECT status FROM sessions WHERE id=$1`, h.sid.String()).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if betaFirst {
				if status != "running" || h.liveOf(t, queue.ModelTurn) != 1 {
					t.Fatal("cloud completion did not process saved beta and resume exactly once")
				}
				var processed bool
				if err := h.pool.QueryRow(ctx, `SELECT processed_at IS NOT NULL FROM events WHERE session_id=$1 AND payload->>'custom_tool_use_id'=$2`, h.sid.String(), beta.String()).Scan(&processed); err != nil {
					t.Fatal(err)
				}
				if !processed {
					t.Fatal("saved beta not processed after cloud execution")
				}
				return
			}
			if status != "idle" {
				t.Fatalf("after cloud result=%s, want idle waiting only beta", status)
			}
			if got := h.liveOf(t, queue.ModelTurn); got != 0 {
				t.Fatalf("premature model work=%d", got)
			}
			idle := h.types(t, "session.status_idle")
			var payload struct {
				StopReason domain.StopReason `json:"stop_reason"`
			}
			if len(idle) == 0 {
				t.Fatal("missing beta wait")
			}
			if err = json.Unmarshal(idle[len(idle)-1].Body, &payload); err != nil {
				t.Fatal(err)
			}
			calls := h.types(t, "agent.custom_tool_use")
			if len(payload.StopReason.EventIDs) != 1 || payload.StopReason.EventIDs[0] != calls[1].ID {
				t.Fatalf("remaining=%v", payload.StopReason)
			}
		})
	}
}

func TestIdleHarvestAndToolsDoNotOverlap(t *testing.T) {
	for _, harvestFirst := range []bool{false, true} {
		name := "tool_first"
		if harvestFirst {
			name = "harvest_first"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, &fakeSandbox{})
			ctx := context.Background()
			h.enqueueIdleHarvest(t)
			harvest, err := h.queue.Claim(ctx, queue.OutputsHarvest, time.Minute)
			if err != nil || harvest == nil {
				t.Fatalf("claim harvest: %v %v", harvest, err)
			}
			if harvestFirst {
				if _, live, err := h.exec.sessionForRun(ctx, harvest); err != nil || !live {
					t.Fatalf("harvest admission: %v %v", live, err)
				}
			}
			h.suspend(t, writeUse("out.txt", "hello"))
			pgtest.SetSessionStatus(t, h.pool, h.sid, "running")
			tool, err := h.queue.Claim(ctx, queue.ToolExec, time.Minute)
			if err != nil || tool == nil {
				t.Fatalf("claim tool: %v %v", tool, err)
			}
			if _, live, err := h.exec.sessionForRun(ctx, tool); err != nil || live {
				t.Fatalf("tool overlapped claimed harvest: %v %v", live, err)
			}
			if harvestFirst {
				if err := h.queue.Complete(ctx, h.pool, harvest); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, live, err := h.exec.sessionForRun(ctx, harvest); err != nil || live {
					t.Fatalf("obsolete harvest admission: %v %v", live, err)
				}
			}
			tool, err = h.queue.Claim(ctx, queue.ToolExec, time.Minute)
			if err != nil || tool == nil {
				t.Fatalf("reclaim tool: %v %v", tool, err)
			}
			if _, live, err := h.exec.sessionForRun(ctx, tool); err != nil || !live {
				t.Fatalf("tool after harvest: %v %v", live, err)
			}
		})
	}
}
