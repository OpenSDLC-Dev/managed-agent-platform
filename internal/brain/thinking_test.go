package brain_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// Thinking replay (#67, docs/plan/60_thinking-replay.md). A model's thinking
// goes back on the requests after the one that produced it, signature and
// all, and only under the prefix it was produced under.

func thinkingChunk(idx int64, text string) provider.Chunk {
	return provider.Chunk{Kind: provider.KindThinkingDelta, Index: idx, Text: text}
}

func signatureChunk(idx int64, sig string) provider.Chunk {
	return provider.Chunk{Kind: provider.KindThinkingSignature, Index: idx, Signature: sig}
}

func redactedChunk(idx int64, data string) provider.Chunk {
	return provider.Chunk{Kind: provider.KindRedactedThinking, Index: idx, Data: data}
}

// lookupAgent gives the session one client-executed custom tool, so a test can
// answer a call itself, under the agent model id model.
func (h *harness) lookupAgent(t *testing.T, model, system string) {
	t.Helper()
	agent, err := json.Marshal(map[string]any{
		"type": "agent", "id": "agent_x", "version": 1, "name": "n",
		"model": map[string]string{"id": model}, "system": system, "description": "",
		"tools": []map[string]any{{"type": "custom", "name": "lookup",
			"description": "look <things> up & report", "input_schema": map[string]string{"type": "object"}}},
		"mcp_servers": []any{}, "skills": []any{}, "multiagent": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = $2 WHERE id = $1`, h.sessionID.String(), agent); err != nil {
		t.Fatal(err)
	}
}

// answerLookup answers the session's newest custom tool call, whatever its name.
func (h *harness) answerLookup(t *testing.T, text string) {
	t.Helper()
	evs, err := h.log.List(context.Background(), h.sessionID, events.ListQuery{Types: []string{"agent.custom_tool_use"}})
	if err != nil || len(evs) == 0 {
		t.Fatalf("no custom tool call to answer: %v", err)
	}
	h.postToolResult(t, domain.EventUserCustomToolRes, map[string]any{
		"custom_tool_use_id": evs[len(evs)-1].ID.String(),
		"content":            []map[string]string{{"type": "text", "text": text}},
	})
}

// blocksOf decodes a message's content blocks.
func blocksOf(t *testing.T, m provider.Message) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal(m.Content, &out); err != nil {
		t.Fatalf("content %s: %v", m.Content, err)
	}
	return out
}

// assistantBlocks returns the content of call's assistant messages in order.
func (h *harness) assistantBlocks(t *testing.T, call int) [][]map[string]any {
	t.Helper()
	var out [][]map[string]any
	for _, m := range h.provider.calls[call].Messages {
		if m.Role == "assistant" {
			out = append(out, blocksOf(t, m))
		}
	}
	return out
}

func blockTypes(blocks []map[string]any) []string {
	out := make([]string, len(blocks))
	for i, b := range blocks {
		out[i], _ = b["type"].(string)
	}
	return out
}

func (h *harness) thinkingRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM thinking_blocks WHERE session_id = $1`, h.sessionID.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The core round trip: a thinking block and its signature come back on the
// next request, ahead of the tool call they led to, byte-for-byte what the
// model sent. The fixtures carry <, > and & on every lane the digest reads —
// the user's message, the tool definition, the tool input, the thinking text —
// because json.Marshal escapes those inside a message, and a digest taken over
// one form and checked over the other would refuse every block.
func TestThinkingReplaysWithItsSignature(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{
			thinkingChunk(0, "the user wants <go> & co; "), thinkingChunk(0, "call lookup"),
			signatureChunk(0, "sig-part-1"), signatureChunk(0, "+part-2"),
			provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
				ID: "toolu_1", Name: "lookup", Input: json.RawMessage(`{"q":"<go> & co"}`)}},
			done("tool_use", 3),
		},
		{textChunk(0, "It is a language."), done("end_turn", 5)},
	}, nil)
	h.lookupAgent(t, "fixture-model", "answer <tersely> & well")
	h.wake(t, "what is <go> & co?")
	h.runOnce(t)

	// The wire event stays content-free (anthropic-sdk-go v1.70.1 —
	// betasessionevent.go BetaManagedAgentsAgentThinkingEvent).
	evs, err := h.log.List(context.Background(), h.sessionID, events.ListQuery{Types: []string{"agent.thinking"}})
	if err != nil || len(evs) != 1 {
		t.Fatalf("agent.thinking events = %d (%v), want 1", len(evs), err)
	}
	var wire map[string]any
	if err := json.Unmarshal(evs[0].Body, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["thinking"]; ok {
		t.Errorf("agent.thinking payload carries content: %s", evs[0].Body)
	}
	if _, ok := wire["signature"]; ok {
		t.Errorf("agent.thinking payload carries a signature: %s", evs[0].Body)
	}

	h.answerLookup(t, "a <programming> language & more")
	h.runOnce(t)

	turns := h.assistantBlocks(t, 1)
	if len(turns) != 1 {
		t.Fatalf("resumed request has %d assistant messages, want 1", len(turns))
	}
	got := turns[0]
	if want := []string{"thinking", "tool_use"}; !equalStrings(blockTypes(got), want) {
		t.Fatalf("assistant blocks = %v, want %v", blockTypes(got), want)
	}
	if got[0]["thinking"] != "the user wants <go> & co; call lookup" || got[0]["signature"] != "sig-part-1+part-2" {
		t.Errorf("replayed thinking = %v", got[0])
	}
}

