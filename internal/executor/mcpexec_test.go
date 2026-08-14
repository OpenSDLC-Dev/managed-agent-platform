package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp/mcptest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
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
	h.declareListedMCPServers(t, [2]string{"docs", url})
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
	h.declareListedMCPServers(t, [2]string{"docs", url})
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
	// A credential in the endpoint, because that is what the cut is for: an
	// endpoint with nothing to leak cannot tell a message that leaks one from a
	// message that does not.
	h.declareListedMCPServers(t, [2]string{"docs", "http://svc:s3cret@127.0.0.1:1/mcp"})
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
			// The union variant, not a bare string: every retry_status on this
			// wire is an object carrying a type, and a string here would decode
			// to the zero value in a client's typed union rather than fail.
			RetryStatus struct {
				Type string `json:"type"`
			} `json:"retry_status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(errs[0].Body, &e); err != nil {
		t.Fatalf("decode session.error: %v", err)
	}
	if e.Error.Type != "mcp_connection_failed_error" || e.Error.MCPServerName != "docs" {
		t.Errorf("session.error = %+v, want mcp_connection_failed_error naming docs", e.Error)
	}
	if e.Error.RetryStatus.Type != "retrying" {
		t.Errorf("retry_status = %+v, want the retrying variant — every turn re-attempts the server",
			e.Error.RetryStatus)
	}
	// The endpoint is customer-supplied and may carry a credential; the message
	// is cut to scheme://host exactly as a catalog reason is.
	if strings.Contains(e.Error.Message, "/mcp") {
		t.Errorf("message = %q, want the endpoint cut to scheme://host", e.Error.Message)
	}
	if strings.Contains(e.Error.Message, "s3cret") || strings.Contains(e.Error.Message, "svc:") {
		t.Errorf("message = %q, want the endpoint's credential gone", e.Error.Message)
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
	h.declareListedMCPServers(t, [2]string{"docs", url})
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
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	if len(h.mcpResults(t)) != 1 {
		t.Errorf("the outstanding call was not answered")
	}
	// The seeded row is the listing this call's tool was offered from. Had
	// discovery run, it would have replaced it with the server's own — which
	// reports one tool, where the seeded row reports none.
	if got := h.catalog(t); len(got) != 1 || len(got["docs"].tools) != 0 {
		t.Errorf("catalog = %v, want the row untouched — this pass had a call to answer", got)
	}
}

// A pass that answers every call it found must hand the turn on. Nothing else
// wakes the session — the brain is not running, and no client can answer an MCP
// call.
func TestAnsweredMCPCallsWakeTheBrain(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: "answered"})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
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
	h.declareListedMCPServers(t, [2]string{"docs", url})
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
	h.declareListedMCPServers(t, [2]string{"docs", url})
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

// A call naming a server the agent has since stopped declaring is answered, not
// dialled: mcp_servers is mid-session-mutable, so the endpoint can be gone by
// the time the call runs. The model is told, and nothing is reported to the
// operator — an agent edit is not a connection that failed.
func TestMCPCallForADroppedServerIsAnsweredWithoutDialling(t *testing.T) {
	h := mcpHarness(t)
	h.declareMCPServers(t) // the agent declares none
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
	if blocks := blocksOf(t, results[0]); len(blocks) != 1 ||
		!strings.Contains(blocks[0]["text"].(string), "no longer configured") {
		t.Errorf("content = %v, want the model told the server is gone", blocks)
	}
	errs, err := h.log.List(context.Background(), h.sid, events.ListQuery{
		Types: []string{string(domain.EventSessionError)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 0 {
		t.Errorf("session.error count = %d, want 0 — nothing was dialled", len(errs))
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1 — the call is answered", n)
	}
}

// A call naming a server the agent has since repointed is answered, not dialled
// at the new address. The model was offered this tool because the *old* endpoint
// published it, so the new one is being asked a question it never advertised an
// answer to — and it is a different party, which is the part that matters when
// the tool has a side effect or the input carries the session's data.
func TestMCPCallForARepointedServerIsNotDialledElsewhere(t *testing.T) {
	moved := mcptest.Server(t, mcptest.Tool{Name: "search", Result: "should never be reached"})
	h := mcpHarness(t)
	// Listed at one address, declared at another: the shape a mid-session patch
	// leaves behind if its own invalidating delete has not run.
	h.listMCPServer(t, "docs", "http://127.0.0.1:1/original")
	h.declareMCPServers(t, [2]string{"docs", moved})
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
	if blocks := blocksOf(t, results[0]); len(blocks) != 1 ||
		!strings.Contains(blocks[0]["text"].(string), "points somewhere else") {
		t.Errorf("content = %v, want the model told the server moved", blocks)
	}
	errs, err := h.log.List(context.Background(), h.sid, events.ListQuery{
		Types: []string{string(domain.EventSessionError)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 0 {
		t.Errorf("session.error count = %d, want 0 — nothing was dialled", len(errs))
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn = %d, want 1 — the call is answered", n)
	}
}

// The environment's networking policy is enforced at the dial or nowhere, since
// the platform dials from this process rather than through the per-session gate.
// A refusal answers the call — the turn must continue — and reports itself, so
// an operator who meant to admit the server can see why it was not.
func TestMCPCallRefusedByEgressPolicyIsAnsweredAndReported(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: "never reached"})
	h := mcpHarness(t)
	h.setNetworking(t, domain.Networking{Type: domain.NetLimited})
	h.declareListedMCPServers(t, [2]string{"docs", url})
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
	if blocks := blocksOf(t, results[0]); len(blocks) != 1 ||
		!strings.Contains(blocks[0]["text"].(string), "allow_mcp_servers") {
		t.Errorf("content = %v, want the refusal to name what would admit the host", blocks)
	}
	errs, err := h.log.List(context.Background(), h.sid, events.ListQuery{
		Types: []string{string(domain.EventSessionError)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 1 {
		t.Fatalf("session.error count = %d, want 1", len(errs))
	}
	// retrying, and truthfully so: the union is about the session — the SDK
	// documents terminal as the session transitioning to terminated, and this
	// session carries on with the call answered is_error — and a refusal is in
	// any case healable while the session runs, since an environment's
	// networking and an agent's mcp_servers are both patchable mid-session and
	// this driver re-reads them every pass.
	var e struct {
		Error struct {
			RetryStatus struct {
				Type string `json:"type"`
			} `json:"retry_status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(errs[0].Body, &e); err != nil {
		t.Fatalf("decode session.error: %v", err)
	}
	if e.Error.RetryStatus.Type != "retrying" {
		t.Errorf("retry_status = %q, want retrying — this session does not terminate",
			e.Error.RetryStatus.Type)
	}
	// And the session is demonstrably not terminated, which is what would have
	// made `terminal` the honest variant.
	var status string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT status FROM sessions WHERE id = $1`, h.sid.String()).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status == "terminated" {
		t.Errorf("session status = %q after a refusal, want it still live", status)
	}
}

// A tool that genuinely returns nothing still gets a block. The wire's content
// is optional, but an empty array is indistinguishable from a tool that returned
// nothing, and a Messages endpoint rejects an empty text block — which is what
// every later replay of this session would send.
func TestMCPToolThatReturnsNothingStillGetsABlock(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "noop", Blocks: []mcptest.Block{}})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "noop", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %v, want one", results)
	}
	if results[0]["is_error"] == true {
		t.Errorf("is_error = true, want a tool that succeeded with no output: %v", results[0])
	}
	blocks := blocksOf(t, results[0])
	if len(blocks) != 1 || blocks[0]["type"] != "text" || blocks[0]["text"] == "" {
		t.Errorf("content = %v, want one non-empty text block", blocks)
	}
}

// One huge text block is truncated, not dropped: it is the ordinary shape of an
// over-long answer, and a model can act on the first hundred kilobytes of a
// document where it can do nothing with an answer that vanished.
func TestMCPHugeTextAnswerIsTruncatedToTheToolBudget(t *testing.T) {
	huge := strings.Repeat("x", 4*toolset.MaxOutputBytes)
	url := mcptest.Server(t, mcptest.Tool{Name: "dump", Result: huge})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "dump", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %d, want one", len(results))
	}
	blocks := blocksOf(t, results[0])
	if len(blocks) != 1 || blocks[0]["type"] != "text" {
		t.Fatalf("content = %v, want one text block", blocks)
	}
	text := blocks[0]["text"].(string)
	if len(text) > toolset.MaxOutputBytes+64 {
		t.Errorf("text is %d bytes, want it held to the %d-byte tool budget",
			len(text), toolset.MaxOutputBytes)
	}
	if !strings.Contains(text, "truncated") {
		t.Errorf("text does not say it was truncated: %q", text[max(0, len(text)-120):])
	}
}

// Many blocks are charged against one shared budget and the answer ends at the
// first that does not fit — dropped whole, since half a base64 payload decodes
// to nothing — with a block saying how many went. A short answer that silently
// lost half its blocks would read to a model as the tool's whole output.
func TestMCPAnswerBeyondTheBudgetIsCutWithANotice(t *testing.T) {
	// Four blocks of a third of the budget each: the first three fit, the
	// fourth does not.
	third := strings.Repeat("y", toolset.MaxOutputBytes/3)
	url := mcptest.Server(t, mcptest.Tool{Name: "many", Blocks: []mcptest.Block{
		{Type: "text", Text: third},
		{Type: "text", Text: third},
		{Type: "text", Text: third},
		{Type: "text", Text: third},
		{Type: "text", Text: "the tail nobody sees"},
	}})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "many", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %d, want one", len(results))
	}
	blocks := blocksOf(t, results[0])
	// Exactly how many of the four fit depends on each block's JSON overhead,
	// so the assertion is the shape rather than the count: some were kept, some
	// were dropped, and the last block says so.
	if len(blocks) < 2 || len(blocks) >= 5 {
		t.Fatalf("got %d blocks, want some kept, some dropped, and a notice", len(blocks))
	}
	notice, _ := blocks[len(blocks)-1]["text"].(string)
	if !strings.Contains(notice, "content block(s) of this answer were dropped") {
		t.Errorf("last block = %q, want it to name what was dropped", notice)
	}
	total := 0
	for _, b := range blocks {
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		total += len(raw)
	}
	if total > toolset.MaxOutputBytes+512 {
		t.Errorf("answer is %d bytes, want it held near the %d-byte budget",
			total, toolset.MaxOutputBytes)
	}
}

// The budget binds a leading image too. Only a text block is bounded on its own
// (textBlock caps it), so exempting whatever came first would let a lone image —
// bounded by nothing but the transport's megabytes, and base64 a third larger
// again — land whole on the append-only log and ride every later replay of the
// session. There is no truncation to fall back on for one: half a base64 payload
// decodes to nothing, so it is dropped and said to have been.
func TestMCPOversizedLeadingImageIsDroppedNotExempted(t *testing.T) {
	png := make([]byte, 2*toolset.MaxOutputBytes)
	for i := range png {
		png[i] = byte(i)
	}
	url := mcptest.Server(t, mcptest.Tool{Name: "shot", Blocks: []mcptest.Block{
		{Type: "image", Data: png, MIMEType: "image/png"},
	}})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "shot", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %d, want one", len(results))
	}
	blocks := blocksOf(t, results[0])
	for _, b := range blocks {
		if b["type"] == "image" {
			t.Fatalf("the oversized image landed on the log: %v", b["type"])
		}
	}
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want just the notice", len(blocks))
	}
	notice, _ := blocks[0]["text"].(string)
	if !strings.Contains(notice, "content block(s) of this answer were dropped") {
		t.Errorf("block = %q, want it to say the image was dropped", notice)
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > toolset.MaxOutputBytes {
		t.Errorf("answer is %d bytes, want it under the %d-byte budget",
			len(raw), toolset.MaxOutputBytes)
	}
}

// A large *text* embedded resource arrives truncated, not as nothing. Its data
// is text, so resourceBlock caps it where the block is built — but a capped body
// plus its document wrapper always exceeds the whole budget, so exempting only
// text *blocks* dropped every such document whole and a 150 KB file reached the
// model as a one-line notice. The exemption follows the cap, not the block type.
func TestMCPLargeTextResourceArrivesTruncated(t *testing.T) {
	body := strings.Repeat("z", 2*toolset.MaxOutputBytes)
	url := mcptest.Server(t, mcptest.Tool{Name: "read", Blocks: []mcptest.Block{
		{Type: "resource", URI: "file:///big.txt", MIMEType: "text/plain", Text: body},
	}})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "read", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %d, want one", len(results))
	}
	blocks := blocksOf(t, results[0])
	if len(blocks) != 1 || blocks[0]["type"] != "document" {
		t.Fatalf("content = %v, want the document itself rather than a notice", blocks)
	}
	src, ok := blocks[0]["source"].(map[string]any)
	if !ok || src["type"] != "text" {
		t.Fatalf("source = %v, want the plain-text source", blocks[0]["source"])
	}
	data, _ := src["data"].(string)
	if len(data) == 0 || len(data) > toolset.MaxOutputBytes+64 {
		t.Errorf("data is %d bytes, want it truncated to the %d-byte tool budget",
			len(data), toolset.MaxOutputBytes)
	}
	if !strings.Contains(data, "truncated") {
		t.Errorf("data does not say it was truncated: %q", data[max(0, len(data)-80):])
	}
}

// The exemption's premise is that everything textual in an exempt block was
// capped where the block was built — so the two strings a document carries
// *about* its resource are capped too. Neither is content: a URI is a title and
// a media type is a note, and a server that sends megabytes of either is using a
// label as a payload. Left uncapped they walked straight past the answer budget
// inside the one block the budget exempts.
func TestMCPResourceLabelsAreCappedSoTheExemptionHolds(t *testing.T) {
	huge := strings.Repeat("u", 4*toolset.MaxOutputBytes)
	url := mcptest.Server(t, mcptest.Tool{Name: "read", Blocks: []mcptest.Block{
		{Type: "resource", URI: "file:///" + huge, MIMEType: "text/" + huge, Text: "short body"},
	}})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "read", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %d, want one", len(results))
	}
	blocks := blocksOf(t, results[0])
	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > toolset.MaxOutputBytes {
		t.Errorf("answer is %d bytes, want it under the %d-byte budget — the labels rode past it",
			len(raw), toolset.MaxOutputBytes)
	}
	// The block still arrives: capping a label must not cost the resource.
	if len(blocks) != 1 || blocks[0]["type"] != "document" {
		t.Fatalf("content = %v, want the document itself", blocks)
	}
	title, _ := blocks[0]["title"].(string)
	if title == "" || len(title) > maxResourceLabel+32 {
		t.Errorf("title is %d bytes, want it capped to %d", len(title), maxResourceLabel)
	}
	ctx, _ := blocks[0]["context"].(string)
	if len(ctx) > maxResourceLabel+128 {
		t.Errorf("context is %d bytes, want it capped to %d", len(ctx), maxResourceLabel)
	}
}

// A server may answer with an explicit empty text block — the SDK decodes it,
// the wire admits it, and it is a different shape from the empty content array
// the no-content fallback covers. A Messages endpoint does not admit it: replay
// hands an mcp_tool_result's content array straight into a Messages tool_result
// (brain/replay.go's toolResultBlock), so an empty text block on this
// append-only log is a session that fails on this turn and on every later one.
// It is the same wedge TestEmptyToolResultOmitsEmptyTextBlock keeps a built-in
// tool's empty read out of, arriving by a route only a server can open.
func TestMCPEmptyTextBlockNeverReachesTheLog(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Blocks: []mcptest.Block{
		{Type: "text", Text: ""},
	}})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 {
		t.Fatalf("results = %d, want one", len(results))
	}
	blocks := blocksOf(t, results[0])
	for i, b := range blocks {
		if b["type"] == "text" && b["text"] == "" {
			t.Errorf("block %d is an empty text block — a Messages endpoint rejects it, "+
				"so every later replay of this session would fail", i)
		}
	}
	// The answer still says something: dropping the empty block must not leave a
	// result with no content, which is the other shape the model cannot read.
	if len(blocks) != 1 {
		t.Fatalf("content = %v, want the one block that says the tool answered with nothing", blocks)
	}
}

// A tool the server does not have is not a server that could not be reached.
// The connection opened, the server answered, and it answered with a JSON-RPC
// error — so the model is told the call failed and the operator is told nothing,
// because mcp_connection_failed_error is documented as "Failed to connect to an
// MCP server" and this platform does not have a connection to heal.
func TestMCPProtocolFailureIsNotReportedAsAConnectionFailure(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Fail: "no such tool"})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 || results[0]["mcp_tool_use_id"] != useID {
		t.Fatalf("results = %v, want one answering %s", results, useID)
	}
	if results[0]["is_error"] != true {
		t.Errorf("is_error = %v, want true — the call did not produce an answer", results[0]["is_error"])
	}
	errs, err := h.log.List(context.Background(), h.sid, events.ListQuery{
		Types: []string{string(domain.EventSessionError)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 0 {
		t.Errorf("session.error count = %d, want 0 — the server was reached and answered", len(errs))
	}
	// The turn still moves: the call is answered, so the model runs again.
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("model_turn = %d, want 1", n)
	}
}

// The calls in a turn are run one after another, so without a bound on the pass
// a turn holding several slow servers owns this process's single work goroutine
// for as long as they care to take, and every other session on the replica waits
// behind it. Discovery is bounded for exactly this reason; execution is bounded
// the same way, and hands the rest back rather than dropping it — the calls it
// already made are committed, so nothing with a side effect runs twice.
func TestMCPPassStopsAtItsBudgetAndHandsTheRestBack(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: "answered", While: func() {
		time.Sleep(120 * time.Millisecond)
	}})
	h := mcpHarnessWith(t, Config{MCPPassTimeout: 40 * time.Millisecond})
	h.declareListedMCPServers(t, [2]string{"docs", url})
	first := h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.appendMCPToolUse(t, "docs", "search", `{"q":"second"}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 || results[0]["mcp_tool_use_id"] != first {
		t.Fatalf("results = %v, want only the first call answered — the budget stops the pass "+
			"before the second, and never before the first, or no pass makes progress", results)
	}
	// The call left over is this item's to finish, so the item comes back rather
	// than the session going idle on an unanswered call.
	if n := h.liveOf(t, queue.MCPExec); n != 1 {
		t.Errorf("mcp_exec live = %d, want 1 — the outstanding call keeps the item", n)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 0 {
		t.Errorf("model_turn = %d, want 0 — a call is still unanswered", n)
	}
}

