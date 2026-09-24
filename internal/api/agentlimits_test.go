package api_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
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

// The agent create and update params bound three strings the generated Go doc
// comments leave unbounded — name at 256, description at 2,048 and system at
// 100,000 characters — as the spec's maxLength (checked against
// anthropic-sdk-go v1.70.1 — spec
// components.schemas.BetaManagedAgentsCreateAgentParams.properties and checked
// against anthropic-sdk-go v1.70.1 — spec
// components.schemas.BetaManagedAgentsUpdateAgentParams.properties). Each binds
// the value a request supplies, never a stored one (#665).

// wantUpdateRejected asserts an agent update 400s with a message carrying frag,
// and that the refused update changed nothing: same version, same agent.
func wantUpdateRejected(t *testing.T, s *tserver, id string, body map[string]any, frag string) {
	t.Helper()
	_, before := s.do(http.MethodGet, "/v1/agents/"+id, nil)
	status, res := s.do(http.MethodPost, "/v1/agents/"+id, body)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	if msg := errMessage(res); !strings.Contains(msg, frag) {
		t.Errorf("error message %q does not mention %q", msg, frag)
	}
	if _, after := s.do(http.MethodGet, "/v1/agents/"+id, nil); !reflect.DeepEqual(after, before) {
		t.Errorf("refused update changed the agent (version %v, now %v)", before["version"], after["version"])
	}
}

func TestAgentSystemCap(t *testing.T) {
	s := newTestServer(t)
	// Recorded 2026-09-02: agent create refused 100,001 ASCII characters with
	// 400 invalid_request_error.
	wantAgentRejected(t, s, agentBody(map[string]any{"system": strings.Repeat("a", 100_001)}), "100000")
	id := createAgent(t, s, agentBody(map[string]any{"system": strings.Repeat("a", 100_000)}))["id"].(string)

	// Recorded: agent create took 100,000 "é", 200,000 UTF-8 bytes, so the unit
	// is not bytes. One more is refused here.
	createAgent(t, s, agentBody(map[string]any{"system": strings.Repeat("é", 100_000)}))
	wantAgentRejected(t, s, agentBody(map[string]any{"system": strings.Repeat("é", 100_001)}), "100000")
	// Our choice, not a recording: agent create was never probed past the BMP,
	// so counting code points rather than UTF-16 units carries over the session
	// override's recorded unit. 50,001 astral characters are 100,002 UTF-16
	// units and pass.
	createAgent(t, s, agentBody(map[string]any{"system": strings.Repeat("😀", 50_001)}))

	// Update is bound on the system it supplies — the spec's maxLength; the
	// reference's update was not probed.
	wantUpdateRejected(t, s, id, map[string]any{"system": strings.Repeat("a", 100_001)}, "100000")
	if status, body := s.do(http.MethodPost, "/v1/agents/"+id,
		map[string]any{"system": strings.Repeat("b", 100_000)}); status != http.StatusOK {
		t.Fatalf("update at the cap: status %d (body %v)", status, body)
	}
}

// Name and description: the spec's bounds, unprobed on the reference.
func TestAgentNameAndDescriptionCaps(t *testing.T) {
	s := newTestServer(t)
	wantAgentRejected(t, s, agentBody(map[string]any{"name": strings.Repeat("n", 257)}), "256")
	wantAgentRejected(t, s, agentBody(map[string]any{"description": strings.Repeat("d", 2049)}), "2048")
	// At both bounds in code points, three times as many UTF-8 bytes.
	id := createAgent(t, s, agentBody(map[string]any{
		"name": strings.Repeat("界", 256), "description": strings.Repeat("界", 2048)}))["id"].(string)

	wantUpdateRejected(t, s, id, map[string]any{"name": strings.Repeat("n", 257)}, "256")
	wantUpdateRejected(t, s, id, map[string]any{"description": strings.Repeat("d", 2049)}, "2048")
}