// A Claude 5 model's default: the thinking text omitted, the signature
// carrying it — and a redacted block beside it, which has no signature but a
// payload. Both are kept, in order.
func TestOmittedAndRedactedThinkingReplay(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{
			thinkingChunk(0, ""), signatureChunk(0, "sig-omitted"),
			redactedChunk(1, "ENCRYPTED=="),
			provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
				ID: "toolu_1", Name: "lookup", Input: json.RawMessage(`{}`)}},
			done("tool_use", 3),
		},
		{textChunk(0, "done"), done("end_turn", 5)},
	}, nil)
	h.lookupAgent(t, "fixture-model", "s")
	h.wake(t, "go")
	h.runOnce(t)
	if n := h.countType(t, "agent.thinking"); n != 2 {
		t.Fatalf("agent.thinking events = %d, want one per block (2)", n)
	}
	h.answerLookup(t, "ok")
	h.runOnce(t)

	got := h.assistantBlocks(t, 1)[0]
	if want := []string{"thinking", "redacted_thinking", "tool_use"}; !equalStrings(blockTypes(got), want) {
		t.Fatalf("assistant blocks = %v, want %v", blockTypes(got), want)
	}
	if got[0]["thinking"] != "" || got[0]["signature"] != "sig-omitted" {
		t.Errorf("omitted thinking = %v", got[0])
	}
	if got[1]["data"] != "ENCRYPTED==" {
		t.Errorf("redacted thinking = %v", got[1])
	}
}

// An end_turn reply keeps its thinking too, and the next user message's
// request carries it ahead of the text.
func TestThinkingReplaysAfterAnEndTurn(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{thinkingChunk(0, "greet"), signatureChunk(0, "s1"), textChunk(1, "Hello."), done("end_turn", 3)},
		{textChunk(0, "Again."), done("end_turn", 3)},
	}, nil)
	h.wake(t, "hi")
	h.runOnce(t)
	h.wake(t, "and again")
	h.runOnce(t)

	got := h.assistantBlocks(t, 1)[0]
	if want := []string{"thinking", "text"}; !equalStrings(blockTypes(got), want) {
		t.Fatalf("assistant blocks = %v, want %v", blockTypes(got), want)
	}
}

// Only the run of signed blocks a response opens with is kept: one after the
// response's text would replay ahead of that text, under a prefix it was not
// produced under, and one after an unsigned block would replay without the
// block before it.
func TestOnlyALeadingSignedRunIsKept(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []provider.Chunk
	}{
		{"after text", []provider.Chunk{
			textChunk(0, "Let me check."),
			thinkingChunk(1, "x"), signatureChunk(1, "s"),
		}},
		{"after an unsigned block", []provider.Chunk{
			thinkingChunk(0, "unsigned"),
			thinkingChunk(1, "x"), signatureChunk(1, "s"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := append(tc.chunks,
				provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
					ID: "toolu_1", Name: "lookup", Input: json.RawMessage(`{}`)}},
				done("tool_use", 3))
			h := newHarness(t, [][]provider.Chunk{first, {textChunk(0, "ok"), done("end_turn", 1)}}, nil)
			h.lookupAgent(t, "fixture-model", "s")
			h.wake(t, "go")
			h.runOnce(t)
			if n := h.thinkingRows(t); n != 0 {
				t.Errorf("kept %d thinking blocks, want 0", n)
			}
			h.answerLookup(t, "ok")
			h.runOnce(t)
			for _, typ := range blockTypes(h.assistantBlocks(t, 1)[0]) {
				if typ == "thinking" {
					t.Errorf("a thinking block was replayed: %v", blockTypes(h.assistantBlocks(t, 1)[0]))
				}
			}
		})
	}
}

