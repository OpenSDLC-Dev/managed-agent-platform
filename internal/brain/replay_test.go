package brain

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

func ev(seq int64, typ domain.EventType, body string) domain.Event {
	return domain.Event{ID: domain.NewID("sevt"), Seq: seq, Type: typ, Body: []byte(body)}
}

func TestBuildRequestReplaysTheLog(t *testing.T) {
	agent := domain.ResolvedAgent{
		Type: "agent", ID: "agent_1", Version: 1, Name: "n",
		AgentSpec: domain.AgentSpec{
			Model:  domain.Model{ID: "m"},
			System: "base prompt",
			Tools: []json.RawMessage{
				json.RawMessage(`{"type":"custom","name":"lookup","description":"d","input_schema":{"type":"object"}}`),
				json.RawMessage(`{"type":"agent_toolset_20260401"}`),
				json.RawMessage(`{"type":"mcp_toolset","mcp_server_name":"srv"}`),
			},
		},
	}
	toolUse := ev(5, domain.EventAgentToolUse, `{"name":"bash","input":{"command":"ls"}}`)
	mcpUse := ev(7, domain.EventAgentMCPToolUse, `{"name":"search","mcp_server_name":"srv","input":{}}`)
	// Realistic order: tool_use → user text mid-tool → tool_result; the
	// result and the mid-tool text land in one user turn where the
	// tool_result must sort ahead of the earlier text.
	history := []domain.Event{
		ev(1, domain.EventUserMessage, `{"content":"plain string form"}`),
		ev(2, domain.EventSessionStatusRunning, `{}`),
		ev(3, domain.EventSystemMessage, `{"content":[{"type":"text","text":"mid-run steering"}]}`),
		ev(4, domain.EventAgentMessage, `{"content":[{"type":"text","text":"reply one"}]}`),
		toolUse,
		ev(6, domain.EventUserMessage, `{"content":[{"type":"text","text":"typed while tool ran"}]}`),
		{ID: "sevt_res", Seq: 7, Type: domain.EventUserToolResult,
			Body: []byte(`{"tool_use_id":"` + toolUse.ID.String() + `","content":[{"type":"text","text":"out"}],"is_error":false}`)},
		mcpUse,
		{ID: "sevt_mres", Seq: 9, Type: domain.EventAgentMCPToolResult,
			Body: []byte(`{"mcp_tool_use_id":"` + mcpUse.ID.String() + `"}`)},
		ev(10, domain.EventUserInterrupt, `{}`),
	}

	// The custom tool, the eight expanded agent_toolset tools and the one tool
	// the MCP server reported all reach the model, the agent's own first.
	tools, _, _, err := resolveTools(agent, mcpCatalog{"srv": listingOf(t, mcpTool("search"))})
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	want := []string{"lookup", "bash", "read", "write", "edit", "glob", "grep",
		"web_fetch", "web_search", "mcp__srv__search"}
	if got := defNames(t, tools); !slicesEqual(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}

	req, watermark, err := buildRequest(agent.System, tools, history, "", "", "")
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if watermark != 10 {
		t.Errorf("watermark = %d, want 10", watermark)
	}
	if req.System != "base prompt\n\nmid-run steering" {
		t.Errorf("system = %q", req.System)
	}
	if len(req.Tools) != len(tools) {
		t.Errorf("request carried %d tools, want the %d assembled", len(req.Tools), len(tools))
	}

	roles := make([]string, len(req.Messages))
	for i, m := range req.Messages {
		roles[i] = m.Role
	}
	wantRoles := []string{"user", "assistant", "user", "assistant", "user"}
	if strings.Join(roles, ",") != strings.Join(wantRoles, ",") {
		t.Fatalf("roles = %v, want %v", roles, wantRoles)
	}

	// String content normalized to a text block.
	var first []map[string]any
	_ = json.Unmarshal(req.Messages[0].Content, &first)
	if first[0]["type"] != "text" || first[0]["text"] != "plain string form" {
		t.Errorf("string content = %v", first)
	}
	// Assistant run merges text + tool_use, under the EVENT id.
	var second []map[string]any
	_ = json.Unmarshal(req.Messages[1].Content, &second)
	if len(second) != 2 || second[0]["text"] != "reply one" ||
		second[1]["type"] != "tool_use" || second[1]["id"] != toolUse.ID.String() || second[1]["name"] != "bash" {
		t.Errorf("assistant run = %v", second)
	}
	// The user run puts the tool_result first even though the text event
	// came earlier, and carries content + is_error through.
	var third []map[string]any
	_ = json.Unmarshal(req.Messages[2].Content, &third)
	if len(third) != 2 || third[0]["type"] != "tool_result" || third[0]["tool_use_id"] != toolUse.ID.String() {
		t.Fatalf("user run = %v", third)
	}
	if third[0]["is_error"] != false || third[1]["text"] != "typed while tool ran" {
		t.Errorf("user run detail = %v", third)
	}
	// The MCP call replays under the name the model was offered, put back
	// together from the two fields the wire event splits it into — a tool_use
	// block naming a tool the request does not offer is one the endpoint may
	// refuse, and this block replays on every later turn.
	var fourth []map[string]any
	_ = json.Unmarshal(req.Messages[3].Content, &fourth)
	if len(fourth) != 1 || fourth[0]["name"] != "mcp__srv__search" || fourth[0]["id"] != mcpUse.ID.String() {
		t.Errorf("mcp call = %v, want the prefixed name under the event id", fourth)
	}
	// MCP result: id via mcp_tool_use_id, no content key when absent.
	var fifth []map[string]any
	_ = json.Unmarshal(req.Messages[4].Content, &fifth)
	if fifth[0]["tool_use_id"] != mcpUse.ID.String() {
		t.Errorf("mcp result = %v", fifth)
	}
	if _, has := fifth[0]["content"]; has {
		t.Errorf("absent content rendered: %v", fifth[0])
	}
}