// An agent stored over all three string bounds — written before #665 enforced
// them, and planted here in its agent row and its version row as such an agent
// sits — is grandfathered: the bounds bind what a request supplies, so nothing
// that only reads the stored agent is stranded, and nothing that carries it
// forward cuts it down. Every step asserts the planted values themselves, not
// only a status.
func TestStoredOverCapAgentGrandfathered(t *testing.T) {
	s := newTestServer(t)
	envID := createEnvironment(t, s, map[string]any{"name": "env"})["id"].(string)
	legacy := createAgent(t, s, agentBody(map[string]any{"name": "legacy"}))["id"].(string)
	coord := createAgent(t, s, map[string]any{"name": "coordinator", "model": "claude-opus-4-8",
		"multiagent": map[string]any{"type": "coordinator", "agents": []any{legacy}}})["id"].(string)
	planted := map[string]string{
		"name":        strings.Repeat("n", 257),
		"description": strings.Repeat("d", 2049),
		"system":      strings.Repeat("b", 100_001),
	}
	plantAgent(t, s, legacy, planted)
	all := []string{"name", "description", "system"}
	wantPlanted := func(what string, obj map[string]any, fields ...string) {
		t.Helper()
		for _, f := range fields {
			if got, _ := obj[f].(string); got != planted[f] {
				t.Errorf("%s: %s has %d characters, want the planted %d", what, f, len(got), len(planted[f]))
			}
		}
	}
	sessionAgent := func(id string) map[string]any {
		t.Helper()
		status, res := s.do(http.MethodGet, "/v1/sessions/"+id, nil)
		if status != http.StatusOK {
			t.Fatalf("get session %s: status %d (body %v)", id, status, res)
		}
		agent, _ := res["agent"].(map[string]any)
		return agent
	}
	getAgent := func() map[string]any {
		t.Helper()
		_, res := s.do(http.MethodGet, "/v1/agents/"+legacy, nil)
		return res
	}
	update := func(what string, body map[string]any) {
		t.Helper()
		if status, res := s.do(http.MethodPost, "/v1/agents/"+legacy, body); status != http.StatusOK {
			t.Fatalf("%s: status %d (body %v)", what, status, res)
		}
	}

	// Session create resolves it by plain reference (the agent row) and pinned
	// to version 1 (the version row).
	var sid string
	for _, c := range []struct {
		what string
		ref  any
	}{
		{"plain session create", legacy},
		{"version-pinned session create", map[string]any{"type": "agent", "id": legacy, "version": 1}},
	} {
		res := createSession(t, s, map[string]any{"agent": c.ref, "environment_id": envID})
		wantPlanted(c.what, res["agent"].(map[string]any), all...)
		sid = res["id"].(string)
	}
	// A coordinator whose roster pins it starts, the member snapshotted whole.
	res := createSession(t, s, map[string]any{"agent": coord, "environment_id": envID})
	roster, _ := res["agent"].(map[string]any)["multiagent"].(map[string]any)
	members, _ := roster["agents"].([]any)
	found := false
	for _, m := range members {
		if member, _ := m.(map[string]any); member["id"] == legacy {
			wantPlanted("roster member", member, all...)
			found = true
		}
	}
	if !found {
		t.Fatalf("roster snapshot %v carries no member %s", members, legacy)
	}
	// A session carrying it still takes both agent patches.
	for _, patch := range []map[string]any{{"tools": []any{}}, {"mcp_servers": []any{}}} {
		if status, body := s.do(http.MethodPost, "/v1/sessions/"+sid,
			map[string]any{"agent": patch}); status != http.StatusOK {
			t.Fatalf("session patch %v: status %d (body %v)", patch, status, body)
		}
		wantPlanted(fmt.Sprintf("session after patch %v", patch), sessionAgent(sid), all...)
	}
	// A deployment of it fires.
	deplID := createDeployment(t, s, deploymentBody(legacy, envID))["id"].(string)
	run := runDeployment(t, s, deplID)
	wantPlanted("deployment-fired session", sessionAgent(run["session_id"].(string)), all...)

	// An agent update that does not resend a field keeps it as stored...
	update("metadata-only update", map[string]any{"metadata": map[string]any{"k": "v"}})
	wantPlanted("after a metadata-only update", getAgent(), all...)
	// ...while one that supplies an over-bound value is refused, even the very
	// value stored — and leaves the planted values, asserted just above, whole.
	for field, frag := range map[string]string{"name": "256", "description": "2048", "system": "100000"} {
		wantUpdateRejected(t, s, legacy, map[string]any{field: planted[field]}, frag)
	}
	wantPlanted("after the refused updates", getAgent(), all...)
	update("name-only update", map[string]any{"name": "renamed"})
	wantPlanted("after a name-only update", getAgent(), "description", "system")
	update("system-only update", map[string]any{"system": "short"})
	wantPlanted("after a system-only update", getAgent(), "description")
}

// plantAgent writes values into an agent's stored name, description or system
// around the API — into its agent row and every one of its version rows, where
// an agent written before #665 keeps them.
func plantAgent(t *testing.T, s *tserver, id string, values map[string]string) {
	t.Helper()
	for _, table := range []struct{ name, key string }{{"agents", "id"}, {"agent_versions", "agent_id"}} {
		for field, val := range values {
			q := `UPDATE ` + table.name + ` SET spec = jsonb_set(spec, '{` + field + `}', to_jsonb($2::text)) WHERE ` +
				table.key + ` = $1`
			if field == "name" {
				q = `UPDATE ` + table.name + ` SET name = $2 WHERE ` + table.key + ` = $1`
			}
			if _, err := s.pool.Exec(context.Background(), q, id, val); err != nil {
				t.Fatalf("plant stored %s in %s: %v", field, table.name, err)
			}
		}
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
