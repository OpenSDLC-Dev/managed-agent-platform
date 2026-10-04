package brain_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/brain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
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

	// The wire event stays content-free (checked against anthropic-sdk-go
	// v1.70.1 — betasessionevent.go BetaManagedAgentsAgentThinkingEvent).
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
	if want := []string{"thinking", "tool_use"}; !slices.Equal(blockTypes(got), want) {
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
	if want := []string{"thinking", "redacted_thinking", "tool_use"}; !slices.Equal(blockTypes(got), want) {
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
	if want := []string{"thinking", "text"}; !slices.Equal(blockTypes(got), want) {
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

// A block the brain never saw — one a stream carried no chunk for — still sits
// between its neighbours in the response, so the run of kept blocks ends at
// the gap in the block indices: the block after it was produced after content
// the replay would not send ahead of it.
func TestAnUnseenBlockEndsTheLeadingRun(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{
			thinkingChunk(0, "first"), signatureChunk(0, "s0"),
			thinkingChunk(2, "after a gap"), signatureChunk(2, "s2"),
			provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
				ID: "toolu_1", Name: "lookup", Input: json.RawMessage(`{}`)}},
			done("tool_use", 3),
		},
		{textChunk(0, "ok"), done("end_turn", 1)},
	}, nil)
	h.lookupAgent(t, "fixture-model", "s")
	h.wake(t, "go")
	h.runOnce(t)
	if n := h.thinkingRows(t); n != 1 {
		t.Fatalf("kept %d thinking blocks, want the one before the gap", n)
	}
	h.answerLookup(t, "ok")
	h.runOnce(t)
	got := h.assistantBlocks(t, 1)[0]
	if want := []string{"thinking", "tool_use"}; !slices.Equal(blockTypes(got), want) {
		t.Fatalf("assistant blocks = %v, want %v", blockTypes(got), want)
	}
	if got[0]["signature"] != "s0" {
		t.Errorf("replayed block = %v, want the first", got[0])
	}
}

// wakeWith is wake with a user message of the given content blocks.
func (h *harness) wakeWith(t *testing.T, content []map[string]any) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"content": content})
	_, err := h.log.AppendTransition(context.Background(), h.sessionID,
		[]events.NewEvent{{Type: domain.EventUserMessage, Payload: payload}},
		[]events.ThreadTransition{{Status: domain.SessionRunning}},
		events.AppendOptions{
			Then: func(ctx context.Context, tx pgx.Tx) error {
				_, err := h.queue.Enqueue(ctx, tx, h.envID, h.sessionID, queue.ModelTurn)
				return err
			},
		})
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
}

// A block produced after an image or document fetched by URL is not kept: the
// bytes behind a URL can change while the request stays the same, and a block
// replayed over changed bytes is one under another prefix. Inline media is
// hashed with the request and keeps its blocks.
func TestThinkingIsNotKeptAfterURLMedia(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source map[string]any
		want   int
	}{
		{"url", map[string]any{"type": "url", "url": "https://example.com/latest.png"}, 0},
		{"base64", map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, [][]provider.Chunk{{
				thinkingChunk(0, "look"), signatureChunk(0, "s"),
				provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
					ID: "toolu_1", Name: "lookup", Input: json.RawMessage(`{}`)}},
				done("tool_use", 3),
			}}, nil)
			h.lookupAgent(t, "fixture-model", "s")
			h.wakeWith(t, []map[string]any{
				{"type": "text", "text": "what is this?"},
				{"type": "image", "source": tc.source},
			})
			h.runOnce(t)
			if n := h.thinkingRows(t); n != tc.want {
				t.Errorf("kept %d thinking blocks, want %d", n, tc.want)
			}
		})
	}
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
	if want := []string{"thinking", "tool_use"}; !slices.Equal(blockTypes(turns[0]), want) {
		t.Fatalf("assistant blocks = %v, want %v", blockTypes(turns[0]), want)
	}
}