// An MCP read of an empty file is the resource-shaped twin of the empty text
// block: a document whose source holds nothing. Rather than put an empty payload
// on the log in a shape whose acceptance nothing here has established, the
// address is said in text.
func TestMCPEmptyResourceIsSaidRatherThanSentEmpty(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "read", Blocks: []mcptest.Block{
		{Type: "resource", URI: "file:///empty.txt", MIMEType: "text/plain", Text: ""},
	}})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "read", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	blocks := blocksOf(t, h.mcpResults(t)[0])
	if len(blocks) != 1 {
		t.Fatalf("content = %v, want one block", blocks)
	}
	if blocks[0]["type"] != "text" {
		t.Fatalf("block = %v, want text — an empty resource has no payload to carry", blocks[0])
	}
	if txt, _ := blocks[0]["text"].(string); !strings.Contains(txt, "file:///empty.txt") {
		t.Errorf("text = %q, want it to name the resource", txt)
	}
}

// The budget must never starve the pass of its first call. A bound short enough
// to have expired before the loop begins would otherwise answer nothing and hand
// the item straight back, and the item would come back to do it again — a queue
// spinning on an unanswerable turn rather than a slow one.
func TestMCPPassAlwaysMakesItsFirstCall(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: "answered"})
	h := mcpHarnessWith(t, Config{MCPPassTimeout: time.Nanosecond})
	h.declareListedMCPServers(t, [2]string{"docs", url})
	useID := h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	results := h.mcpResults(t)
	if len(results) != 1 || results[0]["mcp_tool_use_id"] != useID {
		t.Fatalf("results = %v, want the call answered whatever the budget says", results)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("model_turn = %d, want 1 — the turn moves on", n)
	}
}

