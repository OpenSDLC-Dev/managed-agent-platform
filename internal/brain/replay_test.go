package brain

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
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
	mcpUse := ev(7, domain.EventAgentMCPToolUse, `{"name":"search","input":{}}`)
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

	req, watermark, err := buildRequest(agent, history, "")
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if watermark != 10 {
		t.Errorf("watermark = %d, want 10", watermark)
	}
	if req.System != "base prompt\n\nmid-run steering" {
		t.Errorf("system = %q", req.System)
	}
	// The custom tool and the six expanded agent_toolset tools reach the model,
	// in order; mcp_toolset still waits for the MCP client.
	if len(req.Tools) != 7 || !strings.Contains(string(req.Tools[0]), `"lookup"`) {
		t.Fatalf("tools = %v", req.Tools)
	}
	var names []string
	for _, raw := range req.Tools[1:] {
		var d struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &d)
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "bash,read,write,edit,glob,grep" {
		t.Errorf("expanded toolset names = %v", names)
	}

	roles := make([]string, len(req.Messages))
	for i, m := range req.Messages {
		roles[i] = m.Role
	}
	want := []string{"user", "assistant", "user", "assistant", "user"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("roles = %v, want %v", roles, want)
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
	req, _, err := buildRequest(agent, []domain.Event{
		ev(1, domain.EventAgentCustomToolUse, `{"name":"x","input":{}}`),
		ev(2, domain.EventUserCustomToolRes, `{"custom_tool_use_id":"sevt_abc","is_error":true}`),
	}, "")
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
		req, _, err := buildRequest(agent, []domain.Event{
			ev(1, domain.EventAgentToolUse, payload),
		}, "")
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
		if _, _, err := buildRequest(agent, []domain.Event{bad}, ""); err == nil {
			t.Errorf("%s with body %q accepted", bad.Type, bad.Body)
		}
	}

	badTool := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{
		Model: domain.Model{ID: "m"},
		Tools: []json.RawMessage{json.RawMessage(`"not an object"`)},
	}}
	if _, _, err := buildRequest(badTool, nil, ""); err == nil {
		t.Error("malformed agent tool accepted")
	}

	// The skills block is placed after the agent system prompt and before any
	// runtime system.message text, joined with blank lines.
	skilled := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Model: domain.Model{ID: "m"}, System: "base"}}
	req, _, err := buildRequest(skilled, []domain.Event{
		ev(1, domain.EventSystemMessage, `{"content":[{"type":"text","text":"steer"}]}`),
	}, "SKILLS")
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "base\n\nSKILLS\n\nsteer" {
		t.Errorf("system with skills = %q, want %q", req.System, "base\n\nSKILLS\n\nsteer")
	}
	// With no agent system prompt the block leads, still before runtime text.
	bare := domain.ResolvedAgent{AgentSpec: domain.AgentSpec{Model: domain.Model{ID: "m"}}}
	req, _, err = buildRequest(bare, nil, "SKILLS")
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "SKILLS" {
		t.Errorf("system with only skills = %q", req.System)
	}
}