// A reply of thinking alone, and a turn that fails after its thinking
// streamed, keep nothing: the first is no assistant turn the Messages API
// accepts back, and the second never committed.
func TestThinkingIsKeptOnlyWithACommittedAnswer(t *testing.T) {
	t.Run("thinking alone", func(t *testing.T) {
		h := newHarness(t, [][]provider.Chunk{
			{thinkingChunk(0, "hmm"), signatureChunk(0, "s"), done("end_turn", 1)},
		}, nil)
		h.wake(t, "hi")
		h.runOnce(t)
		if n := h.thinkingRows(t); n != 0 {
			t.Errorf("kept %d thinking blocks, want 0", n)
		}
	})
	t.Run("failed turn", func(t *testing.T) {
		h := newHarness(t, [][]provider.Chunk{
			{thinkingChunk(0, "hmm"), signatureChunk(0, "s"), textChunk(1, "partial")},
		}, []error{errors.New("connection reset")})
		h.wake(t, "hi")
		h.runOnce(t)
		if n := h.countType(t, "agent.thinking"); n != 1 {
			t.Fatalf("agent.thinking events = %d, want the streamed one", n)
		}
		if n := h.thinkingRows(t); n != 0 {
			t.Errorf("kept %d thinking blocks from a failed turn, want 0", n)
		}
	})
}

// The delegated settlement keeps thinking too: a call the settlement answers
// itself — here a name the model was not offered (#567) — commits through
// commitDelegatedTurn, and the request it chains to carries the block.
func TestThinkingReplaysAfterADelegatedSettlement(t *testing.T) {
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
		t.Fatalf("kept blocks = %d, want the delegated turn's one", n)
	}
	h.runOnce(t)

	turns := h.assistantBlocks(t, 1)
	if len(turns) != 1 {
		t.Fatalf("chained request has %d assistant messages, want 1", len(turns))
	}
	if want := []string{"thinking", "tool_use"}; !equalStrings(blockTypes(turns[0]), want) {
		t.Fatalf("assistant blocks = %v, want %v", blockTypes(turns[0]), want)
	}
}

// A block goes back only to the model that produced it: an agent switched to
// another model loses its earlier thinking rather than sending one vendor's
// signature to another.
func TestThinkingDropsWhenTheModelChanges(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{thinkingChunk(0, "x"), signatureChunk(0, "s"),
			provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
				ID: "toolu_1", Name: "lookup", Input: json.RawMessage(`{}`)}},
			done("tool_use", 3)},
		{textChunk(0, "ok"), done("end_turn", 1)},
	}, nil)
	h.lookupAgent(t, "model-a", "s")
	h.wake(t, "go")
	h.runOnce(t)
	h.lookupAgent(t, "model-b", "s")
	h.answerLookup(t, "ok")
	h.runOnce(t)
	if got := blockTypes(h.assistantBlocks(t, 1)[0]); !equalStrings(got, []string{"tool_use"}) {
		t.Errorf("assistant blocks after a model change = %v, want the tool call alone", got)
	}
}

// A system.message folds into the system prompt, which every block's prefix
// begins with: the blocks produced before it drop, and the ones produced after
// it — under the new prompt, without the dropped ones — go back.
func TestThinkingDropsExactlyTheBlocksBeforeAPrefixChange(t *testing.T) {
	call := func(id string) provider.Chunk {
		return provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
			ID: id, Name: "lookup", Input: json.RawMessage(`{}`)}}
	}
	h := newHarness(t, [][]provider.Chunk{
		{thinkingChunk(0, "first"), signatureChunk(0, "s1"), call("toolu_1"), done("tool_use", 3)},
		{thinkingChunk(0, "second"), signatureChunk(0, "s2"), call("toolu_2"), done("tool_use", 3)},
		{textChunk(0, "done"), done("end_turn", 1)},
	}, nil)
	h.lookupAgent(t, "fixture-model", "s")
	h.wake(t, "go")
	h.runOnce(t)
	if _, err := h.log.Append(context.Background(), h.sessionID, []events.NewEvent{{
		Type:    domain.EventSystemMessage,
		Payload: json.RawMessage(`{"content":[{"type":"text","text":"a new instruction"}]}`),
	}}); err != nil {
		t.Fatal(err)
	}
	h.answerLookup(t, "one")
	h.runOnce(t)
	if got := blockTypes(h.assistantBlocks(t, 1)[0]); !equalStrings(got, []string{"tool_use"}) {
		t.Errorf("after the system.message the first block still went back: %v", got)
	}
	h.answerLookup(t, "two")
	h.runOnce(t)
	turns := h.assistantBlocks(t, 2)
	if len(turns) != 2 {
		t.Fatalf("third request has %d assistant messages, want 2", len(turns))
	}
	if got := blockTypes(turns[0]); !equalStrings(got, []string{"tool_use"}) {
		t.Errorf("first turn = %v, want its thinking still dropped", got)
	}
	if got := blockTypes(turns[1]); !equalStrings(got, []string{"thinking", "tool_use"}) {
		t.Errorf("second turn = %v, want its thinking kept", got)
	}
}
