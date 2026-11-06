package brain_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// removeTools stores what a session update with agent.tools: [] stores.
func (h *harness) removeTools(t *testing.T) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = jsonb_set(resolved_agent, '{tools}', '[]'::jsonb) WHERE id = $1`,
		h.sessionID.String()); err != nil {
		t.Fatal(err)
	}
}

// thinkingTexts is the thinking a request replays, in order.
func thinkingTexts(t *testing.T, h *harness, call int) []string {
	t.Helper()
	var out []string
	for _, blocks := range h.assistantBlocks(t, call) {
		for _, b := range blocks {
			if b["type"] == "thinking" {
				s, _ := b["thinking"].(string)
				out = append(out, s)
			}
		}
	}
	return out
}

// A session whose agent loses every tool after a call still sends the
// conversation the model had: the next request replays the call and its
// answer beside the one placeholder definition, rather than tool blocks with no
// tools (#1021). The requests before the change offer the agent's tool.
func TestATurnAfterTheToolsAreRemovedOffersOnlyThePlaceholder(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{toolUseChunk("toolu_a", "lookup"), done("tool_use", 3)},
		{textChunk(0, "found it"), done("end_turn", 2)},
		{textChunk(0, "you're welcome"), done("end_turn", 2)},
	}, nil)
	h.customTool(t, "lookup")
	h.wake(t, "look it up")
	h.runOnce(t)
	h.answerLookup(t, "42")
	h.runOnce(t)
	h.removeTools(t)
	h.wake(t, "thanks")
	h.runOnce(t)

	if n := len(h.provider.calls); n != 3 {
		t.Fatalf("model requests = %d, want 3", n)
	}
	for i, req := range h.provider.calls[:2] {
		if got := requestTools(t, req); !slices.Equal(got, []string{"lookup"}) {
			t.Errorf("request %d: tools = %v, want lookup", i, got)
		}
	}
	last := h.provider.calls[2]
	if got := requestTools(t, last); !slices.Equal(got, []string{"no_tools_available"}) {
		t.Fatalf("request after the removal: tools = %v, want the placeholder alone", got)
	}
	var kinds []string
	for _, m := range last.Messages {
		for _, b := range blocksOf(t, m) {
			kinds = append(kinds, m.Role+":"+b["type"].(string))
		}
	}
	want := []string{"user:text", "assistant:tool_use", "user:tool_result", "assistant:text", "user:text"}
	if !slices.Equal(kinds, want) {
		t.Errorf("replayed blocks = %v, want %v", kinds, want)
	}
}

// The placeholder is part of the prefix kept thinking is checked against: a
// block kept before the tools went is dropped on the first placeholder turn, as
// any change of tools drops it, and a block produced under the placeholder
// replays on the next one. The order in buildRequest (the placeholder before
// admitThinking) is what the second half pins.
func TestThinkingAcrossThePlaceholderState(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{
			thinkingChunk(0, "before"), signatureChunk(0, "s-before"),
			provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
				ID: "toolu_1", Name: "lookup", Input: json.RawMessage(`{}`)}},
			done("tool_use", 3),
		},
		{textChunk(0, "found"), done("end_turn", 2)},
		{thinkingChunk(0, "under-placeholder"), signatureChunk(0, "s-ph"), textChunk(1, "welcome"), done("end_turn", 2)},
		{textChunk(0, "sure"), done("end_turn", 2)},
	}, nil)
	h.lookupAgent(t, "fixture-model", "s")
	h.wake(t, "go")
	h.runOnce(t)
	h.answerLookup(t, "42")
	h.runOnce(t)
	h.removeTools(t)
	h.wake(t, "thanks")
	h.runOnce(t)
	h.wake(t, "more")
	h.runOnce(t)

	if n := len(h.provider.calls); n != 4 {
		t.Fatalf("model requests = %d, want 4", n)
	}
	if got := thinkingTexts(t, h, 1); !slices.Equal(got, []string{"before"}) {
		t.Errorf("request 1 thinking = %v, want [before] (tools unchanged)", got)
	}
	if got := thinkingTexts(t, h, 2); len(got) != 0 {
		t.Errorf("first placeholder request thinking = %v, want none", got)
	}
	if got := thinkingTexts(t, h, 3); !slices.Equal(got, []string{"under-placeholder"}) {
		t.Errorf("second placeholder request thinking = %v, want [under-placeholder]", got)
	}
}

// A model that calls the placeholder anyway gets #567's unknown-tool answer and
// the turn chains on, so the session reaches idle with no session.error.
func TestACallToThePlaceholderIsAnsweredAsAnUnknownTool(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{toolUseChunk("toolu_a", "lookup"), done("tool_use", 3)},
		{textChunk(0, "found"), done("end_turn", 2)},
		{toolUseChunk("toolu_b", "no_tools_available"), done("tool_use", 3)},
		{textChunk(0, "no tools, so in text"), done("end_turn", 2)},
	}, nil)
	h.customTool(t, "lookup")
	h.wake(t, "look it up")
	h.runOnce(t)
	h.answerLookup(t, "42")
	h.runOnce(t)
	h.removeTools(t)
	h.wake(t, "thanks")
	h.runOnce(t)

	got := h.answers(t)
	if len(got) != 1 || !got[0].isErr || !strings.Contains(got[0].text, `unknown tool "no_tools_available"`) {
		t.Fatalf("answers = %+v, want one unknown-tool error", got)
	}
	h.runOnce(t)
	if s := h.status(t); s != "idle" {
		t.Errorf("status after the chained turn = %s, want idle", s)
	}
	if n := h.countType(t, string(domain.EventSessionError)); n != 0 {
		t.Errorf("session.error events = %d, want 0", n)
	}
}

// A model that keeps calling the placeholder is ended by the settlement's chain
// cap, not left to spin.
func TestAPlaceholderLoopEndsAtTheChainCap(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{toolUseChunk("toolu_a", "lookup"), done("tool_use", 3)},
		{textChunk(0, "found"), done("end_turn", 2)},
		{toolUseChunk("toolu_b", "no_tools_available"), done("tool_use", 3)},
	}, nil)
	h.customTool(t, "lookup")
	h.wake(t, "look it up")
	h.runOnce(t)
	h.answerLookup(t, "42")
	h.runOnce(t)
	h.removeTools(t)
	h.wake(t, "thanks")
	h.setChainCount(t, "", 24)
	h.runOnce(t)

	if s := h.status(t); s != "idle" {
		t.Errorf("status = %s, want idle once the cap ends the chain", s)
	}
	if n := h.liveTurns(t, ""); n != 0 {
		t.Errorf("live turns = %d, want 0", n)
	}
	if n := h.countType(t, string(domain.EventSessionError)); n != 1 {
		t.Errorf("session.error events = %d, want the cap's one", n)
	}
	uses, err := h.log.List(context.Background(), h.sessionID, events.ListQuery{Types: []string{"agent.tool_use"}})
	if err != nil || len(uses) != 1 {
		t.Errorf("placeholder calls = %d (%v), want 1", len(uses), err)
	}
}

// A tool-less agent's model that calls a tool anyway gets #567's answer, and
// the chained request is the first with a tool block in its history, so it
// offers the placeholder: the thinking kept from the call's turn, bound to a
// prefix with no tools, is dropped there. TestThinkingReplaysAfterADelegatedSettlement
// pins the other side, an agent whose tools stay the same.
func TestAToolLessAgentsStrayCallDropsItsThinking(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{
			thinkingChunk(0, "delegate it"), signatureChunk(0, "sig-d"),
			toolCall("t1", "create_agent", `{"agent_name":"researcher","message":"go"}`),
			done("tool_use", 1),
		},
		{textChunk(0, "done"), done("end_turn", 1)},
	}, nil)
	h.wake(t, "hello")
	h.runOnce(t)
	if n := h.thinkingRows(t); n != 1 {
		t.Fatalf("kept blocks = %d, want the stray call's turn's one", n)
	}
	h.runOnce(t)

	if got := requestTools(t, h.provider.calls[0]); len(got) != 0 {
		t.Errorf("first request offered %v, want nothing", got)
	}
	if got := requestTools(t, h.provider.calls[1]); !slices.Equal(got, []string{"no_tools_available"}) {
		t.Fatalf("chained request offered %v, want the placeholder alone", got)
	}
	turns := h.assistantBlocks(t, 1)
	if len(turns) != 1 || !slices.Equal(blockTypes(turns[0]), []string{"tool_use"}) {
		t.Errorf("chained request's assistant turns = %v, want the call alone, its thinking dropped", turns)
	}
}