// The two fields an image's source requires are the server's to supply, and a
// block missing either cannot be sent as an image: the schema marks both
// required, and an empty one is a required field left blank. MCP requires a
// mimeType on image content, so a block without one is a server out of spec
// rather than a case to guess at — the bytes are described, the way audio is.
func TestMCPImageWithoutItsRequiredFieldsIsDescribed(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "shot", Blocks: []mcptest.Block{
		{Type: "image", Data: []byte{0x89, 'P', 'N', 'G'}},     // no media type
		{Type: "image", MIMEType: "image/png", Data: []byte{}}, // no bytes
		// A real MIME type the image block's own source has no slot for: the
		// Messages request fixes that field at four values, so this one can be
		// stored and never sent again.
		{Type: "image", MIMEType: "image/svg+xml", Data: []byte("<svg/>")},
	}})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "shot", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	blocks := blocksOf(t, h.mcpResults(t)[0])
	if len(blocks) != 3 {
		t.Fatalf("content = %v, want all three blocks accounted for", blocks)
	}
	for i, b := range blocks {
		if b["type"] != "text" {
			t.Errorf("block %d = %v, want it described rather than sent as an image", i, b)
		}
	}
}

// What a block may declare is the intersection of two schemas. The session event
// takes a free string for a media type; the brain replays a result's content
// array into a Messages tool_result unchanged, and there a base64 document's
// media type is the single constant application/pdf and an image's is an enum of
// four. A block this platform can store but never send again fails every later
// turn of the session, on a log that cannot take it back — so a blob rides the
// block type that can carry it, and is described when neither can.
func TestMCPBlobResourceRidesOnlyABlockTheWireCanCarry(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G'}
	url := mcptest.Server(t, mcptest.Tool{Name: "read", Blocks: []mcptest.Block{
		{Type: "resource", URI: "file:///report.pdf", MIMEType: "application/pdf", Data: []byte{'%', 'P', 'D', 'F'}},
		{Type: "resource", URI: "file:///shot.png", MIMEType: "image/png", Data: png},
		{Type: "resource", URI: "file:///bundle.zip", MIMEType: "application/zip", Data: png},
		{Type: "resource", URI: "file:///opaque", Data: png},
	}})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "read", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	blocks := blocksOf(t, h.mcpResults(t)[0])
	if len(blocks) != 4 {
		t.Fatalf("content = %v, want all four resources accounted for", blocks)
	}
	pdf, ok := blocks[0]["source"].(map[string]any)
	if !ok || blocks[0]["type"] != "document" || pdf["media_type"] != "application/pdf" {
		t.Errorf("block 0 = %v, want the pdf as a document source", blocks[0])
	}
	img, ok := blocks[1]["source"].(map[string]any)
	if !ok || blocks[1]["type"] != "image" || img["media_type"] != "image/png" {
		t.Errorf("block 1 = %v, want the png as an image block", blocks[1])
	}
	// Neither block type can declare these, so they are said rather than sent.
	for i, name := range []string{"file:///bundle.zip", "file:///opaque"} {
		b := blocks[2+i]
		if b["type"] != "text" {
			t.Fatalf("block %d = %v, want it described — no block type can carry it", 2+i, b)
		}
		if txt, _ := b["text"].(string); !strings.Contains(txt, name) {
			t.Errorf("block %d text = %q, want it to name %s", 2+i, txt, name)
		}
	}
}

