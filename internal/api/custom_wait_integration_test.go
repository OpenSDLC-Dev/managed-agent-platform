package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// The client follows the cookbook: it answers the idle/requires_action event,
// never a bare custom_tool_use and never by manually enqueueing model work.
func TestCustomWaitCookbookRoundTrip(t *testing.T) {
	s := newTestServer(t)
	a := createAgent(t, s, map[string]any{"name": "custom-wait", "model": "claude-opus-4-8", "tools": []any{customTool("decide")}})
	e := createEnvironment(t, s, map[string]any{"name": "custom-env", "config": map[string]any{"type": "self_hosted"}})
	session := createSession(t, s, map[string]any{"agent": a["id"], "environment_id": e["id"]})
	sid := session["id"].(string)
	sendEvents(t, s, sid, userMessage("decide"))
	stream := s.stream(t, "/v1/sessions/"+sid+"/events/stream")
	defer stream.close()
	client := sdk.NewClient(option.WithBaseURL(s.url), option.WithAPIKey(testKey))
	for decision := 0; decision < 12; decision++ {
		b := newScriptedBrain(t, s.pool, []provider.Chunk{
			{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{ID: "toolu_decide", Name: "decide", Input: json.RawMessage(`{}`)}},
			{Kind: provider.KindDone, StopReason: "tool_use", Usage: &domain.ModelUsage{InputTokens: 1, OutputTokens: 1}},
		})
		if found, err := b.RunOnce(context.Background()); err != nil || !found {
			t.Fatalf("brain: found=%v err=%v", found, err)
		}
		if got := s.sessionStatus(sid); got != "idle" {
			t.Fatalf("custom wait status=%s, want idle/requires_action", got)
		}
		idle := lastEventOfType(t, s, sid, "session.status_idle")
		stop := idle["stop_reason"].(map[string]any)
		ids := stop["event_ids"].([]any)
		if stop["type"] != "requires_action" || len(ids) != 1 {
			t.Fatalf("custom wait: %v", stop)
		}
		use := lastEventOfType(t, s, sid, "agent.custom_tool_use")
		if ids[0] != use["id"] {
			t.Fatalf("blocking id=%v, call=%v", ids, use)
		}
		for {
			f := stream.next(t)
			if f.name == "session.status_idle" {
				break
			}
		}

		reply := sdk.BetaManagedAgentsEventParamsOfUserCustomToolResult(ids[0].(string))
		reply.OfUserCustomToolResult.Type = sdk.BetaManagedAgentsUserCustomToolResultEventParamsTypeUserCustomToolResult
		reply.OfUserCustomToolResult.Content = []sdk.BetaManagedAgentsUserCustomToolResultEventParamsContentUnion{{OfText: &sdk.BetaManagedAgentsTextBlockParam{Type: sdk.BetaManagedAgentsTextBlockTypeText, Text: "approved"}}}
		if _, err := client.Beta.Sessions.Events.Send(context.Background(), sid, sdk.BetaSessionEventSendParams{Events: []sdk.BetaManagedAgentsEventParamsUnion{reply}}); err != nil {
			t.Fatal(err)
		}

		if got := s.sessionStatus(sid); got != "running" {
			t.Fatalf("after result=%s, want running", got)
		}
	}
	finish := newScriptedBrain(t, s.pool, []provider.Chunk{{Kind: provider.KindDone, StopReason: "end_turn", Usage: &domain.ModelUsage{InputTokens: 1, OutputTokens: 1}}})
	if found, err := finish.RunOnce(context.Background()); err != nil || !found {
		t.Fatalf("resumed brain: found=%v err=%v", found, err)
	}
	if got := lastEventOfType(t, s, sid, "session.status_idle")["stop_reason"].(map[string]any)["type"]; got != "end_turn" {
		t.Fatalf("final reason=%v", got)
	}
	var turns int
	if err := s.pool.QueryRow(context.Background(), "SELECT count(*) FROM events WHERE session_id=$1 AND type='span.model_request_start'", sid).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if turns != 13 {
		t.Fatalf("model calls=%d want 13 for 12 decisions", turns)
	}

}

