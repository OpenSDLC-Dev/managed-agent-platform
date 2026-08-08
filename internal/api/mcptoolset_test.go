package api_test

import (
	"net/http"
	"testing"
)

// Plan 29 slice 1 — the wire rungs an MCP-configured agent needs before any MCP
// client exists.
//
// The reference pins the referential integrity in both directions: "Every
// `mcp_servers` entry must be referenced by an `mcp_toolset` in the `tools`
// array, and every `mcp_toolset` must reference a declared server. The API
// rejects agent definitions with unreferenced servers or dangling toolsets"
// (platform.claude.com managed-agents/mcp-connector, fetched 2026-08-08). Only
// the first half was enforced — the SDK's param docs mention only that
// direction, and #66 recorded the omission as deliberate on that basis. The
// public docs settle it, so the dangling direction is a 400 too.

// TestAgentDanglingMCPToolsetRejected covers the direction #66 left open, on
// every surface that resolves a whole agent spec: create, update's merged
// result, a session's agent_with_overrides, and a session-update agent patch.
func TestAgentDanglingMCPToolsetRejected(t *testing.T) {
	s := newTestServer(t)

	// Create: a toolset naming a server that was never declared.
	wantAgentRejected(t, s, agentBody(map[string]any{
		"tools": []any{mcpToolset("ghost")},
	}), "ghost")

	// Create: servers exist, but one toolset names a different one — the
	// unreferenced-server rung would have caught the reverse, this one would
	// have passed.
	wantAgentRejected(t, s, agentBody(map[string]any{
		"mcp_servers": []any{mcpServer("srv")},
		"tools":       []any{mcpToolset("srv"), mcpToolset("ghost")},
	}), "ghost")

	// Update: the merged result is what binds, so clearing the servers while
	// keeping the toolset strands it.
	agent := createAgent(t, s, agentBody(map[string]any{
		"mcp_servers": []any{mcpServer("srv")},
		"tools":       []any{mcpToolset("srv")},
	}))
	id := agent["id"].(string)
	status, res := s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"mcp_servers": []any{}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// The pair moves together, so replacing both at once is accepted.
	if status, res := s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{
		"mcp_servers": []any{mcpServer("other")},
		"tools":       []any{mcpToolset("other")},
	}); status != http.StatusOK {
		t.Fatalf("replacing both sides: status %d, body %v", status, res)
	}
}

// TestSessionDanglingMCPToolsetRejected pins the same rung on the session
// surfaces, which resolve the agent spec through the identical validator.
func TestSessionDanglingMCPToolsetRejected(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	status, res := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": map[string]any{
			"type": "agent_with_overrides", "id": agentID,
			"tools": []any{mcpToolset("ghost")},
		},
		"environment_id": envID,
	})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	session := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	sid := session["id"].(string)
	status, res = s.do(http.MethodPost, "/v1/sessions/"+sid, map[string]any{
		"agent": map[string]any{"tools": []any{mcpToolset("ghost")}},
	})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

// TestMCPToolsetShapeValidated gives the MCP arm the create-time shape check
// the agent toolset has had since #26: parseTools checked only that
// mcp_server_name was non-empty, so a misspelled permission_policy key was
// dropped by encoding/json and the tool silently resolved to the toolset
// default — a fail-open at the human-in-the-loop boundary, on the toolset kind
// whose whole point is that it defaults to always_ask.
func TestMCPToolsetShapeValidated(t *testing.T) {
	s := newTestServer(t)
	servers := []any{mcpServer("srv")}

	for _, tc := range []struct {
		name    string
		toolset map[string]any
		frag    string
	}{{
		name: "misspelled permission_policy in a configs entry",
		toolset: map[string]any{"type": "mcp_toolset", "mcp_server_name": "srv",
			"configs": []any{map[string]any{"name": "t",
				"permission_polciy": map[string]any{"type": "always_ask"}}}},
		frag: "permission_polciy",
	}, {
		name: "unknown key on the toolset object",
		toolset: map[string]any{"type": "mcp_toolset", "mcp_server_name": "srv",
			"allowed_tools": []any{"t"}},
		frag: "allowed_tools",
	}, {
		// defer_loading is real on the Messages-API connector's config and
		// absent from the managed-agents one: the two schemas must not be
		// conflated.
		name: "a Messages-API-only key inside default_config",
		toolset: map[string]any{"type": "mcp_toolset", "mcp_server_name": "srv",
			"default_config": map[string]any{"defer_loading": true}},
		frag: "defer_loading",
	}, {
		name: "a policy type this platform cannot evaluate",
		toolset: map[string]any{"type": "mcp_toolset", "mcp_server_name": "srv",
			"default_config": map[string]any{"permission_policy": map[string]any{"type": "always_deny"}}},
		frag: "always_deny",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			wantAgentRejected(t, s, agentBody(map[string]any{
				"mcp_servers": servers, "tools": []any{tc.toolset},
			}), tc.frag)
		})
	}

	// A well-formed entry with every knob set is accepted.
	createAgent(t, s, agentBody(map[string]any{
		"mcp_servers": servers,
		"tools": []any{map[string]any{"type": "mcp_toolset", "mcp_server_name": "srv",
			"default_config": map[string]any{"enabled": false,
				"permission_policy": map[string]any{"type": "always_ask"}},
			"configs": []any{map[string]any{"name": "get_issue", "enabled": true,
				"permission_policy": map[string]any{"type": "always_allow"}}}}},
	}))
}