// The budget exempts one block so an over-long answer arrives truncated rather
// than not at all, and which block it exempts has to be the one that was capped
// — not whichever came first. A server that sends a thumbnail and then its
// report would otherwise have the report vanish for standing second, which is
// exactly the answer the exemption exists to keep readable.
func TestMCPExemptionFollowsTheCappedBlockNotThePosition(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "report", Blocks: []mcptest.Block{
		{Type: "image", MIMEType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}},
		{Type: "text", Text: strings.Repeat("r", 4*toolset.MaxOutputBytes)},
	}})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "report", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	blocks := blocksOf(t, h.mcpResults(t)[0])
	if len(blocks) != 2 {
		t.Fatalf("content = %v, want the thumbnail and the report", blocks)
	}
	if blocks[0]["type"] != "image" {
		t.Errorf("block 0 = %v, want the image kept — it fits", blocks[0])
	}
	txt, _ := blocks[1]["text"].(string)
	if blocks[1]["type"] != "text" {
		t.Fatalf("block 1 = %v, want the report truncated rather than dropped", blocks[1])
	}
	// It must be the report itself and not the notice that says the report was
	// dropped: both are short text blocks in the second slot, and an assertion
	// that only checked the type and the length would pass against either.
	if !strings.HasPrefix(txt, "rrrr") {
		t.Fatalf("block 1 text = %.80q, want the report's own bytes — a notice here means it was dropped", txt)
	}
	// The cap plus its own truncation marker, not the raw four times over.
	if len(txt) < toolset.MaxOutputBytes/2 || len(txt) > toolset.MaxOutputBytes+64 {
		t.Errorf("report is %d bytes, want it capped to about %d", len(txt), toolset.MaxOutputBytes)
	}
}

