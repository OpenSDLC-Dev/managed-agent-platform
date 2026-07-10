package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// agentRequiredFields is the full BetaManagedAgentsAgent wire surface; every
// field is api:"required" and must be present in every agent response.
var agentRequiredFields = []string{
	"id", "type", "name", "version", "model", "system", "description",
	"tools", "mcp_servers", "skills", "metadata", "multiagent",
	"created_at", "updated_at", "archived_at",
}

func createAgent(t *testing.T, s *tserver, body map[string]any) map[string]any {
	t.Helper()
	status, res := s.do(http.MethodPost, "/v1/agents", body)
	if status != http.StatusOK {
		t.Fatalf("create agent: status %d, body %v", status, res)
	}
	return res
}

func TestAgentCreateMinimal(t *testing.T) {
	s := newTestServer(t)
	res := createAgent(t, s, map[string]any{"name": "helper", "model": "claude-opus-4-8"})

	wantFields(t, res, agentRequiredFields...)
	id, _ := res["id"].(string)
	if !strings.HasPrefix(id, "agent_") {
		t.Errorf("id = %q, want agent_ prefix", id)
	}
	if res["type"] != "agent" {
		t.Errorf(`type = %v, want "agent"`, res["type"])
	}
	if res["name"] != "helper" {
		t.Errorf("name = %v", res["name"])
	}
	if res["version"] != float64(1) {
		t.Errorf("version = %v, want 1", res["version"])
	}
	model, _ := res["model"].(map[string]any)
	if model == nil || model["id"] != "claude-opus-4-8" {
		t.Errorf("model = %v, want object with id", res["model"])
	}
	if _, hasSpeed := model["speed"]; hasSpeed {
		t.Errorf("model.speed should be omitted when unset: %v", model)
	}
	if res["system"] != "" || res["description"] != "" {
		t.Errorf("system/description should default to empty strings: %v / %v", res["system"], res["description"])
	}
	for _, k := range []string{"tools", "mcp_servers", "skills"} {
		if arr, ok := res[k].([]any); !ok || len(arr) != 0 {
			t.Errorf("%s = %v, want []", k, res[k])
		}
	}
	if md, ok := res["metadata"].(map[string]any); !ok || len(md) != 0 {
		t.Errorf("metadata = %v, want {}", res["metadata"])
	}
	if res["multiagent"] != nil {
		t.Errorf("multiagent = %v, want null", res["multiagent"])
	}
	if res["archived_at"] != nil {
		t.Errorf("archived_at = %v, want null", res["archived_at"])
	}
	for _, k := range []string{"created_at", "updated_at"} {
		ts, _ := res[k].(string)
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			t.Errorf("%s = %q is not RFC3339: %v", k, ts, err)
		}
		if !strings.HasSuffix(ts, "Z") {
			t.Errorf("%s = %q must be UTC (Z suffix), not a local offset", k, ts)
		}
	}
}

