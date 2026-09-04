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
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

const mcpServerURL = "https://docs.example.test/mcp"

// mcpAgent puts an agent declaring one MCP server on the session, with the
// given tools[] entries.
func mcpAgent(t *testing.T, h *harness, tools string) {
	t.Helper()
	agentJSON := `{"type":"agent","id":"agent_mcp","version":1,"name":"n",
		"model":{"id":"fixture-model"},"system":"do the task","description":"",
		"tools":[` + tools + `],
		"mcp_servers":[{"type":"url","name":"docs","url":"` + mcpServerURL + `"}],
		"skills":[],"multiagent":null}`
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = $2 WHERE id = $1`, h.sessionID.String(), agentJSON); err != nil {
		t.Fatal(err)
	}
}

// listing writes the catalog row the executor's discovery driver would have
// written for the server.
func listing(t *testing.T, h *harness, status, tools string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO mcp_catalogs (session_id, server_name, url, tools, status)
		 VALUES ($1, 'docs', $2, $3::jsonb, $4)`,
		h.sessionID.String(), mcpServerURL, tools, status); err != nil {
		t.Fatal(err)
	}
}

const searchTool = `[{"name":"search","description":"Search the docs.","input_schema":{"type":"object"}}]`

// requestTools reads the model-facing tool names out of one captured request.
func requestTools(t *testing.T, req provider.Request) []string {
	t.Helper()
	out := make([]string, len(req.Tools))
	for i, raw := range req.Tools {
		var d struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		out[i] = d.Name
	}
	return out
}

// A server this session has never listed has tools nobody can name, so the turn
// is not assembled at all: no model call, the item comes back as mcp_exec, and
// the session stays running while discovery runs. Sending the turn without the
// tools would be worse than waiting — the agent's whole reason for declaring the
// server would silently miss its first turn.
func TestATurnWaitsForItsFirstMCPListing(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{textChunk(0, "unused"), done("end_turn", 3)}}, nil)
	mcpAgent(t, h, `{"type":"mcp_toolset","mcp_server_name":"docs"}`)

	h.wake(t, "search the docs")
	h.runOnce(t)

	if n := len(h.provider.calls); n != 0 {
		t.Fatalf("model requests = %d, want 0 (the turn should not have been assembled)", n)
	}
	if got := h.liveOf(t, queue.MCPExec); got != 1 {
		t.Errorf("mcp_exec items = %d, want 1", got)
	}
	if got := h.liveOf(t, queue.ModelTurn); got != 0 {
		t.Errorf("model_turn live = %d, want 0 (handed back as mcp work)", got)
	}
	if got := h.status(t); got != "running" {
		t.Errorf("status = %q, want running (waiting on a listing is working, not waiting on input)", got)
	}
	want := []string{"user.message", "session.status_running"}
	if got := h.types(t); !typesEqual(got, want) {
		t.Errorf("log = %v, want %v — a suspend writes no conversation", got, want)
	}
}

// A listing in hand, the server's tools reach the model under the prefixed name,
// and a call on one commits as agent.mcp_tool_use with the server and the bare
// name in the two fields the wire has for them. always_ask is the MCP toolset's
// default, so the turn gates on confirmation like any other ask tool.
func TestAListedMCPToolIsOfferedAndGated(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolUseChunk("toolu_1", "mcp__docs__search"),
		done("tool_use", 3),
	}}, nil)
	mcpAgent(t, h, `{"type":"mcp_toolset","mcp_server_name":"docs"}`)
	listing(t, h, "ready", searchTool)

	h.wake(t, "search the docs")
	h.runOnce(t)

	if n := len(h.provider.calls); n != 1 {
		t.Fatalf("model requests = %d, want 1", n)
	}
	if got := requestTools(t, h.provider.calls[0]); len(got) != 1 || got[0] != "mcp__docs__search" {
		t.Fatalf("offered tools = %v, want the one prefixed MCP name", got)
	}

	useEvs, _ := h.log.List(context.Background(), h.sessionID,
		events.ListQuery{Types: []string{string(domain.EventAgentMCPToolUse)}})
	if len(useEvs) != 1 {
		t.Fatalf("agent.mcp_tool_use events = %d, want 1", len(useEvs))
	}
	var use struct {
		Name                string `json:"name"`
		Server              string `json:"mcp_server_name"`
		EvaluatedPermission string `json:"evaluated_permission"`
	}
	if err := json.Unmarshal(useEvs[0].Body, &use); err != nil {
		t.Fatal(err)
	}
	if use.Name != "search" || use.Server != "docs" {
		t.Errorf("intent = %+v, want the bare name and the server apart", use)
	}
	if use.EvaluatedPermission != "ask" {
		t.Errorf("evaluated_permission = %q, want ask (the MCP toolset's default)", use.EvaluatedPermission)
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle (gated on confirmation)", got)
	}
	if got := h.liveOf(t, queue.MCPExec); got != 0 {
		t.Errorf("mcp_exec items = %d, want 0 while the call awaits confirmation", got)
	}
}

