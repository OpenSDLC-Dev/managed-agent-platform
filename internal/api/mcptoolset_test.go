package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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

// wantRejected asserts a 400 whose message carries frag. The fragment is the
// point: these surfaces have many ways to answer 400, and a test that accepts
// any of them stays green when the rung it is meant to cover is removed.
func wantRejected(t *testing.T, status int, res map[string]any, frag string) {
	t.Helper()
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	inner, _ := res["error"].(map[string]any)
	if msg, _ := inner["message"].(string); !strings.Contains(msg, frag) {
		t.Errorf("error message %q does not mention %q", msg, frag)
	}
}

// TestSessionDanglingMCPToolsetRejected pins the same rung on the session
// surfaces, which resolve the agent spec through the identical validator, in
// both directions: a toolset naming a server the spec lacks, and the mirror
// image — clearing mcp_servers while the toolset that referenced them stays.
// The reference states both halves, and clearing one side of the pair was
// already a 400 in the unreferenced-server direction before this slice, so a
// client turning MCP off for one session replaces both sides or neither.
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
	wantRejected(t, status, res, "ghost")

	session := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	sid := session["id"].(string)
	status, res = s.do(http.MethodPost, "/v1/sessions/"+sid, map[string]any{
		"agent": map[string]any{"tools": []any{mcpToolset("ghost")}},
	})
	wantRejected(t, status, res, "ghost")

	// The other direction, on both session surfaces: an agent whose pair is
	// intact, overridden to drop the servers alone.
	paired := createAgent(t, s, agentBody(map[string]any{
		"mcp_servers": []any{mcpServer("srv")},
		"tools":       []any{mcpToolset("srv")},
	}))
	pairedID := paired["id"].(string)
	status, res = s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": map[string]any{
			"type": "agent_with_overrides", "id": pairedID, "mcp_servers": []any{},
		},
		"environment_id": envID,
	})
	wantRejected(t, status, res, "srv")

	paired2 := createSession(t, s, map[string]any{"agent": pairedID, "environment_id": envID})
	status, res = s.do(http.MethodPost, "/v1/sessions/"+paired2["id"].(string), map[string]any{
		"agent": map[string]any{"mcp_servers": []any{}},
	})
	wantRejected(t, status, res, "srv")

	// Replacing both sides at once is accepted, which is how a client moves an
	// MCP-configured session off one server and onto another.
	if status, res := s.do(http.MethodPost, "/v1/sessions/"+paired2["id"].(string), map[string]any{
		"agent": map[string]any{
			"mcp_servers": []any{mcpServer("other")},
			"tools":       []any{mcpToolset("other")},
		},
	}); status != http.StatusOK {
		t.Fatalf("replacing both sides on a session: status %d, body %v", status, res)
	}
}

// TestStoredSpecsAreRevalidatedOnResolve pins that both new rungs bind the
// bytes in the store, not merely the request that wrote them. Only a row
// written before this slice can be dangling or carry a misspelled nested key,
// and there is no way to produce one through the API any more — so the rows
// here are written straight to Postgres, which is also what makes the test
// meaningful: it is the pre-existing-data case, and it is the case the API
// surface cannot reach.
//
// The behavior is deliberate and matches how the agent caps already work (#66,
// "a pre-#66 stored spec that violates a cap fails here too"): an agent whose
// toolset cannot resolve fails loudly at session create rather than starting a
// session whose brain would have no server to expand. The agent resource still
// renders, so the message names what to repair.
func TestStoredSpecsAreRevalidatedOnResolve(t *testing.T) {
	s := newTestServer(t)
	envID := createEnvironment(t, s, map[string]any{"name": "e"})["id"].(string)

	for _, tc := range []struct {
		name           string
		servers, tools string
		frag           string
	}{{
		name:    "a dangling toolset",
		servers: `[]`,
		tools:   `[{"type":"mcp_toolset","mcp_server_name":"ghost"}]`,
		frag:    "ghost",
	}, {
		name:    "a misspelled key inside a stored configs entry",
		servers: `[{"type":"url","name":"srv","url":"https://mcp.example/srv"}]`,
		tools: `[{"type":"mcp_toolset","mcp_server_name":"srv","configs":` +
			`[{"name":"t","permission_polciy":{"type":"always_ask"}}]}]`,
		frag: "permission_polciy",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			agent := createAgent(t, s, agentBody(map[string]any{
				"mcp_servers": []any{mcpServer("srv")},
				"tools":       []any{mcpToolset("srv")},
			}))
			id := agent["id"].(string)
			if _, err := s.pool.Exec(context.Background(),
				`UPDATE agents SET spec = jsonb_set(jsonb_set(spec, '{tools}', $2::jsonb),
				 '{mcp_servers}', $3::jsonb) WHERE id = $1`, id, tc.tools, tc.servers); err != nil {
				t.Fatalf("seed stored spec: %v", err)
			}
			status, res := s.do(http.MethodPost, "/v1/sessions",
				map[string]any{"agent": id, "environment_id": envID})
			wantRejected(t, status, res, tc.frag)
		})
	}
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