func TestCustomWaitBatchAndConcurrentCompletion(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrent_%v", concurrent), func(t *testing.T) {
			s := newTestServer(t)
			sid, ids := customWaitPair(t, s)
			if concurrent {
				var wg sync.WaitGroup
				statuses := make(chan int, 2)
				for _, id := range ids {
					wg.Add(1)
					go func(id string) {
						defer wg.Done()
						code, _ := s.do(http.MethodPost, "/v1/sessions/"+sid+"/events", map[string]any{"events": []any{customReply(id)}})
						statuses <- code
					}(id)
				}
				wg.Wait()
				close(statuses)
				for code := range statuses {
					if code != http.StatusOK {
						t.Fatalf("concurrent response=%d", code)
					}
				}
			} else {
				sendEvents(t, s, sid, customReply(ids[1]), customReply(ids[0]))
			}
			if got := s.liveWork(sid, queue.ModelTurn); got != 1 {
				t.Fatalf("complete set queued %d model tasks", got)
			}
			var processed int
			if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE session_id=$1 AND type='user.custom_tool_result' AND processed_at IS NOT NULL`, sid).Scan(&processed); err != nil {
				t.Fatal(err)
			}
			if processed != 2 {
				t.Fatalf("processed=%d", processed)
			}
			code, _ := s.do(http.MethodPost, "/v1/sessions/"+sid+"/events", map[string]any{"events": []any{customReply(ids[1])}})
			if code != http.StatusBadRequest || s.liveWork(sid, queue.ModelTurn) != 1 {
				t.Fatal("duplicate reply changed scheduling")
			}
		})
	}
}

func TestCustomWaitRollsBackFailedProcessing(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	sid, ids := customWaitPair(t, s)
	sendEvents(t, s, sid, customReply(ids[1]))
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_tool_processing() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'injected processing failure'; END $$;
 CREATE TRIGGER reject_tool_processing BEFORE UPDATE OF processed_at ON events
 FOR EACH ROW EXECUTE FUNCTION reject_tool_processing()`); err != nil {
		t.Fatal(err)
	}
	code, _ := s.do(http.MethodPost, "/v1/sessions/"+sid+"/events", map[string]any{"events": []any{customReply(ids[0])}})
	if code != http.StatusInternalServerError {
		t.Fatalf("processing failure response=%d", code)
	}
	var received, processed int
	if err := s.pool.QueryRow(ctx, `SELECT count(*),count(processed_at) FROM events
 WHERE session_id=$1 AND type='user.custom_tool_result'`, sid).Scan(&received, &processed); err != nil {
		t.Fatal(err)
	}
	if received != 1 || processed != 0 || s.sessionStatus(sid) != "idle" || s.liveWork(sid, queue.ModelTurn) != 0 {
		t.Fatalf("failed transaction changed receipt/processing/state: %d/%d", received, processed)
	}
	if got := lastEventOfType(t, s, sid, "session.status_idle")["stop_reason"].(map[string]any)["event_ids"]; !reflect.DeepEqual(got, []any{ids[0], ids[1]}) {
		t.Fatalf("failed processing changed blockers: %v", got)
	}
	if _, err := s.pool.Exec(ctx, `DROP TRIGGER reject_tool_processing ON events`); err != nil {
		t.Fatal(err)
	}
	sendEvents(t, s, sid, customReply(ids[0]))
	if s.sessionStatus(sid) != "running" || s.liveWork(sid, queue.ModelTurn) != 1 {
		t.Fatal("retry after rollback failed to resume exactly once")
	}
}

func TestCustomWaitMessagesAndCancellation(t *testing.T) {
	for _, archive := range []bool{false, true} {
		t.Run(fmt.Sprintf("archive_%v", archive), func(t *testing.T) {
			s := newTestServer(t)
			sid, ids := customWaitPair(t, s)
			sendEvents(t, s, sid, customReply(ids[1]))
			saved := lastEventOfType(t, s, sid, "user.custom_tool_result")["id"]
			sendEvents(t, s, sid, userMessage("do not skip the outstanding tool"))
			if s.sessionStatus(sid) != "idle" || s.liveWork(sid, queue.ModelTurn) != 0 {
				t.Fatal("ordinary message bypassed tool wait")
			}
			if archive {
				code, res := s.do(http.MethodPost, "/v1/sessions/"+sid+"/archive", nil)
				if code != http.StatusOK {
					t.Fatalf("archive %d: %v", code, res)
				}
			} else {
				sendEvents(t, s, sid, map[string]any{"type": "user.interrupt"})
			}
			var count, processed int
			if err := s.pool.QueryRow(context.Background(), `SELECT count(*),count(processed_at) FROM events WHERE session_id=$1 AND id=$2`, sid, saved).Scan(&count, &processed); err != nil {
				t.Fatal(err)
			}
			if count != 1 || processed != 1 {
				t.Fatalf("received reply lost or unprocessed: %d/%d", count, processed)
			}
			if s.liveWork(sid, queue.ModelTurn) != 0 {
				t.Fatal("cancellation resumed model")
			}
			code, _ := s.do(http.MethodPost, "/v1/sessions/"+sid+"/events", map[string]any{"events": []any{customReply(ids[0])}})
			wantStatus := "idle"
			if archive {
				wantStatus = "terminated"
			}
			if code != http.StatusBadRequest || s.sessionStatus(sid) != wantStatus {
				t.Fatal("late reply revived stopped session")
			}
		})
	}
}

