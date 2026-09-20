package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	sdk "github.com/anthropics/anthropic-sdk-go"
)

func TestWorkerExecutesWhileIdleBeforeCustomReply(t *testing.T) {
	for _, workerFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("worker_first_%v", workerFirst), func(t *testing.T) {
			sb := &fakeSandbox{}
			h := newHarness(t, sb)
			ctx := context.Background()
			if workerFirst {
				h.suspend(t, writeUse("out.txt", "hello"))
			}
			custom, err := h.log.AppendWith(ctx, h.sid, []events.NewEvent{{Type: domain.EventAgentCustomToolUse, Payload: json.RawMessage(`{"name":"decision","input":{}}`)}}, events.AppendOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !workerFirst {
				h.suspend(t, writeUse("out.txt", "hello"))
			}
			pgtest.SetSessionStatus(t, h.pool, h.sid, "idle")
			h.enqueueWork(t)
			w, done := h.newWorker(Config{})
			cancel, errc := runWorker(w)
			defer waitExit(t, cancel, errc)
			waitDone(t, done)
			results := h.types(t, "user.tool_result")
			if len(results) != 1 {
				t.Fatalf("worker results=%d, want 1 while idle", len(results))
			}
			if (results[0].ProcessedAt != nil) != workerFirst {
				t.Fatal("worker processing did not respect generated call order")
			}
			if sb.files["/workspace/out.txt"] != "hello" {
				t.Fatal("authorized worker tool did not execute")
			}
			if h.liveModelTurns(t) != 0 {
				t.Fatal("model resumed before custom reply")
			}
			_, err = h.client.Beta.Sessions.Events.Send(ctx, h.sid.String(), sdk.BetaSessionEventSendParams{Events: []sdk.BetaManagedAgentsEventParamsUnion{{OfUserCustomToolResult: &sdk.BetaManagedAgentsUserCustomToolResultEventParams{Type: sdk.BetaManagedAgentsUserCustomToolResultEventParamsTypeUserCustomToolResult, CustomToolUseID: custom[0].ID.String(), Content: []sdk.BetaManagedAgentsUserCustomToolResultEventParamsContentUnion{{OfText: &sdk.BetaManagedAgentsTextBlockParam{Type: sdk.BetaManagedAgentsTextBlockTypeText, Text: "approved"}}}}}}})
			if err != nil {
				t.Fatal(err)
			}
			if h.liveModelTurns(t) != 1 {
				t.Fatal("complete results did not resume exactly once")
			}
			if h.types(t, "user.tool_result")[0].ProcessedAt == nil {
				t.Fatal("stored worker result was not processed")
			}
		})
	}
}
