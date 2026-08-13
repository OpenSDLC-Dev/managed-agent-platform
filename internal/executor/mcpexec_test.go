package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp/mcptest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// appendMCPToolUse plants the call the brain will commit once MCP tools reach a
// model — the reference shape, with the server name the result does not carry
// and the driver therefore has to read from here.
func (h *harness) appendMCPToolUse(t *testing.T, server, name, input string) string {
	t.Helper()
	payload := fmt.Sprintf(`{"name":%q,"mcp_server_name":%q,"input":%s,"session_thread_id":null}`,
		name, server, input)
	evs, err := h.log.Append(context.Background(), h.sid,
		[]events.NewEvent{{Type: domain.EventAgentMCPToolUse, Payload: json.RawMessage(payload)}})
	if err != nil {
		t.Fatalf("append mcp tool use: %v", err)
	}
	return evs[0].ID.String()
}

// mcpResults reads the session's agent.mcp_tool_result events in log order.
func (h *harness) mcpResults(t *testing.T) []map[string]any {
	t.Helper()
	evs, err := h.log.List(context.Background(), h.sid, events.ListQuery{
		Types: []string{string(domain.EventAgentMCPToolResult)}})
	if err != nil {
		t.Fatalf("list mcp results: %v", err)
	}
	out := make([]map[string]any, 0, len(evs))
	for _, ev := range evs {
		var m map[string]any
		if err := json.Unmarshal(ev.Body, &m); err != nil {
			t.Fatalf("decode mcp result: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func blocksOf(t *testing.T, res map[string]any) []map[string]any {
	t.Helper()
	raw, ok := res["content"].([]any)
	if !ok {
		t.Fatalf("result content = %v, want an array", res["content"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, b := range raw {
		out = append(out, b.(map[string]any))
	}
	return out
}

// TestMCPCallIsAnsweredWithItsOutput is the driver's second job, and the one the
// session waits on: an unanswered agent.mcp_tool_use is dialled, called, and
// answered with an agent.mcp_tool_result carrying the server's own blocks.
func TestMCPCallIsAnsweredWithItsOutput(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: "the answer"})
	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "search", `{"q":"x"}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %v, want one", results)
	}
	if results[0]["mcp_tool_use_id"] != useID {
		t.Errorf("mcp_tool_use_id = %v, want %s", results[0]["mcp_tool_use_id"], useID)
	}
	if results[0]["is_error"] == true {
		t.Errorf("is_error = true, want the call to have succeeded: %v", results[0])
	}
	blocks := blocksOf(t, results[0])
	if len(blocks) != 1 || blocks[0]["type"] != "text" || blocks[0]["text"] != "the answer" {
		t.Errorf("content = %v, want the server's text block", blocks)
	}
}

// TestMCPToolFailureIsTheModelsToRead: a tool that ran and failed is an ordinary
// result with is_error, never a work-item fault. Faulting would reclaim-loop the
// item against a tool that will fail again, and the model can read the error and
// try different arguments — the same contract a sandbox tool's nonzero exit has.
func TestMCPToolFailureIsTheModelsToRead(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: "no such record", IsError: true})
	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 || results[0]["mcp_tool_use_id"] != useID {
		t.Fatalf("results = %v, want one answering %s", results, useID)
	}
	if results[0]["is_error"] != true {
		t.Errorf("is_error = %v, want true", results[0]["is_error"])
	}
	if blocks := blocksOf(t, results[0]); len(blocks) != 1 || blocks[0]["text"] != "no such record" {
		t.Errorf("content = %v, want the tool's own message", blocks)
	}
	if n := h.liveOf(t, queue.MCPExec); n != 0 {
		t.Errorf("live mcp_exec = %d, want 0 — a failed tool is answered, not retried", n)
	}
}

// TestMCPTransportFailureIsAnsweredAndReported: a call that never happened is
// both — an is_error result so the turn can continue, and a session.error so the
// platform sees a connection it must heal. Collapsing the two loses one of them:
// answering only would hide a dead server from the operator, and reporting only
// would leave the call unanswered and wedge every later replay.
func TestMCPTransportFailureIsAnsweredAndReported(t *testing.T) {
	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"docs", "http://127.0.0.1:1/mcp"})
	useID := h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 || results[0]["mcp_tool_use_id"] != useID {
		t.Fatalf("results = %v, want one answering %s", results, useID)
	}
	if results[0]["is_error"] != true {
		t.Errorf("is_error = %v, want true", results[0]["is_error"])
	}
	errs, err := h.log.List(context.Background(), h.sid, events.ListQuery{
		Types: []string{string(domain.EventSessionError)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 1 {
		t.Fatalf("session.error count = %d, want 1", len(errs))
	}
	var e struct {
		Error struct {
			Type          string `json:"type"`
			MCPServerName string `json:"mcp_server_name"`
			Message       string `json:"message"`
			RetryStatus   string `json:"retry_status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(errs[0].Body, &e); err != nil {
		t.Fatalf("decode session.error: %v", err)
	}
	if e.Error.Type != "mcp_connection_failed_error" || e.Error.MCPServerName != "docs" {
		t.Errorf("session.error = %+v, want mcp_connection_failed_error naming docs", e.Error)
	}
	// The endpoint is customer-supplied and may carry a credential; the message
	// is cut to scheme://host exactly as a catalog reason is.
	if strings.Contains(e.Error.Message, "/mcp") {
		t.Errorf("message = %q, want the endpoint cut to scheme://host", e.Error.Message)
	}
}

// TestMCPBlocksBecomeTheirAnthropicShapes pins the conversion, block by block.
// The wire admits exactly four block types in an agent.mcp_tool_result, and MCP
// hands back five of its own — so each mapping is a decision, and one that
// silently dropped a block would lose output the model was told it would get.
func TestMCPBlocksBecomeTheirAnthropicShapes(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G'}
	url := mcptest.Server(t, mcptest.Tool{Name: "mixed", Blocks: []mcptest.Block{
		{Type: "text", Text: "hello"},
		{Type: "image", Data: png, MIMEType: "image/png"},
		{Type: "resource", URI: "file:///notes.txt", MIMEType: "text/plain", Text: "note body"},
		{Type: "resource", URI: "file:///doc.pdf", MIMEType: "application/pdf", Data: png},
		{Type: "resource_link", URI: "https://example.test/x", MIMEType: "text/html"},
		{Type: "audio", Data: png, MIMEType: "audio/wav"},
	}})
	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "mixed", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %v, want one", results)
	}
	blocks := blocksOf(t, results[0])
	if len(blocks) != 6 {
		t.Fatalf("got %d blocks, want one per block the server sent: %v", len(blocks), blocks)
	}

	if blocks[0]["type"] != "text" || blocks[0]["text"] != "hello" {
		t.Errorf("text block = %v", blocks[0])
	}

	img := blocks[1]
	if img["type"] != "image" {
		t.Fatalf("image block = %v", img)
	}
	src := img["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" ||
		src["data"] != base64.StdEncoding.EncodeToString(png) {
		t.Errorf("image source = %v", src)
	}

	// An embedded text resource is a document the model can read inline.
	doc := blocks[2]
	if doc["type"] != "document" {
		t.Fatalf("text resource block = %v", doc)
	}
	dsrc := doc["source"].(map[string]any)
	if dsrc["type"] != "text" || dsrc["data"] != "note body" || dsrc["media_type"] != "text/plain" {
		t.Errorf("text resource source = %v", dsrc)
	}
	if doc["title"] != "file:///notes.txt" {
		t.Errorf("document title = %v, want the resource URI", doc["title"])
	}

	// An embedded blob resource is the same document, carried as base64.
	blob := blocks[3]
	bsrc := blob["source"].(map[string]any)
	if blob["type"] != "document" || bsrc["type"] != "base64" ||
		bsrc["media_type"] != "application/pdf" || bsrc["data"] != base64.StdEncoding.EncodeToString(png) {
		t.Errorf("blob resource block = %v", blob)
	}

	// A resource link carries no content — only a pointer. It becomes text
	// naming the resource, never a url document source: that would have the
	// model's own side fetch an address the MCP server chose.
	link := blocks[4]
	if link["type"] != "text" || !strings.Contains(link["text"].(string), "https://example.test/x") {
		t.Errorf("resource_link block = %v, want text naming the URI", link)
	}

	// Audio has no block type on this wire at all, so it is described rather
	// than dropped: the model is told something came back it cannot read.
	audio := blocks[5]
	if audio["type"] != "text" || !strings.Contains(audio["text"].(string), "audio/wav") {
		t.Errorf("audio block = %v, want text naming the media type", audio)
	}
}

// TestMCPCallsAreAnsweredBeforeDiscoveryRuns: one item does one job. A session
// with an outstanding call needs that call answered — running discovery first
// would spend the pass's budget dialling for a listing nothing is waiting on,
// while the turn sits on an unanswered call.
func TestMCPCallsAreAnsweredBeforeDiscoveryRuns(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: "answered"})
	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if len(h.mcpResults(t)) != 1 {
		t.Errorf("the outstanding call was not answered")
	}
	if got := h.catalog(t); len(got) != 0 {
		t.Errorf("catalog = %v, want none written — this pass had a call to answer", got)
	}
}

// A pass that answers every call it found must hand the turn on. Nothing else
// wakes the session — the brain is not running, and no client can answer an MCP
// call.
func TestAnsweredMCPCallsWakeTheBrain(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: "answered"})
	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1 — every call is answered", n)
	}
}