// TestResolvedEchoDoesNotReachTheStore reads the stored bytes directly, which
// is the only way to tell render-time resolution from write-time resolution:
// every HTTP surface renders, so both would look identical through the API.
// What the store holds decides what an update merges against and what a stored
// spec means, so it is asserted rather than assumed.
func TestResolvedEchoDoesNotReachTheStore(t *testing.T) {
	s := newTestServer(t)
	bare := map[string]any{"type": "agent_toolset_20260401"}
	agent := createAgent(t, s, agentBody(map[string]any{"tools": []any{bare}}))
	id := agent["id"].(string)

	env := createEnvironment(t, s, map[string]any{"name": "e"})
	session := createSession(t, s, map[string]any{"agent": id, "environment_id": env["id"].(string)})

	ctx := context.Background()
	for _, q := range []struct {
		what, sql string
		arg       any
	}{
		{"agents.spec", `SELECT spec->'tools' FROM agents WHERE id = $1`, id},
		{"agent_versions.spec", `SELECT spec->'tools' FROM agent_versions WHERE agent_id = $1 AND version = 1`, id},
		{"sessions.resolved_agent", `SELECT resolved_agent->'tools' FROM sessions WHERE id = $1`, session["id"].(string)},
	} {
		var stored []byte
		if err := s.pool.QueryRow(ctx, q.sql, q.arg).Scan(&stored); err != nil {
			t.Fatalf("%s: %v", q.what, err)
		}
		var tools []map[string]any
		if err := json.Unmarshal(stored, &tools); err != nil {
			t.Fatalf("%s: decode %s: %v", q.what, stored, err)
		}
		if len(tools) != 1 {
			t.Fatalf("%s: %d tools, want 1", q.what, len(tools))
		}
		if _, ok := tools[0]["default_config"]; ok {
			t.Errorf("%s holds a resolved entry, so materialization reached the store: %s", q.what, stored)
		}
		if _, ok := tools[0]["configs"]; ok {
			t.Errorf("%s holds a resolved entry, so materialization reached the store: %s", q.what, stored)
		}
	}
}

// TestSessionUpdatedEventCarriesResolvedAgent pins the one surface that ships a
// resolved-agent snapshot without going through renderSession. The reference
// types the event's `agent` as the same resolved session-agent object the
// session response carries, so a sparse agent there would answer one update in
// two different shapes.
func TestSessionUpdatedEventCarriesResolvedAgent(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	session := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	sid := session["id"].(string)

	status, res := s.do(http.MethodPost, "/v1/sessions/"+sid, map[string]any{
		"agent": map[string]any{"tools": []any{map[string]any{"type": "agent_toolset_20260401"}}},
	})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, res)
	}

	_, listed := s.do(http.MethodGet, "/v1/sessions/"+sid+"/events", nil)
	var updated map[string]any
	for _, ev := range listData(t, listed) {
		if ev["type"] == "session.updated" {
			updated = ev
		}
	}
	if updated == nil {
		t.Fatalf("no session.updated event in %v", listed)
	}
	eventAgent, ok := updated["agent"].(map[string]any)
	if !ok {
		t.Fatalf("session.updated carries no agent: %v", updated)
	}
	tools, _ := eventAgent["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("event agent tools = %v, want one entry", eventAgent["tools"])
	}
	entry, _ := tools[0].(map[string]any)
	wantConfig(t, "session.updated agent default_config", entry["default_config"], true, "always_allow")
}