// TestToolsetConfigsEchoResolved pins the response shape the reference's types
// require of both toolset kinds: configs and default_config are always present
// and every enabled / permission_policy inside them carries a concrete value.
// The kinds differ only in the policy an omission resolves to.
func TestToolsetConfigsEchoResolved(t *testing.T) {
	s := newTestServer(t)
	agent := createAgent(t, s, agentBody(map[string]any{
		"mcp_servers": []any{mcpServer("srv")},
		"tools": []any{
			map[string]any{"type": "agent_toolset_20260401"},
			map[string]any{"type": "mcp_toolset", "mcp_server_name": "srv",
				"configs": []any{map[string]any{"name": "get_issue"}}},
			customTool("mine"),
		},
	}))

	assertResolved := func(t *testing.T, where string, tools []any) {
		t.Helper()
		if len(tools) != 3 {
			t.Fatalf("%s: %d tools, want 3", where, len(tools))
		}
		builtin, _ := tools[0].(map[string]any)
		wantConfig(t, where+" agent_toolset default_config", builtin["default_config"], true, "always_allow")
		if configs, ok := builtin["configs"].([]any); !ok || len(configs) != 0 {
			t.Errorf("%s: agent_toolset configs = %v, want []", where, builtin["configs"])
		}

		mcp, _ := tools[1].(map[string]any)
		wantConfig(t, where+" mcp_toolset default_config", mcp["default_config"], true, "always_ask")
		configs, _ := mcp["configs"].([]any)
		if len(configs) != 1 {
			t.Fatalf("%s: mcp_toolset configs = %v, want one entry", where, mcp["configs"])
		}
		entry, _ := configs[0].(map[string]any)
		if entry["name"] != "get_issue" {
			t.Errorf("%s: configs[0].name = %v, want get_issue", where, entry["name"])
		}
		wantConfig(t, where+" mcp_toolset configs[0]", entry, true, "always_ask")

		// A custom tool is not a toolset: nothing is added to it.
		if custom, _ := tools[2].(map[string]any); custom["default_config"] != nil || custom["configs"] != nil {
			t.Errorf("%s: custom tool grew toolset config: %v", where, custom)
		}
	}

	assertResolved(t, "create", agent["tools"].([]any))

	_, got := s.do(http.MethodGet, "/v1/agents/"+agent["id"].(string), nil)
	assertResolved(t, "get", got["tools"].([]any))

	// The session's resolved-agent snapshot echoes through the same rule.
	env := createEnvironment(t, s, map[string]any{"name": "e"})
	session := createSession(t, s, map[string]any{
		"agent": agent["id"].(string), "environment_id": env["id"].(string)})
	sessionAgent, _ := session["agent"].(map[string]any)
	assertResolved(t, "session create", sessionAgent["tools"].([]any))

	_, gotSession := s.do(http.MethodGet, "/v1/sessions/"+session["id"].(string), nil)
	sessionAgent, _ = gotSession["agent"].(map[string]any)
	assertResolved(t, "session get", sessionAgent["tools"].([]any))
}

// toolTypes lists the `type` of each entry of an echoed tools[], for the tests
// whose subject is which tools an override or patch produced rather than how
// each one renders.
func toolTypes(t *testing.T, tools any) []string {
	t.Helper()
	entries, ok := tools.([]any)
	if !ok {
		t.Fatalf("tools = %v, want an array", tools)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		obj, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("tools entry = %v, want an object", e)
		}
		typ, _ := obj["type"].(string)
		out = append(out, typ)
	}
	return out
}

// wantConfig asserts one resolved config object carries the expected enabled
// flag and permission policy.
func wantConfig(t *testing.T, where string, got any, enabled bool, policy string) {
	t.Helper()
	obj, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("%s: %v is not an object", where, got)
	}
	if obj["enabled"] != enabled {
		t.Errorf("%s: enabled = %v, want %v", where, obj["enabled"], enabled)
	}
	pp, ok := obj["permission_policy"].(map[string]any)
	if !ok {
		t.Fatalf("%s: permission_policy = %v, want an object", where, obj["permission_policy"])
	}
	if pp["type"] != policy {
		t.Errorf("%s: permission_policy.type = %v, want %v", where, pp["type"], policy)
	}
}

// TestAgentVersionEchoResolved pins the remaining agent read surfaces — the
// version snapshot and the list — on the same rule, since every one of them
// renders through the same funnel and a client reading any of them must see the
// same resolved shape.
func TestAgentVersionEchoResolved(t *testing.T) {
	s := newTestServer(t)
	agent := createAgent(t, s, agentBody(map[string]any{
		"tools": []any{map[string]any{"type": "agent_toolset_20260401"}},
	}))
	id := agent["id"].(string)

	_, v1 := s.do(http.MethodGet, "/v1/agents/"+id+"?version=1", nil)
	tools, _ := v1["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("version 1 tools = %v, want one entry", v1["tools"])
	}
	entry, _ := tools[0].(map[string]any)
	wantConfig(t, "version 1 default_config", entry["default_config"], true, "always_allow")

	_, list := s.do(http.MethodGet, "/v1/agents", nil)
	entries := listData(t, list)
	if len(entries) != 1 {
		t.Fatalf("agents = %d, want 1", len(entries))
	}
	listed, _ := entries[0]["tools"].([]any)
	if len(listed) != 1 {
		t.Fatalf("listed tools = %v, want one entry", entries[0]["tools"])
	}
	listedEntry, _ := listed[0].(map[string]any)
	wantConfig(t, "list default_config", listedEntry["default_config"], true, "always_allow")
}