func customWaitPair(t *testing.T, s *tserver) (string, []string) {
	t.Helper()
	a := createAgent(t, s, map[string]any{"name": "ordered-custom", "model": "claude-opus-4-8", "tools": []any{customTool("decide")}})
	e := createEnvironment(t, s, map[string]any{"name": "ordered-env", "config": map[string]any{"type": "self_hosted"}})
	sid := createSession(t, s, map[string]any{"agent": a["id"], "environment_id": e["id"]})["id"].(string)
	sendEvents(t, s, sid, userMessage("two decisions"))
	var chunks []provider.Chunk
	for i := 0; i < 2; i++ {
		chunks = append(chunks, provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{ID: fmt.Sprintf("toolu_%d", i), Name: "decide", Input: json.RawMessage(`{}`)}})
	}
	chunks = append(chunks, provider.Chunk{Kind: provider.KindDone, StopReason: "tool_use", Usage: &domain.ModelUsage{InputTokens: 1, OutputTokens: 1}})
	if found, err := newScriptedBrain(t, s.pool, chunks).RunOnce(context.Background()); err != nil || !found {
		t.Fatalf("brain: %v %v", found, err)
	}
	idle := lastEventOfType(t, s, sid, "session.status_idle")
	raw := idle["stop_reason"].(map[string]any)["event_ids"].([]any)
	return sid, []string{raw[0].(string), raw[1].(string)}
}

func customReply(id string) map[string]any {
	return map[string]any{"type": "user.custom_tool_result", "custom_tool_use_id": id, "content": []any{map[string]any{"type": "text", "text": "approved"}}}
}

func TestCustomWaitPartialAndReverseReplies(t *testing.T) {
	for _, first := range []int{0, 1} {
		t.Run(fmt.Sprintf("first_%d", first), func(t *testing.T) {
			s := newTestServer(t)
			sid, ids := customWaitPair(t, s)
			sendEvents(t, s, sid, customReply(ids[first]))
			want := []any{ids[1]}
			if first == 1 {
				want = []any{ids[0], ids[1]}
			}
			idle := lastEventOfType(t, s, sid, "session.status_idle")
			if got := idle["stop_reason"].(map[string]any)["event_ids"]; !reflect.DeepEqual(got, want) {
				t.Fatalf("remaining=%v want %v", got, want)
			}
			received := lastEventOfType(t, s, sid, "user.custom_tool_result")
			if (received["processed_at"] != nil) != (first == 0) {
				t.Fatalf("first reply processing=%v, order=%d", received, first)
			}
			if s.sessionStatus(sid) != "idle" {
				t.Fatal("partial set woke the model")
			}
			sendEvents(t, s, sid, customReply(ids[1-first]))
			if s.sessionStatus(sid) != "running" {
				t.Fatal("complete set did not resume")
			}
			if lastEventOfType(t, s, sid, "user.custom_tool_result")["processed_at"] == nil {
				t.Fatal("complete results remain unprocessed")
			}
			var ordered bool
			if err := s.pool.QueryRow(context.Background(), `SELECT a.processed_at <= b.processed_at FROM events a, events b WHERE a.session_id=$1 AND b.session_id=$1 AND a.payload->>'custom_tool_use_id'=$2 AND b.payload->>'custom_tool_use_id'=$3`, sid, ids[0], ids[1]).Scan(&ordered); err != nil {
				t.Fatal(err)
			}
			if !ordered {
				t.Fatal("results processed in arrival order instead of tool generation order")
			}
		})
	}
}