// The route is part of every block's prefix, and the brain hands replay the
// one the turn goes over: a session whose model moves to another endpoint
// under the same model id loses its earlier thinking rather than sending a
// signature to an endpoint that did not issue it.
func TestThinkingDropsWhenTheRouteMoves(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{
			thinkingChunk(0, "first"), signatureChunk(0, "s"),
			provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
				ID: "toolu_1", Name: "lookup", Input: json.RawMessage(`{}`)}},
			done("tool_use", 3),
		},
		{textChunk(0, "ok"), done("end_turn", 1)},
	}, nil)
	h.lookupAgent(t, "fixture-model", "s")
	h.wake(t, "go")
	h.runOnce(t)
	if n := h.thinkingRows(t); n != 1 {
		t.Fatalf("kept %d thinking blocks, want 1", n)
	}

	moved, err := provider.NewRegistry(
		[]provider.Route{{Model: "*", Config: provider.Config{Protocol: "fake", BaseURL: "http://fake-elsewhere"}}},
		map[string]provider.Factory{"fake": func(provider.Config) (provider.Provider, error) { return h.provider, nil }})
	if err != nil {
		t.Fatal(err)
	}
	h.registry, h.brain = moved, brain.New(h.pool, moved, nil, brain.Config{})
	h.answerLookup(t, "ok")
	h.runOnce(t)
	if got := blockTypes(h.assistantBlocks(t, 1)[0]); !slices.Equal(got, []string{"tool_use"}) {
		t.Errorf("assistant blocks after the route moved = %v, want the tool call alone", got)
	}
}

// A request that fails with thinking to replay makes the session forget what
// it kept: an endpoint that refuses a kept block would refuse it on every turn
// after, and the session would never recover.
func TestAFailedRequestForgetsTheSessionsThinking(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{
		{
			thinkingChunk(0, "first"), signatureChunk(0, "s"),
			provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
				ID: "toolu_1", Name: "lookup", Input: json.RawMessage(`{}`)}},
			done("tool_use", 3),
		},
		{},
		{textChunk(0, "ok"), done("end_turn", 1)},
	}, []error{nil, errors.New("400 Bad Request: invalid signature in thinking block")})
	h.lookupAgent(t, "fixture-model", "s")
	h.wake(t, "go")
	h.runOnce(t)
	h.answerLookup(t, "ok")
	h.runOnce(t)
	if n := h.countType(t, "session.error"); n != 1 {
		t.Fatalf("session.error events = %d, want the refused request's", n)
	}
	if n := h.thinkingRows(t); n != 0 {
		t.Fatalf("kept %d thinking blocks after the refusal, want none", n)
	}
	h.wake(t, "try again")
	h.runOnce(t)
	if got := blockTypes(h.assistantBlocks(t, 2)[0]); !slices.Equal(got, []string{"tool_use"}) {
		t.Errorf("assistant blocks after the refusal = %v, want the tool call alone", got)
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
	if got := blockTypes(h.assistantBlocks(t, 1)[0]); !slices.Equal(got, []string{"tool_use"}) {
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
	if got := blockTypes(h.assistantBlocks(t, 1)[0]); !slices.Equal(got, []string{"tool_use"}) {
		t.Errorf("after the system.message the first block still went back: %v", got)
	}
	h.answerLookup(t, "two")
	h.runOnce(t)
	turns := h.assistantBlocks(t, 2)
	if len(turns) != 2 {
		t.Fatalf("third request has %d assistant messages, want 2", len(turns))
	}
	if got := blockTypes(turns[0]); !slices.Equal(got, []string{"tool_use"}) {
		t.Errorf("first turn = %v, want its thinking still dropped", got)
	}
	if got := blockTypes(turns[1]); !slices.Equal(got, []string{"thinking", "tool_use"}) {
		t.Errorf("second turn = %v, want its thinking kept", got)
	}
}