// A turn mixing an allowed MCP call with a sandbox tool enqueues mcp_exec and
// nothing else. Only this platform's MCP driver answers an agent.mcp_tool_use,
// and a tool_exec is the one kind a BYOC worker claims — it would take the item,
// answer the built-in, and leave the MCP call to nobody. The MCP driver answers
// its own family and chains the rest.
func TestAnAllowedMCPCallSchedulesTheMCPDriverFirst(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolUseChunk("toolu_1", "mcp__docs__search"),
		provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
			ID: "toolu_2", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}},
		done("tool_use", 3),
	}}, nil)
	mcpAgent(t, h, `{"type":"agent_toolset_20260401"},
		{"type":"mcp_toolset","mcp_server_name":"docs",
		 "default_config":{"permission_policy":{"type":"always_allow"}}}`)
	listing(t, h, "ready", searchTool)

	h.wake(t, "search then list")
	h.runOnce(t)

	if got := h.liveOf(t, queue.MCPExec); got != 1 {
		t.Errorf("mcp_exec items = %d, want 1", got)
	}
	if got := h.liveOf(t, queue.ToolExec); got != 0 {
		t.Errorf("tool_exec items = %d, want 0 (the MCP driver chains it)", got)
	}
	if got := h.liveOf(t, queue.WebExec); got != 0 {
		t.Errorf("web_exec items = %d, want 0", got)
	}
	if got := h.status(t); got != "running" {
		t.Errorf("status = %q, want running (suspended on its tools)", got)
	}
}

// The web tools already displace a sandbox tool_exec, so a turn carrying both a
// web call and an MCP call is where the ranking is actually decided: mcp_exec
// outranks web_exec, not merely tool_exec. The web driver chains what is left.
func TestAnMCPCallOutranksAWebCallOnTheSameTurn(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		provider.Chunk{Kind: provider.KindToolUse, ToolUse: &provider.ToolUse{
			ID: "toolu_1", Name: "web_fetch", Input: json.RawMessage(`{"url":"https://x.test"}`)}},
		toolUseChunk("toolu_2", "mcp__docs__search"),
		done("tool_use", 3),
	}}, nil)
	mcpAgent(t, h, `{"type":"agent_toolset_20260401"},
		{"type":"mcp_toolset","mcp_server_name":"docs",
		 "default_config":{"permission_policy":{"type":"always_allow"}}}`)
	listing(t, h, "ready", searchTool)

	h.wake(t, "fetch then search")
	h.runOnce(t)

	if got := h.liveOf(t, queue.MCPExec); got != 1 {
		t.Errorf("mcp_exec items = %d, want 1", got)
	}
	if got := h.liveOf(t, queue.WebExec); got != 0 {
		t.Errorf("web_exec items = %d, want 0 (the MCP driver chains it)", got)
	}
}