// The MCP driver's own settlement chains what it cannot answer. A pass answers
// only the calls it found at its start, so a call the brain commits while it
// runs is one nothing else will ever answer — the client cannot post an MCP
// result and no BYOC worker sees the call — and completing over it would strand
// the session: it keeps HasUnansweredToolUse true, so no model_turn follows
// either.
//
// The chain is this item handed back, not a second one enqueued. Enqueue is
// keyed (session_id, kind) over the live states, so an mcp_exec raised while
// this mcp_exec is still active is dropped on conflict — silently, since the
// conflict is not an error — and the session would wait on work nobody queued.
func TestMCPPassChainsItsOwnItemForACallThatArrivedMidPass(t *testing.T) {
	h := mcpHarness(t)
	type planted struct {
		id  string
		err error
	}
	mid := make(chan planted, 1)
	// Once: the late call is planted by the first pass alone. Planting on every
	// call would chain a pass for a call the next pass plants again, and the
	// test would describe a loop rather than the heal.
	var once sync.Once
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: "found", While: func() {
		once.Do(func() {
			evs, err := h.log.Append(context.Background(), h.sid, []events.NewEvent{{
				Type:    domain.EventAgentMCPToolUse,
				Payload: json.RawMessage(`{"name":"search","mcp_server_name":"docs","input":{"n":2},"session_thread_id":null}`),
			}})
			if err != nil {
				mid <- planted{err: err}
				return
			}
			mid <- planted{id: evs[0].ID.String()}
		})
	}})
	h.declareMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "search", `{"n":1}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	late := <-mid
	if late.err != nil {
		t.Fatalf("plant the mid-pass call: %v", late.err)
	}
	if n := len(h.mcpResults(t)); n != 1 {
		t.Fatalf("results after the first pass = %d, want 1 (the call it found)", n)
	}
	if n := h.liveOf(t, queue.MCPExec); n != 1 {
		t.Fatalf("live mcp_exec = %d, want 1 — the item is handed back for the late call", n)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 0 {
		t.Errorf("live model_turn = %d, want 0 — a call is still unanswered", n)
	}

	// The chained pass answers the late call and only then wakes the brain.
	h.stepOnce(t)
	results := h.mcpResults(t)
	if len(results) != 2 {
		t.Fatalf("results after the chained pass = %d, want 2", len(results))
	}
	if results[1]["mcp_tool_use_id"] != late.id {
		t.Errorf("second result answers %v, want the late call %s", results[1]["mcp_tool_use_id"], late.id)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1 — everything is answered now", n)
	}
}

// A sandbox pass that finds an MCP call outstanding chains an mcp_exec rather
// than completing over it. No enqueue site produces that log — every one holds
// a tool_exec back behind an outstanding MCP call — but the heal is what the
// web one already is: the alternative is a session stranded for good.
func TestSandboxPassChainsMCPRatherThanStranding(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: "found"})
	h := mcpHarness(t)
	h.declareMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.suspend(t, writeUse("out.txt", "hello"))

	h.stepOnce(t)

	if n := len(h.toolResults(t)); n != 1 {
		t.Fatalf("sandbox results = %d, want 1 (the pass still ran its own tool)", n)
	}
	if n := h.liveOf(t, queue.ToolExec); n != 0 {
		t.Errorf("live tool_exec = %d, want 0 (completed)", n)
	}
	if n := h.liveOf(t, queue.MCPExec); n != 1 {
		t.Fatalf("live mcp_exec = %d, want 1 (chained for the MCP call)", n)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 0 {
		t.Errorf("live model_turn = %d, want 0 — the MCP call is still unanswered", n)
	}

	h.stepOnce(t)
	if n := len(h.mcpResults(t)); n != 1 {
		t.Fatalf("mcp results after the chained pass = %d, want 1", n)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1 — everything is answered now", n)
	}
}