func TestBuildRequestCustomToolResultID(t *testing.T) {
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Model: domain.Model{ID: "m"}}}
	req, _, err := buildRequest(agent.System, nil, []domain.Event{
		ev(1, domain.EventAgentCustomToolUse, `{"name":"x","input":{}}`),
		ev(2, domain.EventUserCustomToolRes, `{"custom_tool_use_id":"sevt_abc","is_error":true}`),
	}, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var blocks []map[string]any
	_ = json.Unmarshal(req.Messages[1].Content, &blocks)
	if blocks[0]["tool_use_id"] != "sevt_abc" || blocks[0]["is_error"] != true {
		t.Errorf("custom result block = %v", blocks)
	}
}

func TestBuildRequestEmptyToolInputDefaults(t *testing.T) {
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Model: domain.Model{ID: "m"}}}
	// Absent and JSON-null inputs both replay as {} — a tool_use block's
	// input must be an object on the wire.
	for _, payload := range []string{`{"name":"noop"}`, `{"name":"noop","input":null}`} {
		req, _, err := buildRequest(agent.System, nil, []domain.Event{
			ev(1, domain.EventAgentToolUse, payload),
		}, "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		var blocks []map[string]any
		_ = json.Unmarshal(req.Messages[0].Content, &blocks)
		if input, ok := blocks[0]["input"].(map[string]any); !ok || len(input) != 0 {
			t.Errorf("payload %s: input should replay as {}: %v", payload, blocks[0])
		}
	}
}

func TestBuildRequestRejectsMalformedEvents(t *testing.T) {
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Model: domain.Model{ID: "m"}}}
	cases := []domain.Event{
		ev(1, domain.EventUserMessage, `{"content":42}`),
		ev(1, domain.EventUserMessage, `not json`),
		ev(1, domain.EventAgentMessage, `not json`),
		ev(1, domain.EventSystemMessage, `not json`),
		ev(1, domain.EventAgentToolUse, `not json`),
		ev(1, domain.EventUserToolResult, `not json`),
	}
	for _, bad := range cases {
		if _, _, err := buildRequest(agent.System, nil, []domain.Event{bad}, "", "", ""); err == nil {
			t.Errorf("%s with body %q accepted", bad.Type, bad.Body)
		}
	}

	// The skills block is placed after the agent system prompt and before any
	// runtime system.message text, joined with blank lines.
	skilled := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Model: domain.Model{ID: "m"}, System: "base"}}
	req, _, err := buildRequest(skilled.System, nil, []domain.Event{
		ev(1, domain.EventSystemMessage, `{"content":[{"type":"text","text":"steer"}]}`),
	}, "SKILLS", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "base\n\nSKILLS\n\nsteer" {
		t.Errorf("system with skills = %q, want %q", req.System, "base\n\nSKILLS\n\nsteer")
	}
	// With no agent system prompt the block leads, still before runtime text.
	bare := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Model: domain.Model{ID: "m"}}}
	req, _, err = buildRequest(bare.System, nil, nil, "SKILLS", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "SKILLS" {
		t.Errorf("system with only skills = %q", req.System)
	}
}