func TestAgentCreateModelObjectAndFullConfig(t *testing.T) {
	s := newTestServer(t)
	tools := []any{
		map[string]any{
			"type":           "agent_toolset_20260401",
			"default_config": map[string]any{"enabled": true, "permission_policy": map[string]any{"type": "always_allow"}},
			"configs": []any{
				map[string]any{"name": "bash", "enabled": true, "permission_policy": map[string]any{"type": "always_ask"}},
			},
		},
		map[string]any{
			"type":         "custom",
			"name":         "lookup_order",
			"description":  "Look up an order by id",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"order_id": map[string]any{"type": "string"}}},
		},
		map[string]any{
			"type":            "mcp_toolset",
			"mcp_server_name": "docs",
		},
	}
	body := map[string]any{
		"name":        "full",
		"model":       map[string]any{"id": "claude-opus-4-8", "speed": "fast"},
		"system":      "be careful",
		"description": "a fully configured agent",
		"tools":       tools,
		"mcp_servers": []any{map[string]any{"type": "url", "name": "docs", "url": "https://mcp.example.com"}},
		"skills":      []any{map[string]any{"type": "anthropic", "skill_id": "xlsx", "version": "latest"}},
		"metadata":    map[string]any{"team": "sre"},
	}
	res := createAgent(t, s, body)

	model, _ := res["model"].(map[string]any)
	if model["id"] != "claude-opus-4-8" || model["speed"] != "fast" {
		t.Errorf("model = %v", res["model"])
	}
	if res["system"] != "be careful" || res["description"] != "a fully configured agent" {
		t.Errorf("system/description = %v / %v", res["system"], res["description"])
	}
	if !reflect.DeepEqual(jsonNorm(t, res["tools"]), jsonNorm(t, tools)) {
		t.Errorf("tools did not round-trip:\ngot  %v\nwant %v", res["tools"], tools)
	}
	if !reflect.DeepEqual(jsonNorm(t, res["mcp_servers"]), jsonNorm(t, body["mcp_servers"])) {
		t.Errorf("mcp_servers did not round-trip: %v", res["mcp_servers"])
	}
	if !reflect.DeepEqual(jsonNorm(t, res["skills"]), jsonNorm(t, body["skills"])) {
		t.Errorf("skills did not round-trip: %v", res["skills"])
	}
	if md, _ := res["metadata"].(map[string]any); md["team"] != "sre" {
		t.Errorf("metadata = %v", res["metadata"])
	}

	// An omitted skill version is normalized to "latest": the response-side
	// skill carries a required version on the wire.
	bare := createAgent(t, s, map[string]any{
		"name": "bare-skill", "model": "m",
		"skills": []any{map[string]any{"type": "anthropic", "skill_id": "pdf"}},
	})
	if sk, _ := bare["skills"].([]any); len(sk) != 1 {
		t.Fatalf("skills = %v", bare["skills"])
	} else if entry, _ := sk[0].(map[string]any); entry["version"] != "latest" {
		t.Errorf(`omitted skill version = %v, want "latest"`, entry["version"])
	}

	// The stored resource survives a GET unchanged.
	status, got := s.do(http.MethodGet, "/v1/agents/"+res["id"].(string), nil)
	if status != http.StatusOK {
		t.Fatalf("get: %d", status)
	}
	if !reflect.DeepEqual(jsonNorm(t, got["tools"]), jsonNorm(t, tools)) {
		t.Errorf("tools changed across GET: %v", got["tools"])
	}
}

// jsonNorm round-trips a value through JSON so numeric types compare equal.
func jsonNorm(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestAgentCreateValidation(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		name string
		body any
	}{
		{"missing name", map[string]any{"model": "claude-opus-4-8"}},
		{"missing model", map[string]any{"name": "x"}},
		{"unknown tool type", map[string]any{"name": "x", "model": "m", "tools": []any{map[string]any{"type": "bogus"}}}},
		{"custom tool without name", map[string]any{"name": "x", "model": "m", "tools": []any{map[string]any{"type": "custom", "description": "d", "input_schema": map[string]any{"type": "object"}}}}},
		{"mcp_toolset without server name", map[string]any{"name": "x", "model": "m", "tools": []any{map[string]any{"type": "mcp_toolset"}}}},
		{"multiagent unsupported", map[string]any{"name": "x", "model": "m", "multiagent": map[string]any{"type": "coordinator", "agents": []any{"agent_x"}}}},
		{"malformed json", `{"name": `},
		{"non-object body", `"just a string"`},
	}
	for _, tc := range cases {
		status, body := s.do(http.MethodPost, "/v1/agents", tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (body %v)", tc.name, status, body)
			continue
		}
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}
}

func TestAgentGet(t *testing.T) {
	s := newTestServer(t)
	created := createAgent(t, s, map[string]any{"name": "g", "model": "m1"})
	id := created["id"].(string)

	status, got := s.do(http.MethodGet, "/v1/agents/"+id, nil)
	if status != http.StatusOK || got["id"] != id {
		t.Fatalf("get: %d %v", status, got)
	}
	wantFields(t, got, agentRequiredFields...)

	status, body := s.do(http.MethodGet, "/v1/agents/agent_doesnotexist", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}

func TestAgentUpdateOptimisticVersioning(t *testing.T) {
	s := newTestServer(t)
	created := createAgent(t, s, map[string]any{"name": "v", "model": "m1", "system": "keep me"})
	id := created["id"].(string)

	// version is required.
	status, body := s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"name": "v2"})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// Happy path: version 1 → 2; omitted fields preserved.
	status, updated := s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"version": 1, "name": "renamed"})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, updated)
	}
	if updated["version"] != float64(2) || updated["name"] != "renamed" {
		t.Errorf("version/name = %v/%v, want 2/renamed", updated["version"], updated["name"])
	}
	if updated["system"] != "keep me" {
		t.Errorf("omitted system was not preserved: %v", updated["system"])
	}

	// Stale version → 409 with the error envelope.
	status, body = s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"version": 1, "name": "loser"})
	wantErr(t, status, body, http.StatusConflict, "invalid_request_error")

	// The conflicting update changed nothing.
	_, got := s.do(http.MethodGet, "/v1/agents/"+id, nil)
	if got["name"] != "renamed" || got["version"] != float64(2) {
		t.Errorf("state changed after 409: %v", got)
	}

	// Unknown agent → 404.
	status, body = s.do(http.MethodPost, "/v1/agents/agent_missing", map[string]any{"version": 1})
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}