func TestSelfHostedToolWaitUntilResult(t *testing.T) {
	for _, policy := range []string{"always_allow", "always_ask"} {
		t.Run(policy, func(t *testing.T) {
			s := newTestServer(t)
			a := createAgent(t, s, map[string]any{"name": "worker-wait", "model": "claude-opus-4-8", "tools": []any{map[string]any{"type": "agent_toolset_20260401", "default_config": map[string]any{"permission_policy": map[string]any{"type": policy}}}}})
			e := createEnvironment(t, s, map[string]any{"name": "worker-env", "config": map[string]any{"type": "self_hosted"}})
			sid := createSession(t, s, map[string]any{"agent": a["id"], "environment_id": e["id"]})["id"].(string)
			sendEvents(t, s, sid, userMessage("run bash"))
			b := newScriptedBrain(t, s.pool, []provider.Chunk{{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{ID: "toolu_bash", Name: "bash", Input: json.RawMessage(`{"command":"true"}`)}}, {Kind: provider.KindDone, StopReason: "tool_use", Usage: &domain.ModelUsage{InputTokens: 1, OutputTokens: 1}}})
			if found, err := b.RunOnce(context.Background()); err != nil || !found {
				t.Fatalf("brain: %v %v", found, err)
			}
			if got := s.sessionStatus(sid); got != "idle" {
				t.Fatalf("worker wait=%s want idle", got)
			}
			use := lastEventOfType(t, s, sid, "agent.tool_use")["id"].(string)
			idle := lastEventOfType(t, s, sid, "session.status_idle")
			if policy == "always_ask" {
				sendEvents(t, s, sid, confirm(use, "allow", nil))
				if s.sessionStatus(sid) != "idle" {
					t.Fatal("approval resolved the worker result wait")
				}
				if got := lastEventOfType(t, s, sid, "session.status_idle")["id"]; got != idle["id"] {
					t.Fatal("approval emitted a redundant idle")
				}
			}
			sendEventsAs(t, s, workerAuth(t, s, sid), sid, map[string]any{"type": "user.tool_result", "tool_use_id": use, "content": []any{map[string]any{"type": "text", "text": "done"}}})
			if s.sessionStatus(sid) != "running" {
				t.Fatal("worker result did not resume")
			}
		})
	}
}

