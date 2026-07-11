package api_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The confirmation round-trip end to end: a real brain runs an always_ask turn
// and suspends (agent.tool_use "ask" + session.status_idle carrying
// requires_action), then a user.tool_confirmation POSTed through the API
// resolves that exact event and resumes the session. The two halves are only
// correct together if the id the brain mints into requires_action is the id the
// API's validation and blocking-set query recognize — which these tests pin.

type scriptedStream struct {
	chunks []provider.Chunk
	i      int
}

func (s *scriptedStream) Next() bool {
	if s.i >= len(s.chunks) {
		return false
	}
	s.i++
	return true
}
func (s *scriptedStream) Chunk() provider.Chunk { return s.chunks[s.i-1] }
func (s *scriptedStream) Err() error            { return nil }
func (s *scriptedStream) Close() error          { return nil }

type scriptedProvider struct{ chunks []provider.Chunk }

func (p *scriptedProvider) Generate(context.Context, provider.Request) (provider.Stream, error) {
	return &scriptedStream{chunks: p.chunks}, nil
}

func newScriptedBrain(t *testing.T, pool *pgxpool.Pool, chunks []provider.Chunk) *brain.Brain {
	t.Helper()
	reg, err := provider.NewRegistry(
		[]provider.Route{{Model: "*", Config: provider.Config{Protocol: "fake", BaseURL: "http://fake"}}},
		map[string]provider.Factory{"fake": func(provider.Config) (provider.Provider, error) {
			return &scriptedProvider{chunks: chunks}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	return brain.New(pool, reg, brain.Config{})
}

// suspendViaBrain creates an always_ask agent + session, wakes it, and runs one
// real brain turn that calls bash — leaving the session suspended on
// requires_action. It returns the session id and the gated tool_use event id.
func suspendViaBrain(t *testing.T, s *tserver) (sessionID, askID string) {
	t.Helper()
	agent := createAgent(t, s, map[string]any{
		"name": "gated", "model": "claude-opus-4-8",
		"tools": []any{map[string]any{
			"type":           "agent_toolset_20260401",
			"default_config": map[string]any{"permission_policy": map[string]any{"type": "always_ask"}},
		}},
	})
	env := createEnvironment(t, s, map[string]any{"name": "e", "config": map[string]any{"type": "self_hosted"}})
	session := createSession(t, s, map[string]any{"agent": agent["id"], "environment_id": env["id"]})
	sessionID = session["id"].(string)

	sendEvents(t, s, sessionID, userMessage("list the files"))
	b := newScriptedBrain(t, s.pool, []provider.Chunk{
		{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
			ID: "toolu_x", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}},
		{Kind: provider.KindDone, StopReason: "tool_use",
			Usage: &domain.ModelUsage{InputTokens: 5, OutputTokens: 2}},
	})
	found, err := b.RunOnce(context.Background())
	if err != nil || !found {
		t.Fatalf("brain RunOnce: found=%v err=%v", found, err)
	}

	if got := s.sessionStatus(sessionID); got != "idle" {
		t.Fatalf("status after ask suspend = %q, want idle", got)
	}
	idle := lastEventOfType(t, s, sessionID, "session.status_idle")
	stop, _ := idle["stop_reason"].(map[string]any)
	ids, _ := stop["event_ids"].([]any)
	if stop == nil || stop["type"] != "requires_action" || len(ids) != 1 {
		t.Fatalf("stop_reason = %v, want requires_action with one event id", idle["stop_reason"])
	}
	// The gated intent carries evaluated_permission "ask".
	use := lastEventOfType(t, s, sessionID, "agent.tool_use")
	if use["evaluated_permission"] != "ask" {
		t.Fatalf("tool_use evaluated_permission = %v, want ask", use["evaluated_permission"])
	}
	return sessionID, ids[0].(string)
}

func TestConfirmationClosedLoopAllow(t *testing.T) {
	s := newTestServer(t)
	sessionID, askID := suspendViaBrain(t, s)

	if n := s.liveWork(sessionID, queue.ToolExec); n != 0 {
		t.Fatalf("tool_exec before confirmation = %d, want 0", n)
	}
	sendEvents(t, s, sessionID, confirm(askID, "allow", nil))

	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status after allow = %q, want running", got)
	}
	if n := s.liveWork(sessionID, queue.ToolExec); n != 1 {
		t.Errorf("tool_exec after allow = %d, want 1 (executor runs the confirmed tool)", n)
	}
}

func TestConfirmationClosedLoopDeny(t *testing.T) {
	s := newTestServer(t)
	sessionID, askID := suspendViaBrain(t, s)

	sendEvents(t, s, sessionID, confirm(askID, "deny", map[string]any{"deny_message": "blocked"}))

	if got := s.sessionStatus(sessionID); got != "running" {
		t.Errorf("status after deny = %q, want running", got)
	}
	// Nothing left to run: the brain resumes directly on the error result.
	if n := s.liveWork(sessionID, queue.ToolExec); n != 0 {
		t.Errorf("tool_exec after deny = %d, want 0", n)
	}
	if n := s.liveWork(sessionID, queue.ModelTurn); n != 1 {
		t.Errorf("model_turn after deny = %d, want 1", n)
	}
	res := lastEventOfType(t, s, sessionID, "agent.tool_result")
	if res["tool_use_id"] != askID || res["is_error"] != true {
		t.Errorf("deny result = %v", res)
	}
}