func TestAgentUpdatePatchSemantics(t *testing.T) {
	s := newTestServer(t)
	created := createAgent(t, s, map[string]any{
		"name": "p", "model": "m1", "system": "sys", "description": "desc",
		"metadata": map[string]any{"keep": "1", "drop": "2"},
	})
	id := created["id"].(string)

	status, updated := s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{
		"version":     1,
		"description": nil, // explicit null clears
		"metadata":    map[string]any{"drop": nil, "new": "3"},
		"model":       map[string]any{"id": "m2"},
		"tools":       []any{map[string]any{"type": "agent_toolset_20260401"}},
	})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, updated)
	}
	if updated["description"] != "" {
		t.Errorf("null description should clear it, got %v", updated["description"])
	}
	if updated["system"] != "sys" {
		t.Errorf("omitted system changed: %v", updated["system"])
	}
	md, _ := updated["metadata"].(map[string]any)
	if !reflect.DeepEqual(md, map[string]any{"keep": "1", "new": "3"}) {
		t.Errorf("metadata patch = %v, want keep+new only", md)
	}
	if m, _ := updated["model"].(map[string]any); m["id"] != "m2" {
		t.Errorf("model = %v", updated["model"])
	}
	if tools, _ := updated["tools"].([]any); len(tools) != 1 {
		t.Errorf("tools = %v, want the replacement toolset", updated["tools"])
	}

	// Tools are a full replacement: sending [] clears them.
	status, updated = s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"version": 2, "tools": []any{}})
	if status != http.StatusOK {
		t.Fatalf("clear tools: %d", status)
	}
	if tools, _ := updated["tools"].([]any); len(tools) != 0 {
		t.Errorf("tools = %v, want []", updated["tools"])
	}
}