func TestCustomBeforeApprovalDefersExecution(t *testing.T) {
	for _, verdict := range []string{"allow", "deny"} {
		t.Run(verdict, func(t *testing.T) {
			s := newTestServer(t)
			a := createAgent(t, s, map[string]any{"name": "mixed", "model": "claude-opus-4-8", "tools": []any{customTool("decide"), map[string]any{"type": "agent_toolset_20260401", "default_config": map[string]any{"permission_policy": map[string]any{"type": "always_ask"}}}}})
			kind := "cloud"
			if verdict == "deny" {
				kind = "self_hosted"
			}
			e := createEnvironment(t, s, map[string]any{"name": "mixed", "config": map[string]any{"type": kind}})
			sid := createSession(t, s, map[string]any{"agent": a["id"], "environment_id": e["id"]})["id"].(string)
			sendEvents(t, s, sid, userMessage("decide then bash"))
			b := newScriptedBrain(t, s.pool, []provider.Chunk{
				{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{ID: "toolu_c", Name: "decide", Input: json.RawMessage(`{}`)}},
				{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{ID: "toolu_b", Name: "bash", Input: json.RawMessage(`{"command":"true"}`)}},
				{Kind: provider.KindDone, StopReason: "tool_use", Usage: &domain.ModelUsage{InputTokens: 1, OutputTokens: 1}},
			})
			if found, err := b.RunOnce(context.Background()); err != nil || !found {
				t.Fatalf("brain: %v %v", found, err)
			}
			customID := lastEventOfType(t, s, sid, "agent.custom_tool_use")["id"].(string)
			toolID := lastEventOfType(t, s, sid, "agent.tool_use")["id"].(string)
			sendEvents(t, s, sid, confirm(toolID, verdict, nil))
			if got := s.liveWork(sid, queue.ToolExec); got != 0 {
				t.Fatalf("premature tool execution work=%d", got)
			}
			if lastEventOfType(t, s, sid, "user.tool_confirmation")["processed_at"] != nil {
				t.Fatal("approval processed before custom")
			}
			if verdict == "deny" {
				code, _ := readJSON(t, s.doRaw(http.MethodPost, "/v1/sessions/"+sid+"/events", map[string]any{"events": []any{map[string]any{"type": "user.tool_result", "tool_use_id": toolID, "content": []any{map[string]any{"type": "text", "text": "must not override deny"}}}}}, workerAuth(t, s, sid)))
				if code != http.StatusBadRequest {
					t.Fatalf("result after queued denial=%d", code)
				}
				var denied int
				if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE session_id=$1 AND type='agent.tool_result'`, sid).Scan(&denied); err != nil {
					t.Fatal(err)
				}
				if denied != 0 {
					t.Fatal("denial processed before preceding custom")
				}
			}
			sendEvents(t, s, sid, customReply(customID))
			wantWork := 1
			if verdict == "deny" {
				wantWork = 0
			}
			if got := s.liveWork(sid, queue.ToolExec); got != wantWork {
				t.Fatalf("ready tool execution work=%d", got)
			}
			if lastEventOfType(t, s, sid, "user.tool_confirmation")["processed_at"] == nil {
				t.Fatal("queued approval was not processed")
			}
			if s.sessionStatus(sid) != "running" {
				t.Fatal("cloud execution did not run")
			}
			if verdict == "deny" && s.liveWork(sid, queue.ModelTurn) != 1 {
				t.Fatal("ordered denial did not settle the turn")
			}
		})
	}
}

func TestCustomWaitThreadsResumeIndependently(t *testing.T) {
	s := newTestServer(t)
	sid, ids := customWaitPair(t, s)
	child := insertChild(t, s, sid, "running")
	use := appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentCustomToolUse, `{"name":"decide","input":{}}`)
	// A running sibling keeps the session running during the primary's partial reply.
	if _, err := s.pool.Exec(context.Background(), `UPDATE sessions SET status='running' WHERE id=$1`, sid); err != nil {
		t.Fatal(err)
	}
	sendEvents(t, s, sid, customReply(ids[0]))
	if s.sessionStatus(sid) != "running" || len(s.liveTurns(t, sid)) != 0 {
		t.Fatal("primary partial reply changed sibling scheduling")
	}
	log := events.NewLog(s.pool)
	if _, err := log.AppendTransition(context.Background(), domain.ID(sid), nil, []events.ThreadTransition{{ThreadID: domain.ID(child), Status: domain.SessionIdle, Stop: &domain.StopReason{Type: domain.StopRequiresAction, EventIDs: []domain.ID{domain.ID(use)}}}}, events.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	idle := lastEventOfType(t, s, sid, "session.status_idle")
	remaining := idle["stop_reason"].(map[string]any)["event_ids"].([]any)
	if len(remaining) != 2 {
		t.Fatalf("folded blockers=%v", remaining)
	}
	seen := map[any]bool{}
	for _, id := range remaining {
		seen[id] = true
	}
	if !seen[ids[1]] || !seen[use] {
		t.Fatalf("folded blockers=%v", remaining)
	}
	replied := sendEvents(t, s, sid, customReply(use))
	if s.threadOf(t, replied[0]["id"].(string)) != child {
		t.Fatal("child result routed to primary")
	}
	if got := s.liveTurns(t, sid); !reflect.DeepEqual(got, []string{child}) {
		t.Fatalf("resumed threads=%v", got)
	}
	if s.threadStatus(t, domain.PrimaryThreadID(domain.ID(sid)).String()) != "idle" {
		t.Fatal("sibling result woke primary")
	}
	sendEvents(t, s, sid, customReply(ids[1]))
	if got := s.liveTurns(t, sid); len(got) != 2 {
		t.Fatalf("complete threads=%v", got)
	}
}

func TestApprovalBeforeCustomAdvancesItsOwnTool(t *testing.T) {
	for _, verdict := range []string{"allow", "deny"} {
		t.Run(verdict, func(t *testing.T) {
			s := newTestServer(t)
			sid := eventsFixture(t, s)
			ask := appendAskToolUse(t, s, sid, "bash")
			custom := appendOn(t, s, sid, "", false, domain.EventAgentCustomToolUse, `{"name":"decide","input":{}}`)
			sendEvents(t, s, sid, confirm(ask, verdict, nil))
			if lastEventOfType(t, s, sid, "user.tool_confirmation")["processed_at"] == nil {
				t.Fatal("first approval not processed")
			}
			if verdict == "allow" {
				if s.sessionStatus(sid) != "running" || s.liveWork(sid, queue.ToolExec) != 1 {
					t.Fatal("first cloud tool not scheduled")
				}
			} else {
				idle := lastEventOfType(t, s, sid, "session.status_idle")
				if got := idle["stop_reason"].(map[string]any)["event_ids"]; !reflect.DeepEqual(got, []any{custom}) {
					t.Fatalf("denial left blockers=%v", got)
				}
				if s.liveWork(sid, queue.ModelTurn) != 0 {
					t.Fatal("denial bypassed the custom wait")
				}
			}
		})
	}
}