// TestBuildRequestFilesBlockPlacement: the Mounted-files block sits after the
// skills block and before any runtime system.message text, all blank-joined.
func TestBuildRequestFilesBlockPlacement(t *testing.T) {
	agent := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Model: domain.Model{ID: "m"}, System: "base"}}
	req, _, err := buildRequest(agent.System, nil, []domain.Event{
		ev(1, domain.EventSystemMessage, `{"content":[{"type":"text","text":"steer"}]}`),
	}, "SKILLS", "FILES", "")
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "base\n\nSKILLS\n\nFILES\n\nsteer" {
		t.Errorf("system = %q, want base/SKILLS/FILES/steer", req.System)
	}
	// The files block leads when there is no agent prompt and no skills block.
	bare := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Model: domain.Model{ID: "m"}}}
	req, _, err = buildRequest(bare.System, nil, nil, "", "FILES", "")
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "FILES" {
		t.Errorf("system with only files = %q", req.System)
	}
}

func TestBuildRequestRendersDefineOutcome(t *testing.T) {
	agent := domain.ResolvedAgent{
		Type: "agent", ID: "agent_1", Version: 1, Name: "n",
		AgentSpec: domain.AgentSpec{Model: domain.Model{ID: "m"}, System: "base"},
	}
	history := []domain.Event{
		ev(1, domain.EventUserDefineOutcome,
			`{"description":"Build a DCF model","rubric":{"type":"text","content":"# Rubric"},"max_iterations":3,"outcome_id":"outc_1"}`),
		ev(2, domain.EventSessionStatusRunning, `{}`),
	}
	req, watermark, err := buildRequest(agent.System, nil, history, "", "", "")
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if watermark != 2 {
		t.Errorf("watermark = %d, want 2", watermark)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v, want one user message", req.Messages)
	}
	text := string(req.Messages[0].Content)
	if !strings.Contains(text, "Build a DCF model") || !strings.Contains(text, "# Rubric") {
		t.Errorf("rendered outcome message = %s, want description and rubric text", text)
	}
	// The charge names the deliverables directory: nothing else in the
	// conversation does, and files written elsewhere never reach the harvest
	// (the live acceptance's satisfied-with-zero-deliverables run).
	if !strings.Contains(text, "/mnt/session/outputs/") {
		t.Errorf("rendered outcome message = %s, want the outputs directory named", text)
	}

	// A file rubric renders the description alone: its content reaches the
	// grader from the acceptance snapshot (slice 3), not the conversation.
	history[0] = ev(1, domain.EventUserDefineOutcome,
		`{"description":"Build it","rubric":{"type":"file","file_id":"file_1"},"max_iterations":3,"outcome_id":"outc_2"}`)
	req, _, err = buildRequest(agent.System, nil, history, "", "", "")
	if err != nil {
		t.Fatalf("buildRequest (file rubric): %v", err)
	}
	text = string(req.Messages[0].Content)
	if !strings.Contains(text, "Build it") || strings.Contains(text, "file_1") {
		t.Errorf("file-rubric rendering = %s, want description only", text)
	}
	if !strings.Contains(text, "/mnt/session/outputs/") {
		t.Errorf("file-rubric rendering = %s, want the outputs directory named", text)
	}
}

func TestDefineOutcomeChainsMidTurn(t *testing.T) {
	// The wake trigger only fires on idle sessions; a define_outcome landing
	// mid-turn must chain the next turn through the pending-input check.
	if !slices.Contains(pendingInputTypes, string(domain.EventUserDefineOutcome)) {
		t.Errorf("pendingInputTypes = %v, want user.define_outcome included", pendingInputTypes)
	}
}

func TestPendingInputChainsDefineOutcome(t *testing.T) {
	// The DB contract behind mid-turn chaining: an unprocessed
	// user.define_outcome past the watermark reports pending input; at or
	// before the watermark (or once processed) it does not.
	pool := pgtest.NewPool(t)
	sid, _ := pgtest.NewSession(t, pool, "cloud")
	log := events.NewLog(pool)
	appended, err := log.Append(context.Background(), sid, []events.NewEvent{{
		Type:    domain.EventUserDefineOutcome,
		Payload: []byte(`{"description":"d","rubric":{"type":"text","content":"r"},"max_iterations":3,"outcome_id":"outc_1"}`),
	}})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	seq := appended[0].Seq

	check := func(watermark int64, want bool) {
		t.Helper()
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(context.Background())
		got, err := pendingInput(context.Background(), tx, sid, watermark)
		if err != nil {
			t.Fatalf("pendingInput: %v", err)
		}
		if got != want {
			t.Errorf("pendingInput(watermark=%d) = %v, want %v", watermark, got, want)
		}
	}
	check(seq-1, true) // unprocessed define_outcome past the watermark chains
	check(seq, false)  // at the watermark: already consumed by this turn
}
