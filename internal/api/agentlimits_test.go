package api_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The reference documents these caps on the agent create/update params (checked
// against anthropic-sdk-go v1.70.1 — betaagent.go BetaAgentNewParams and
// BetaAgentUpdateParams): at most 128 tools and 20 mcp_servers, server names
// unique and every server referenced by an mcp_toolset in the resulting tools,
// metadata at most 16 pairs with 64-char keys and 512-char values. Unenforced,
// each produced an agent the platform stores but the provider rejects on every
// turn — a 400 at create, not a run-time surprise (#66).

func customTool(name string) map[string]any {
	return map[string]any{"type": "custom", "name": name, "description": "d",
		"input_schema": map[string]any{"type": "object"}}
}

func mcpServer(name string) map[string]any {
	return map[string]any{"type": "url", "name": name, "url": "https://mcp.example/" + name}
}

func mcpToolset(server string) map[string]any {
	return map[string]any{"type": "mcp_toolset", "mcp_server_name": server}
}

func agentBody(extra map[string]any) map[string]any {
	body := map[string]any{"name": "a", "model": "claude-opus-4-8"}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// wantAgentRejected asserts a create body 400s with a message carrying frag.
func wantAgentRejected(t *testing.T, s *tserver, body map[string]any, frag string) {
	t.Helper()
	status, res := s.do(http.MethodPost, "/v1/agents", body)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	inner, _ := res["error"].(map[string]any)
	if msg, _ := inner["message"].(string); !strings.Contains(msg, frag) {
		t.Errorf("error message %q does not mention %q", msg, frag)
	}
}

func TestAgentToolCap(t *testing.T) {
	s := newTestServer(t)
	tools := make([]any, 0, 129)
	for i := 0; i < 129; i++ {
		tools = append(tools, customTool(fmt.Sprintf("t%03d", i)))
	}
	wantAgentRejected(t, s, agentBody(map[string]any{"tools": tools}), "128")

	// The rejected agent was never stored.
	_, list := s.do(http.MethodGet, "/v1/agents", nil)
	if entries := listData(t, list); len(entries) != 0 {
		t.Fatalf("agents after rejected create = %d, want 0", len(entries))
	}
	// The cap is a boundary, not a fence short of it.
	createAgent(t, s, agentBody(map[string]any{"tools": tools[:128]}))
}

func TestAgentMCPServerCap(t *testing.T) {
	s := newTestServer(t)
	servers := make([]any, 0, 21)
	tools := make([]any, 0, 21)
	for i := 0; i < 21; i++ {
		name := fmt.Sprintf("srv%02d", i)
		servers = append(servers, mcpServer(name))
		tools = append(tools, mcpToolset(name))
	}
	wantAgentRejected(t, s,
		agentBody(map[string]any{"mcp_servers": servers, "tools": tools}), "20")
	createAgent(t, s,
		agentBody(map[string]any{"mcp_servers": servers[:20], "tools": tools[:20]}))
}

// An agent's own system prompt is bounded at 100,000 characters on create and
// update alike — the spec's maxLength, which the generated Go doc comments
// drop (checked against anthropic-sdk-go v1.70.1 — spec
// components.schemas.BetaManagedAgentsCreateAgentParams.properties.system) —
// and the reference refuses one past it with 400 invalid_request_error (#665).
func TestAgentSystemCap(t *testing.T) {
	s := newTestServer(t)
	wantAgentRejected(t, s, agentBody(map[string]any{"system": strings.Repeat("a", 100_001)}), "100000")
	createAgent(t, s, agentBody(map[string]any{"system": strings.Repeat("a", 100_000)}))

	// The unit is code points, which is what the 2026-09-02 recording measured:
	// it accepted 100,000 "é" — 200,000 UTF-8 bytes — on agent create, and
	// 50,001 astral characters — 100,002 UTF-16 code units — on the session
	// override. One "é" past the bound rejects.
	createAgent(t, s, agentBody(map[string]any{"system": strings.Repeat("é", 100_000)}))
	wantAgentRejected(t, s, agentBody(map[string]any{"system": strings.Repeat("é", 100_001)}), "100000")
	createAgent(t, s, agentBody(map[string]any{"system": strings.Repeat("😀", 50_001)}))
}

// Update is held to the same bound on the merged spec, like every other agent
// cap: a replacement past it rejects without a version bump. An agent stored
// over it before #665 — planted here around the API — refuses every update
// that leaves its system in place, and the update that brings the system back
// under the bound is the way out.
func TestAgentUpdateSystemCap(t *testing.T) {
	s := newTestServer(t)
	id := createAgent(t, s, agentBody(nil))["id"].(string)
	update := func(body map[string]any) (int, map[string]any) {
		return s.do(http.MethodPost, "/v1/agents/"+id, body)
	}

	status, body := update(map[string]any{"system": strings.Repeat("a", 100_001)})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	if msg := errMessage(body); !strings.Contains(msg, "100000") {
		t.Errorf("error message %q does not name the limit", msg)
	}
	if _, got := s.do(http.MethodGet, "/v1/agents/"+id, nil); got["version"] != float64(1) {
		t.Errorf("version after rejected update = %v, want 1", got["version"])
	}
	if status, body := update(map[string]any{"system": strings.Repeat("a", 100_000)}); status != http.StatusOK {
		t.Fatalf("update at the cap: status %d (body %v)", status, body)
	}

	if _, err := s.pool.Exec(context.Background(),
		`UPDATE agents SET spec = jsonb_set(spec, '{system}', to_jsonb($2::text)) WHERE id = $1`,
		id, strings.Repeat("b", 100_001)); err != nil {
		t.Fatalf("plant over-cap system: %v", err)
	}
	status, body = update(map[string]any{"name": "renamed"})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	if status, body := update(map[string]any{"system": "short"}); status != http.StatusOK {
		t.Fatalf("update replacing the over-cap system: status %d (body %v)", status, body)
	}
}

func TestAgentMCPServerNamesUnique(t *testing.T) {
	s := newTestServer(t)
	wantAgentRejected(t, s, agentBody(map[string]any{
		"mcp_servers": []any{mcpServer("srv"), mcpServer("srv")},
		"tools":       []any{mcpToolset("srv")},
	}), "srv")
}

func TestAgentUnreferencedMCPServerRejected(t *testing.T) {
	s := newTestServer(t)
	// No tools at all, and tools that reference a different server: both leave
	// "srv" unreferenced.
	wantAgentRejected(t, s, agentBody(map[string]any{
		"mcp_servers": []any{mcpServer("srv")},
	}), "srv")
	wantAgentRejected(t, s, agentBody(map[string]any{
		"mcp_servers": []any{mcpServer("srv"), mcpServer("other")},
		"tools":       []any{mcpToolset("other")},
	}), "srv")
}

func TestAgentToolNamesUniqueAcrossToolsets(t *testing.T) {
	s := newTestServer(t)
	// A custom tool named like an enabled built-in duplicates the name once the
	// agent_toolset expands — the Messages API rejects that request, so every
	// turn of the agent would fail.
	wantAgentRejected(t, s, agentBody(map[string]any{
		"tools": []any{map[string]any{"type": "agent_toolset_20260401"}, customTool("bash")},
	}), "bash")
	// Two custom tools with the same name are the same collision without any
	// toolset involved.
	wantAgentRejected(t, s, agentBody(map[string]any{
		"tools": []any{customTool("dup"), customTool("dup")},
	}), "dup")
	// The check resolves what the toolset actually enables, not the static
	// list: with bash disabled, a custom "bash" collides with nothing.
	createAgent(t, s, agentBody(map[string]any{
		"tools": []any{
			map[string]any{"type": "agent_toolset_20260401",
				"default_config": map[string]any{"enabled": false},
				"configs":        []any{map[string]any{"name": "read", "enabled": true}}},
			customTool("bash"),
		},
	}))
}

func TestAgentMetadataCaps(t *testing.T) {
	s := newTestServer(t)
	over := map[string]any{}
	for i := 0; i < 17; i++ {
		over[fmt.Sprintf("k%02d", i)] = "v"
	}
	wantAgentRejected(t, s, agentBody(map[string]any{"metadata": over}), "16")
	wantAgentRejected(t, s, agentBody(map[string]any{
		"metadata": map[string]any{strings.Repeat("k", 65): "v"}}), "64")
	wantAgentRejected(t, s, agentBody(map[string]any{
		"metadata": map[string]any{"k": strings.Repeat("v", 513)}}), "512")

	// The documented boundary itself is accepted: 16 pairs, one of them
	// carrying a 64-char key and a 512-char value.
	atCap := map[string]any{strings.Repeat("k", 64): strings.Repeat("v", 512)}
	for i := 0; i < 15; i++ {
		atCap[fmt.Sprintf("k%02d", i)] = "v"
	}
	createAgent(t, s, agentBody(map[string]any{"metadata": atCap}))

	// The caps are characters, not bytes (the filesupload.go rune-count
	// precedent): a 64-rune CJK key is three times as many UTF-8 bytes and
	// still within the documented cap, and the reject boundary is one rune
	// past it, not one byte.
	createAgent(t, s, agentBody(map[string]any{
		"metadata": map[string]any{strings.Repeat("键", 64): strings.Repeat("值", 512)}}))
	wantAgentRejected(t, s, agentBody(map[string]any{
		"metadata": map[string]any{strings.Repeat("键", 65): "v"}}), "64")
	wantAgentRejected(t, s, agentBody(map[string]any{
		"metadata": map[string]any{"k": strings.Repeat("值", 513)}}), "512")
}

// The update metadata patch is bounded on the resulting stored bag, exactly as
// vaults are: an upsert or a delete-and-add within the cap passes, a 17th key
// does not.
func TestAgentUpdateMetadataCapOnStoredBag(t *testing.T) {
	s := newTestServer(t)
	full := map[string]any{}
	for i := 0; i < 16; i++ {
		full[fmt.Sprintf("k%02d", i)] = "v"
	}
	res := createAgent(t, s, agentBody(map[string]any{"metadata": full}))
	id, _ := res["id"].(string)

	status, body := s.do(http.MethodPost, "/v1/agents/"+id,
		map[string]any{"metadata": map[string]any{"k16": "v"}})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// Upserting an existing key does not grow the bag.
	if status, body := s.do(http.MethodPost, "/v1/agents/"+id,
		map[string]any{"metadata": map[string]any{"k00": "changed"}}); status != http.StatusOK {
		t.Fatalf("upsert within cap: status %d (body %v)", status, body)
	}
	// Neither does deleting one key while adding another.
	if status, body := s.do(http.MethodPost, "/v1/agents/"+id,
		map[string]any{"metadata": map[string]any{"k01": nil, "k16": "v"}}); status != http.StatusOK {
		t.Fatalf("delete-and-add within cap: status %d (body %v)", status, body)
	}
}

// Update validates the merged result: the reference's wording is "every server
// must be referenced by an mcp_toolset in the agent's resulting tools", so an
// update that clears tools while keeping stored servers strands them and must
// reject — and the rejected update must not bump the version.
func TestAgentUpdateValidatesResultingSpec(t *testing.T) {
	s := newTestServer(t)
	res := createAgent(t, s, agentBody(map[string]any{
		"mcp_servers": []any{mcpServer("srv")},
		"tools":       []any{mcpToolset("srv")},
	}))
	id, _ := res["id"].(string)

	status, body := s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"tools": []any{}})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	status, got := s.do(http.MethodGet, "/v1/agents/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get after rejected update: %d", status)
	}
	if got["version"] != float64(1) {
		t.Errorf("version after rejected update = %v, want 1", got["version"])
	}

	// Clearing both halves together leaves nothing stranded.
	if status, body := s.do(http.MethodPost, "/v1/agents/"+id,
		map[string]any{"tools": []any{}, "mcp_servers": []any{}}); status != http.StatusOK {
		t.Fatalf("clearing both: status %d (body %v)", status, body)
	}
}