// A server whose discovery failed has a row, and a row is an answer: the turn
// runs without that server's tools rather than suspending to re-dial an endpoint
// that just refused. The alternative loops forever against a server that is down.
func TestAFailedListingRunsTheTurnWithoutThatServer(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{textChunk(0, "no tools for me"), done("end_turn", 3)}}, nil)
	mcpAgent(t, h, `{"type":"agent_toolset_20260401"},{"type":"mcp_toolset","mcp_server_name":"docs"}`)
	listing(t, h, "failed", `[]`)

	h.wake(t, "search the docs")
	h.runOnce(t)

	if n := len(h.provider.calls); n != 1 {
		t.Fatalf("model requests = %d, want 1 (a failed listing must not suspend the turn)", n)
	}
	offered := requestTools(t, h.provider.calls[0])
	for _, name := range offered {
		if strings.HasPrefix(name, "mcp__") {
			t.Errorf("offered %q from a server whose listing failed", name)
		}
	}
	// The agent's own toolset still reaches the model: one server's failure costs
	// that server's tools and nothing else.
	if !slices.Contains(offered, "bash") {
		t.Errorf("offered = %v, want the agent's built-in tools kept", offered)
	}
	if got := h.liveOf(t, queue.MCPExec); got != 0 {
		t.Errorf("mcp_exec items = %d, want 0", got)
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle (the turn ran to its end)", got)
	}
}

// A listing written for an endpoint the agent no longer declares describes
// nothing this turn may use: it counts as no row, and the turn waits for
// discovery to re-list the server at the address it now has. A mid-session patch
// deletes such rows in its own transaction, so this is the second line — but a
// listing attributed to the wrong endpoint surfaces as a model calling tools
// that do not exist.
func TestAListingForAnotherEndpointIsNotUsed(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{textChunk(0, "unused"), done("end_turn", 3)}}, nil)
	mcpAgent(t, h, `{"type":"mcp_toolset","mcp_server_name":"docs"}`)
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO mcp_catalogs (session_id, server_name, url, tools, status)
		 VALUES ($1, 'docs', 'https://old.example.test/mcp', $2::jsonb, 'ready')`,
		h.sessionID.String(), searchTool); err != nil {
		t.Fatal(err)
	}

	h.wake(t, "search the docs")
	h.runOnce(t)

	if n := len(h.provider.calls); n != 0 {
		t.Fatalf("model requests = %d, want 0 (the stale listing must not be offered)", n)
	}
	if got := h.liveOf(t, queue.MCPExec); got != 1 {
		t.Errorf("mcp_exec items = %d, want 1", got)
	}
}

// A turn that suspends for discovery never began, so the outcome it would work
// toward stays pending. Flipping first would put "the agent begins work now" on
// an entry that then produces nothing at all until a listing arrives — and on a
// server that is slow or down, that is the state a reader of the session sees.
func TestASuspendedTurnLeavesItsOutcomePending(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{textChunk(0, "unused"), done("end_turn", 3)}}, nil)
	mcpAgent(t, h, `{"type":"mcp_toolset","mcp_server_name":"docs"}`)

	h.wakeOutcome(t, "write the report", 3)
	h.runOnce(t)

	if n := len(h.provider.calls); n != 0 {
		t.Fatalf("model requests = %d, want 0 (the turn should have suspended)", n)
	}
	evals := h.outcomes(t)
	if len(evals) != 1 {
		t.Fatalf("outcome entries = %d, want 1", len(evals))
	}
	if evals[0].Result != domain.OutcomeResultPending {
		t.Errorf("outcome result = %q, want %q — no work has begun",
			evals[0].Result, domain.OutcomeResultPending)
	}
}

// A stored spec this platform cannot read back fails the same way on every
// retry, so handing the error to the queue would reclaim-loop the turn forever
// and tell nobody. It fails the turn visibly instead, exactly as a corrupt
// resolved_agent does.
func TestACorruptMCPServerSpecFailsTheTurn(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{textChunk(0, "unused"), done("end_turn", 3)}}, nil)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = jsonb_set(resolved_agent, '{mcp_servers}', '["not an object"]'::jsonb)
		  WHERE id = $1`, h.sessionID.String()); err != nil {
		t.Fatal(err)
	}

	h.wake(t, "search the docs")
	h.runOnce(t)

	if n := len(h.provider.calls); n != 0 {
		t.Fatalf("model requests = %d, want 0", n)
	}
	errs := h.eventsOfType(t, domain.EventSessionError)
	if len(errs) != 1 {
		t.Fatalf("session.error count = %d, want 1 — a permanent failure must be said out loud", len(errs))
	}
	if got := h.status(t); got != "idle" {
		t.Errorf("status = %q, want idle (the turn ended)", got)
	}
	if got := h.liveOf(t, queue.MCPExec); got != 0 {
		t.Errorf("mcp_exec items = %d, want 0", got)
	}
}