func TestAgentVersionsSnapshotHistory(t *testing.T) {
	s := newTestServer(t)
	created := createAgent(t, s, map[string]any{"name": "h1", "model": "m1"})
	id := created["id"].(string)
	for i := 1; i <= 2; i++ {
		status, _ := s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"version": i, "name": fmt.Sprintf("h%d", i+1)})
		if status != http.StatusOK {
			t.Fatalf("update %d failed", i)
		}
	}

	// Pinned reads return the immutable snapshots.
	status, v1 := s.do(http.MethodGet, "/v1/agents/"+id+"?version=1", nil)
	if status != http.StatusOK || v1["name"] != "h1" || v1["version"] != float64(1) {
		t.Errorf("version=1: %d %v", status, v1)
	}
	wantFields(t, v1, agentRequiredFields...)
	status, body := s.do(http.MethodGet, "/v1/agents/"+id+"?version=9", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")

	// The versions list has all three snapshots, newest first.
	status, list := s.do(http.MethodGet, "/v1/agents/"+id+"/versions", nil)
	if status != http.StatusOK {
		t.Fatalf("versions: %d", status)
	}
	entries := listData(t, list)
	if len(entries) != 3 {
		t.Fatalf("versions = %d entries, want 3", len(entries))
	}
	for i, wantV := range []float64{3, 2, 1} {
		if entries[i]["version"] != wantV {
			t.Errorf("entry %d version = %v, want %v", i, entries[i]["version"], wantV)
		}
		wantFields(t, entries[i], agentRequiredFields...)
	}

	// Versions of an unknown agent → 404.
	status, body = s.do(http.MethodGet, "/v1/agents/agent_missing/versions", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")

	// Version lists paginate on the version number.
	status, page1 := s.do(http.MethodGet, "/v1/agents/"+id+"/versions?limit=2", nil)
	if status != http.StatusOK {
		t.Fatalf("versions page 1: %d", status)
	}
	if entries := listData(t, page1); len(entries) != 2 || entries[0]["version"] != float64(3) {
		t.Fatalf("versions page 1 = %v", entries)
	}
	status, page2 := s.do(http.MethodGet, "/v1/agents/"+id+"/versions?limit=2&page="+nextPage(t, page1), nil)
	if status != http.StatusOK {
		t.Fatalf("versions page 2: %d", status)
	}
	if entries := listData(t, page2); len(entries) != 1 || entries[0]["version"] != float64(1) {
		t.Errorf("versions page 2 = %v, want just version 1", entries)
	}
	if got := nextPage(t, page2); got != "" {
		t.Errorf("versions final page next_page = %q, want null", got)
	}
}

func TestAgentListPagination(t *testing.T) {
	s := newTestServer(t)
	var ids []string
	for i := 0; i < 3; i++ {
		res := createAgent(t, s, map[string]any{"name": fmt.Sprintf("a%d", i), "model": "m"})
		ids = append(ids, res["id"].(string))
	}

	status, page1 := s.do(http.MethodGet, "/v1/agents?limit=2", nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d", status)
	}
	d1 := listData(t, page1)
	if len(d1) != 2 {
		t.Fatalf("page 1 = %d entries, want 2", len(d1))
	}
	// Newest first.
	if d1[0]["id"] != ids[2] || d1[1]["id"] != ids[1] {
		t.Errorf("order = %v,%v want %v,%v", d1[0]["id"], d1[1]["id"], ids[2], ids[1])
	}
	cursor := nextPage(t, page1)
	if cursor == "" {
		t.Fatal("next_page empty with more rows remaining")
	}

	status, page2 := s.do(http.MethodGet, "/v1/agents?limit=2&page="+cursor, nil)
	if status != http.StatusOK {
		t.Fatalf("page 2: %d", status)
	}
	d2 := listData(t, page2)
	if len(d2) != 1 || d2[0]["id"] != ids[0] {
		t.Errorf("page 2 = %v", d2)
	}
	if got := nextPage(t, page2); got != "" {
		t.Errorf("next_page on final page = %q, want null", got)
	}

	// Limit bounds: 0 and 101 are invalid; a bogus cursor is invalid.
	for _, q := range []string{"limit=0", "limit=101", "page=@@@"} {
		status, body := s.do(http.MethodGet, "/v1/agents?"+q, nil)
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}
}

func TestAgentListCreatedAtFilters(t *testing.T) {
	s := newTestServer(t)
	createAgent(t, s, map[string]any{"name": "old", "model": "m"})

	// A gte filter far in the future excludes everything.
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	status, list := s.do(http.MethodGet, "/v1/agents?created_at[gte]="+future, nil)
	if status != http.StatusOK {
		t.Fatalf("filtered list: %d %v", status, list)
	}
	if entries := listData(t, list); len(entries) != 0 {
		t.Errorf("future gte filter returned %d entries", len(entries))
	}
	// An lte filter in the future includes it.
	status, list = s.do(http.MethodGet, "/v1/agents?created_at[lte]="+future, nil)
	if status != http.StatusOK || len(listData(t, list)) != 1 {
		t.Errorf("lte filter: %d %v", status, list)
	}
	// Malformed timestamp → 400.
	status, body := s.do(http.MethodGet, "/v1/agents?created_at[gte]=yesterday", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
}

func TestAgentArchive(t *testing.T) {
	s := newTestServer(t)
	kept := createAgent(t, s, map[string]any{"name": "kept", "model": "m"})
	arch := createAgent(t, s, map[string]any{"name": "arch", "model": "m"})
	id := arch["id"].(string)

	status, archived := s.do(http.MethodPost, "/v1/agents/"+id+"/archive", nil)
	if status != http.StatusOK {
		t.Fatalf("archive: %d %v", status, archived)
	}
	first, _ := archived["archived_at"].(string)
	if _, err := time.Parse(time.RFC3339, first); err != nil {
		t.Fatalf("archived_at = %v: %v", archived["archived_at"], err)
	}

	// Archiving again is idempotent and keeps the original timestamp.
	status, again := s.do(http.MethodPost, "/v1/agents/"+id+"/archive", nil)
	if status != http.StatusOK || again["archived_at"] != first {
		t.Errorf("second archive: %d, archived_at %v (want %v)", status, again["archived_at"], first)
	}

	// Hidden from the default list, visible with include_archived, still GETtable.
	_, list := s.do(http.MethodGet, "/v1/agents", nil)
	if entries := listData(t, list); len(entries) != 1 || entries[0]["id"] != kept["id"] {
		t.Errorf("default list = %v, want only the kept agent", entries)
	}
	_, list = s.do(http.MethodGet, "/v1/agents?include_archived=true", nil)
	if entries := listData(t, list); len(entries) != 2 {
		t.Errorf("include_archived list = %d entries, want 2", len(entries))
	}
	status, got := s.do(http.MethodGet, "/v1/agents/"+id, nil)
	if status != http.StatusOK || got["archived_at"] == nil {
		t.Errorf("get archived: %d %v", status, got["archived_at"])
	}

	// Archive of an unknown agent → 404.
	status, body := s.do(http.MethodPost, "/v1/agents/agent_missing/archive", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}