// The model-visible text of a failed call quotes the platform's own error, and
// that error quotes the endpoint — which is customer-supplied and may carry a
// credential. This is the arm no test drove: a call that reached the server and
// failed there, rather than one that never connected.
func TestMCPFailedCallDoesNotShowTheModelTheEndpointsCredential(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "search", Fail: "boom"})
	h := mcpHarness(t)
	// The fixture's own address, with a credential in front of it.
	h.declareListedMCPServers(t, [2]string{"docs", withUserinfo(url)})
	h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	blocks := blocksOf(t, h.mcpResults(t)[0])
	if len(blocks) != 1 {
		t.Fatalf("content = %v, want one block", blocks)
	}
	txt, _ := blocks[0]["text"].(string)
	if txt == "" {
		t.Fatalf("block = %v, want the failure said to the model", blocks[0])
	}
	if strings.Contains(txt, "s3cret") || strings.Contains(txt, "svc:") {
		t.Errorf("model-visible text = %q, want the endpoint's credential gone", txt)
	}
}

// withUserinfo puts a credential in front of a fixture's host.
func withUserinfo(raw string) string {
	return strings.Replace(raw, "http://", "http://svc:s3cret@", 1)
}

// The duration metric's tool name has until now been one of eight fixed
// strings. A server's name and its tools' names are neither fixed nor ours, so
// putting them in a metric label multiplies time series without bound in
// whatever backend collects them — the classic cardinality blowup, and one no
// other assertion in this package would notice. Which tool ran is on the event
// log, where it costs storage rather than series.
func TestMCPCallsShareOneMetricLabel(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	url := mcptest.Server(t, mcptest.Tool{Name: "search", Result: "answered"})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "search", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var names []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != toolset.MetricToolDuration {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				continue
			}
			for _, dp := range hist.DataPoints {
				if v, ok := dp.Attributes.Value(semconv.GenAIToolNameKey); ok {
					names = append(names, v.AsString())
				}
			}
		}
	}
	if len(names) != 1 {
		t.Fatalf("tool-name label values = %v, want exactly one", names)
	}
	if names[0] != "mcp" {
		t.Errorf("tool name = %q, want the one bounded value — a server's and a tool's names "+
			"are third-party and belong on the log, not in a metric label", names[0])
	}
}

// A document's title is optional on the request side, so an absent one is a
// shape the endpoint is known to take and an empty one is not — the same
// standard the hollow blocks are held to. Only a server violating MCP's
// required-URI rule gets here, which is exactly why nothing else would notice.
func TestMCPResourceWithNoAddressOmitsItsTitle(t *testing.T) {
	url := mcptest.Server(t, mcptest.Tool{Name: "read", Blocks: []mcptest.Block{
		{Type: "resource", MIMEType: "text/plain", Text: "body without an address"},
	}})
	h := mcpHarness(t)
	h.declareListedMCPServers(t, [2]string{"docs", url})
	h.appendMCPToolUse(t, "docs", "read", `{}`)
	h.enqueueMCP(t)

	h.stepOnce(t)

	blocks := blocksOf(t, h.mcpResults(t)[0])
	if len(blocks) != 1 || blocks[0]["type"] != "document" {
		t.Fatalf("content = %v, want the resource as a document", blocks)
	}
	if _, ok := blocks[0]["title"]; ok {
		t.Errorf("block = %v, want no title at all rather than an empty one", blocks[0])
	}
}