// A name the model was never offered is answered inline as unknown rather
// than parked as agent.custom_tool_use: no client declared it, so nothing
// would ever post a result, and the settlement's own answer is what lets the
// model self-correct instead of stranding the session forever (#567). This
// slice is what makes the branch reachable in ordinary use: an MCP tool the
// model saw on an earlier turn can go unoffered on a later one (its server's
// listing failed, its name lost a contest, the request's definition budget
// ran out) and the model calls it anyway.
func TestAToolTheModelWasNotOfferedIsAnsweredUnknown(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{
		toolUseChunk("toolu_1", "mcp__docs__search"),
		done("tool_use", 3),
	}}, nil)
	// The server's listing failed, so nothing of its is offered — and the model
	// asks for one of its tools regardless.
	mcpAgent(t, h, `{"type":"mcp_toolset","mcp_server_name":"docs"}`)
	listing(t, h, "failed", `[]`)

	h.wake(t, "search the docs")
	h.runOnce(t)

	if n := len(h.eventsOfType(t, domain.EventAgentMCPToolUse)); n != 0 {
		t.Errorf("agent.mcp_tool_use events = %d, want 0 — the platform must not run what it did not offer", n)
	}
	if n := len(h.eventsOfType(t, domain.EventAgentCustomToolUse)); n != 0 {
		t.Errorf("agent.custom_tool_use events = %d, want 0 — no client declared this name (#567)", n)
	}
	got := h.answers(t)
	if len(got) != 1 || !got[0].isErr {
		t.Fatalf("answers = %v, want one is_error", got)
	}
	if !strings.Contains(got[0].text, `unknown tool "mcp__docs__search"`) {
		t.Errorf("answer text = %q, want it to name the unknown tool", got[0].text)
	}
	for _, k := range []queue.Kind{queue.MCPExec, queue.WebExec, queue.ToolExec} {
		if got := h.liveOf(t, k); got != 0 {
			t.Errorf("%s items = %d, want 0 — nothing was offered to run", k, got)
		}
	}
	if got := h.status(t); got != "running" {
		t.Errorf("status = %q, want running — nothing here waits on a client", got)
	}
	if n := h.liveOf(t, queue.ModelTurn); n != 1 {
		t.Errorf("model_turn items = %d, want the settlement's own chained turn", n)
	}
}

// A session.error sits on the stream for as long as the session does, and every
// string its message is built from belongs to somebody else — here a permission
// policy type as the agent spec spelled it, capped nowhere it is stored.
func TestAFailureMessageIsBounded(t *testing.T) {
	huge := strings.Repeat("p", 100_000)
	h := newHarness(t, [][]provider.Chunk{{textChunk(0, "unused"), done("end_turn", 3)}}, nil)
	mcpAgent(t, h, `{"type":"mcp_toolset","mcp_server_name":"docs",
		"default_config":{"permission_policy":{"type":"`+huge+`"}}}`)
	listing(t, h, "ready", searchTool)

	h.wake(t, "search the docs")
	h.runOnce(t)

	errs := h.eventsOfType(t, domain.EventSessionError)
	if len(errs) != 1 {
		t.Fatalf("session.error count = %d, want 1", len(errs))
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(errs[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Message == "" {
		t.Fatal("the error carries no message")
	}
	// Well under the policy type it quotes, and marked where it was cut.
	if len(body.Error.Message) > 16<<10 {
		t.Errorf("message is %d bytes, want it bounded", len(body.Error.Message))
	}
	if !strings.HasSuffix(body.Error.Message, "[truncated]") {
		t.Errorf("message = %q…, want it marked truncated", body.Error.Message[:64])
	}
}

// An agent with no MCP server at all never reads the catalog and never
// suspends: the whole path is inert for the sessions that do not use it.
func TestASessionWithoutMCPNeverWaits(t *testing.T) {
	h := newHarness(t, [][]provider.Chunk{{textChunk(0, "hello"), done("end_turn", 3)}}, nil)

	h.wake(t, "hi")
	h.runOnce(t)

	if n := len(h.provider.calls); n != 1 {
		t.Fatalf("model requests = %d, want 1", n)
	}
	if got := h.liveOf(t, queue.MCPExec); got != 0 {
		t.Errorf("mcp_exec items = %d, want 0", got)
	}
}