// TestSessionUpdatedEventNormalizesAbsentTools covers the other half of "one
// update, one shape": renderSession turns an absent tools list into [] before
// resolving it, so the event has to as well, on a field the reference marks
// required. Only a snapshot written outside the current create path can hold a
// JSON null there, so the row is seeded directly.
func TestSessionUpdatedEventNormalizesAbsentTools(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	session := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	sid := session["id"].(string)
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE sessions SET resolved_agent = jsonb_set(
		   jsonb_set(resolved_agent, '{tools}', 'null'::jsonb),
		   '{mcp_servers}', $2::jsonb) WHERE id = $1`, sid,
		`[{"type":"url","name":"srv","url":"https://mcp.example/srv"}]`); err != nil {
		t.Fatalf("seed null tools: %v", err)
	}

	// Clear the servers — the only other mutable agent field — so the update
	// emits the snapshot without rewriting the tools list the seed just
	// cleared. Nothing referenced them, which is why they can go.
	status, res := s.do(http.MethodPost, "/v1/sessions/"+sid, map[string]any{
		"agent": map[string]any{"mcp_servers": []any{}},
	})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, res)
	}
	if agent, _ := res["agent"].(map[string]any); agent["tools"] == nil {
		t.Fatalf("response agent tools = null, want []: %v", res["agent"])
	}

	_, listed := s.do(http.MethodGet, "/v1/sessions/"+sid+"/events", nil)
	var updated map[string]any
	for _, ev := range listData(t, listed) {
		if ev["type"] == "session.updated" {
			updated = ev
		}
	}
	// Asserted rather than looped over: a change that stopped emitting the
	// event would satisfy every check below by never running one.
	if updated == nil {
		t.Fatalf("no session.updated event in %v", listed)
	}
	agent, ok := updated["agent"].(map[string]any)
	if !ok {
		t.Fatalf("session.updated carries no agent: %v", updated)
	}
	if _, ok := agent["tools"].([]any); !ok {
		t.Fatalf("session.updated agent tools = %v, want an array — the same "+
			"update answered %v over HTTP", agent["tools"], res["agent"].(map[string]any)["tools"])
	}
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

// TestSessionAgentPatchInvalidatesTheMCPCatalog pins the catalog's invalidation
// (plan 29 slice 2). mcp_servers is one of only two mid-session-mutable agent
// fields and a patch replaces the array whole, so a server can be removed or
// repointed under a catalog that still holds the tools its old endpoint
// reported. Those rows die with the patch, in its own transaction: a listing
// that outlived its endpoint would reach the model as tools that are not there.
// A server the patch leaves alone keeps its row — re-discovering every server
// because one moved would spend a round trip per turn on servers nothing
// changed about.
func TestSessionAgentPatchInvalidatesTheMCPCatalog(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	// "alias" is a second name for "keep"'s endpoint. Two names may share one
	// URL — nothing forbids it, and a `configs[]` entry addresses the name —
	// so the pair is what forces the match to compare both: were it the url
	// alone, dropping "alias" would leave its row standing behind the URL
	// "keep" still declares, and the model would be offered a server the agent
	// no longer has.
	alias := map[string]any{"type": "url", "name": "alias", "url": "https://mcp.example/keep"}
	created := createSession(t, s, map[string]any{
		"agent": map[string]any{"type": "agent_with_overrides", "id": agentID,
			"mcp_servers": []any{mcpServer("keep"), mcpServer("move"), mcpServer("drop"), alias},
			"tools": []any{mcpToolset("keep"), mcpToolset("move"), mcpToolset("drop"),
				mcpToolset("alias")}},
		"environment_id": envID,
	})
	sid := created["id"].(string)

	for _, row := range [][2]string{
		{"keep", "https://mcp.example/keep"},
		{"move", "https://mcp.example/move"},
		{"drop", "https://mcp.example/drop"},
		{"alias", "https://mcp.example/keep"},
	} {
		if _, err := s.pool.Exec(context.Background(),
			`INSERT INTO mcp_catalogs (session_id, server_name, url, tools, status)
			 VALUES ($1, $2, $3, '[]'::jsonb, 'ready')`,
			sid, row[0], row[1]); err != nil {
			t.Fatalf("seed catalog row %q: %v", row[0], err)
		}
	}

	// "keep" is unchanged, "move" is repointed, "drop" and "alias" are gone.
	// The tools move with them, since a dangling toolset would reject the patch
	// outright.
	status, res := s.do(http.MethodPost, "/v1/sessions/"+sid, map[string]any{
		"agent": map[string]any{
			"mcp_servers": []any{
				mcpServer("keep"),
				map[string]any{"type": "url", "name": "move", "url": "https://elsewhere.example/move"},
			},
			"tools": []any{mcpToolset("keep"), mcpToolset("move")},
		}})
	if status != http.StatusOK {
		t.Fatalf("patch: status %d (body %v)", status, res)
	}

	if left := catalogRows(t, s, sid); len(left) != 1 || left[0] != "keep" {
		t.Errorf("catalog rows after the patch = %v, want only the untouched server", left)
	}
}

// TestSessionAgentPatchClearingMCPServersEmptiesTheCatalog is the boundary of
// that invalidation, and the one the SQL is easiest to get wrong at: the
// surviving set is passed as two arrays, and an empty one has to delete every
// row rather than none. A patch that drops the last server leaves a catalog
// whose every entry names an endpoint the session no longer declares.
func TestSessionAgentPatchClearingMCPServersEmptiesTheCatalog(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	created := createSession(t, s, map[string]any{
		"agent": map[string]any{"type": "agent_with_overrides", "id": agentID,
			"mcp_servers": []any{mcpServer("one"), mcpServer("two")},
			"tools":       []any{mcpToolset("one"), mcpToolset("two")}},
		"environment_id": envID,
	})
	sid := created["id"].(string)

	for _, name := range []string{"one", "two"} {
		if _, err := s.pool.Exec(context.Background(),
			`INSERT INTO mcp_catalogs (session_id, server_name, url, tools, status)
			 VALUES ($1, $2, $3, '[]'::jsonb, 'ready')`,
			sid, name, "https://mcp.example/"+name); err != nil {
			t.Fatalf("seed catalog row %q: %v", name, err)
		}
	}

	// The toolsets go with the servers; a toolset naming a server the patch
	// removed would be dangling and the patch would be rejected before it
	// reached the catalog.
	status, res := s.do(http.MethodPost, "/v1/sessions/"+sid, map[string]any{
		"agent": map[string]any{"mcp_servers": []any{}, "tools": []any{}}})
	if status != http.StatusOK {
		t.Fatalf("patch: status %d (body %v)", status, res)
	}

	if left := catalogRows(t, s, sid); len(left) != 0 {
		t.Errorf("catalog rows after clearing mcp_servers = %v, want none", left)
	}
}

// TestSessionAgentPatchLeavesOtherSessionsCatalogsAlone pins the scoping of a
// statement that is destructive and cross-session by construction: one patch on
// one session runs a DELETE over a table every live session has rows in. Widen
// its predicate — drop the session_id, or move it inside the NOT EXISTS — and
// every other session in the deployment silently loses its catalog and re-dials
// all of its MCP servers on its next turn, which no single-session fixture can
// see.
func TestSessionAgentPatchLeavesOtherSessionsCatalogsAlone(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	newSession := func(names ...string) string {
		t.Helper()
		servers, tools := []any{}, []any{}
		for _, n := range names {
			servers, tools = append(servers, mcpServer(n)), append(tools, mcpToolset(n))
		}
		created := createSession(t, s, map[string]any{
			"agent": map[string]any{"type": "agent_with_overrides", "id": agentID,
				"mcp_servers": servers, "tools": tools},
			"environment_id": envID,
		})
		sid := created["id"].(string)
		for _, n := range names {
			if _, err := s.pool.Exec(context.Background(),
				`INSERT INTO mcp_catalogs (session_id, server_name, url, tools, status)
				 VALUES ($1, $2, $3, '[]'::jsonb, 'ready')`,
				sid, n, "https://mcp.example/"+n); err != nil {
				t.Fatalf("seed catalog row %q: %v", n, err)
			}
		}
		return sid
	}
	patched, bystander := newSession("keep", "drop"), newSession("keep", "drop")

	// The patch removes "drop" from the first session only. Both sessions carry
	// rows under the same two names, so a DELETE that lost its session scoping
	// would take the bystander's "drop" with it.
	status, res := s.do(http.MethodPost, "/v1/sessions/"+patched, map[string]any{
		"agent": map[string]any{
			"mcp_servers": []any{mcpServer("keep")},
			"tools":       []any{mcpToolset("keep")},
		}})
	if status != http.StatusOK {
		t.Fatalf("patch: status %d (body %v)", status, res)
	}

	if left := catalogRows(t, s, patched); len(left) != 1 || left[0] != "keep" {
		t.Errorf("patched session's rows = %v, want only the server it still declares", left)
	}
	if left := catalogRows(t, s, bystander); len(left) != 2 {
		t.Errorf("another session's rows = %v, want both untouched by a patch on a different session", left)
	}
}

// A failed listing answers the work cycle it was made in, not the life of the
// session. The brain runs its turn without that server rather than suspending to
// re-dial an endpoint that just refused — and discovery runs only when a turn
// suspends, so nothing would ever try again. The rows are dropped where the
// reference retries them (0023_mcp_catalogs.sql: the status_idle →
// status_running transition), which puts those servers back in the state a turn
// suspends for. A ready row is left alone: re-listing servers nothing changed
// about would spend a round trip per message.
func TestWakingASessionRetriesItsFailedMCPListings(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	created := createSession(t, s, map[string]any{
		"agent": map[string]any{"type": "agent_with_overrides", "id": agentID,
			"mcp_servers": []any{mcpServer("up"), mcpServer("down")},
			"tools":       []any{mcpToolset("up"), mcpToolset("down")}},
		"environment_id": envID,
	})
	sid := created["id"].(string)

	for _, row := range [][3]string{
		{"up", "https://mcp.example/up", "ready"},
		{"down", "https://mcp.example/down", "failed"},
	} {
		if _, err := s.pool.Exec(context.Background(),
			`INSERT INTO mcp_catalogs (session_id, server_name, url, tools, status)
			 VALUES ($1, $2, $3, '[]'::jsonb, $4)`, sid, row[0], row[1], row[2]); err != nil {
			t.Fatalf("seed catalog row %q: %v", row[0], err)
		}
	}

	sendEvents(t, s, sid, map[string]any{"type": "user.message",
		"content": []any{map[string]any{"type": "text", "text": "try again"}}})

	if left := catalogRows(t, s, sid); len(left) != 1 || left[0] != "up" {
		t.Errorf("catalog rows after the wake = %v, want only the server that answered", left)
	}
}

// catalogRows reports the session's catalog server names in name order.
func catalogRows(t *testing.T, s *tserver, sid string) []string {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT server_name FROM mcp_catalogs WHERE session_id = $1 ORDER BY server_name`, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var left []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		left = append(left, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return left
}
