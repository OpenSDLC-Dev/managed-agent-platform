package api_test

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// sessionRequiredFields is the BetaManagedAgentsSession wire surface; all
// fields are api:"required" except deployment_id (nullable but present).
var sessionRequiredFields = []string{
	"id", "type", "agent", "environment_id", "status", "title", "metadata",
	"usage", "stats", "outcome_evaluations", "resources", "vault_ids",
	"deployment_id", "created_at", "updated_at", "archived_at",
}

// sessionAgentRequiredFields is the resolved-agent snapshot embedded in a
// session (BetaManagedAgentsSessionAgent) — all api:"required".
var sessionAgentRequiredFields = []string{
	"id", "type", "name", "version", "model", "system", "description",
	"tools", "mcp_servers", "skills", "multiagent",
}

// Slice 3: a session attaches vaults at create time (top-level vault_ids). The
// ids must name existing, unarchived vaults; the list round-trips on the
// response and on GET; update still rejects vault_ids changes (create-only).
func TestSessionVaultAttachment(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	vaultA := createVault(t, s, "attach-a")
	vaultB := createVault(t, s, "attach-b")

	res := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID, "vault_ids": []any{vaultA, vaultB}})
	got := res["vault_ids"].([]any)
	if len(got) != 2 || got[0] != vaultA || got[1] != vaultB {
		t.Fatalf("vault_ids not round-tripped in order: %v", got)
	}
	sid := res["id"].(string)
	_, fetched := s.do(http.MethodGet, "/v1/sessions/"+sid, nil)
	if got := fetched["vault_ids"].([]any); len(got) != 2 || got[0] != vaultA || got[1] != vaultB {
		t.Fatalf("GET did not echo vault_ids in order: %v", got)
	}

	// Empty/omitted vault_ids is fine and echoes an empty array.
	res2 := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	if len(res2["vault_ids"].([]any)) != 0 {
		t.Fatalf("omitted vault_ids should echo []: %v", res2["vault_ids"])
	}

	// An archived vault fails the create with the standard error envelope.
	s.do(http.MethodPost, "/v1/vaults/"+vaultB+"/archive", nil)
	status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agentID, "environment_id": envID, "vault_ids": []any{vaultA, vaultB}})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	// The unarchived vault alone still succeeds.
	createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID, "vault_ids": []any{vaultA}})
}

// fixture creates an agent and an environment and returns their ids.
func fixture(t *testing.T, s *tserver) (agentID, envID string) {
	t.Helper()
	a := createAgent(t, s, map[string]any{"name": "task-agent", "model": "claude-opus-4-8", "system": "base system"})
	e := createEnvironment(t, s, map[string]any{"name": "task-env"})
	return a["id"].(string), e["id"].(string)
}

func createSession(t *testing.T, s *tserver, body map[string]any) map[string]any {
	t.Helper()
	status, res := s.do(http.MethodPost, "/v1/sessions", body)
	if status != http.StatusOK {
		t.Fatalf("create session: status %d, body %v", status, res)
	}
	return res
}

// The unknown-field rejection covers every API path that accepts a tools array,
// not just agent create: a misspelled permission_policy in a session's inline
// agent_with_overrides tools, or in a session-update agent.tools patch, is a 400
// before the malformed toolset is stored on the session snapshot (issue #26).
func TestSessionToolsetRejectsUnknownField(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	badTools := []any{map[string]any{
		"type":           "agent_toolset_20260401",
		"default_config": map[string]any{"permission_polciy": map[string]any{"type": "always_ask"}},
	}}

	// Session create, via agent_with_overrides.tools.
	status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent":          map[string]any{"type": "agent_with_overrides", "id": agentID, "tools": badTools},
		"environment_id": envID,
	})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	// The rejected session was not stored.
	_, list := s.do(http.MethodGet, "/v1/sessions", nil)
	if entries := listData(t, list); len(entries) != 0 {
		t.Errorf("sessions after rejected create = %d, want 0 (not persisted)", len(entries))
	}

	// Session update, via the agent.tools patch on an existing session.
	id := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
	status, body = s.do(http.MethodPost, "/v1/sessions/"+id, map[string]any{
		"agent": map[string]any{"tools": badTools},
	})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
}

func TestSessionCreateWithAgentString(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	res := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})

	wantFields(t, res, sessionRequiredFields...)
	id, _ := res["id"].(string)
	if !strings.HasPrefix(id, "sesn_") {
		t.Errorf("id = %q, want sesn_ prefix", id)
	}
	if res["type"] != "session" || res["status"] != "idle" {
		t.Errorf("type/status = %v/%v, want session/idle", res["type"], res["status"])
	}
	if res["environment_id"] != envID {
		t.Errorf("environment_id = %v", res["environment_id"])
	}
	if res["title"] != "" {
		t.Errorf("title = %v, want empty string", res["title"])
	}

	agent, _ := res["agent"].(map[string]any)
	wantFields(t, agent, sessionAgentRequiredFields...)
	if agent["id"] != agentID || agent["version"] != float64(1) || agent["type"] != "agent" {
		t.Errorf("agent snapshot = %v", agent)
	}
	if agent["system"] != "base system" || agent["name"] != "task-agent" {
		t.Errorf("agent snapshot content = %v", agent)
	}

	usage, _ := res["usage"].(map[string]any)
	wantFields(t, usage, "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation")
	cc, _ := usage["cache_creation"].(map[string]any)
	wantFields(t, cc, "ephemeral_1h_input_tokens", "ephemeral_5m_input_tokens")
	if usage["input_tokens"] != float64(0) {
		t.Errorf("usage.input_tokens = %v, want 0", usage["input_tokens"])
	}
	stats, _ := res["stats"].(map[string]any)
	wantFields(t, stats, "active_seconds", "duration_seconds")
	for _, k := range []string{"outcome_evaluations", "resources", "vault_ids"} {
		if arr, ok := res[k].([]any); !ok || len(arr) != 0 {
			t.Errorf("%s = %v, want []", k, res[k])
		}
	}
	if res["deployment_id"] != nil || res["archived_at"] != nil {
		t.Errorf("deployment_id/archived_at = %v/%v, want null/null", res["deployment_id"], res["archived_at"])
	}
	for _, k := range []string{"created_at", "updated_at"} {
		ts, _ := res[k].(string)
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			t.Errorf("%s = %q not RFC3339: %v", k, ts, err)
		}
		if !strings.HasSuffix(ts, "Z") {
			t.Errorf("%s = %q must be UTC (Z suffix), not a local offset", k, ts)
		}
	}
}

func TestSessionCreatePinsAgentVersionAndSupportsOverrides(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	// Bump the agent so v1 and v2 differ.
	status, _ := s.do(http.MethodPost, "/v1/agents/"+agentID, map[string]any{"version": 1, "system": "v2 system"})
	if status != http.StatusOK {
		t.Fatalf("agent update: %d", status)
	}

	// Plain string pins the latest version.
	latest := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	if a, _ := latest["agent"].(map[string]any); a["version"] != float64(2) || a["system"] != "v2 system" {
		t.Errorf("latest snapshot = %v", latest["agent"])
	}

	// {type:"agent", version:1} pins the old snapshot.
	pinned := createSession(t, s, map[string]any{
		"agent":          map[string]any{"type": "agent", "id": agentID, "version": 1},
		"environment_id": envID,
	})
	if a, _ := pinned["agent"].(map[string]any); a["version"] != float64(1) || a["system"] != "base system" {
		t.Errorf("pinned snapshot = %v", pinned["agent"])
	}

	// agent_with_overrides overlays fields; id/version still reference the base.
	tools := []any{map[string]any{"type": "agent_toolset_20260401"}}
	over := createSession(t, s, map[string]any{
		"agent": map[string]any{
			"type": "agent_with_overrides", "id": agentID,
			"system": "override system",
			"model":  map[string]any{"id": "claude-haiku-4-5"},
			"tools":  tools,
		},
		"environment_id": envID,
	})
	a, _ := over["agent"].(map[string]any)
	if a["system"] != "override system" || a["id"] != agentID || a["version"] != float64(2) {
		t.Errorf("override snapshot = %v", a)
	}
	if m, _ := a["model"].(map[string]any); m["id"] != "claude-haiku-4-5" {
		t.Errorf("override model = %v", a["model"])
	}
	if !reflect.DeepEqual(jsonNorm(t, a["tools"]), jsonNorm(t, tools)) {
		t.Errorf("override tools = %v", a["tools"])
	}
	// The base agent resource is untouched by overrides.
	_, base := s.do(http.MethodGet, "/v1/agents/"+agentID, nil)
	if base["system"] != "v2 system" {
		t.Errorf("base agent mutated by overrides: %v", base["system"])
	}

	// A skills override is capped at 500 like the agent's own list.
	many := make([]any, 501)
	for i := range many {
		many[i] = map[string]any{"type": "custom", "skill_id": fmt.Sprintf("skill_%022d", i)}
	}
	status, obj := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent":          map[string]any{"type": "agent_with_overrides", "id": agentID, "skills": many},
		"environment_id": envID,
	})
	wantErr(t, status, obj, http.StatusBadRequest, "invalid_request_error")

	// system:null explicitly clears the system prompt (SDK-documented).
	cleared := createSession(t, s, map[string]any{
		"agent":          map[string]any{"type": "agent_with_overrides", "id": agentID, "system": nil},
		"environment_id": envID,
	})
	if a, _ := cleared["agent"].(map[string]any); a["system"] != "" {
		t.Errorf("system:null override should clear, got %v", a["system"])
	}
}

func TestSessionCreateValidation(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	// Archived referents are rejected.
	archAgent := createAgent(t, s, map[string]any{"name": "aa", "model": "m"})
	s.do(http.MethodPost, "/v1/agents/"+archAgent["id"].(string)+"/archive", nil)
	archEnv := createEnvironment(t, s, map[string]any{"name": "ae"})
	s.do(http.MethodPost, "/v1/environments/"+archEnv["id"].(string)+"/archive", nil)

	for name, tc := range map[string]struct {
		body       any
		wantStatus int
		wantType   string
	}{
		"missing agent":        {map[string]any{"environment_id": envID}, 400, "invalid_request_error"},
		"missing environment":  {map[string]any{"agent": agentID}, 400, "invalid_request_error"},
		"unknown agent":        {map[string]any{"agent": "agent_missing", "environment_id": envID}, 404, "not_found_error"},
		"unknown agent object": {map[string]any{"agent": map[string]any{"type": "agent", "id": "agent_missing"}, "environment_id": envID}, 404, "not_found_error"},
		"unknown version":      {map[string]any{"agent": map[string]any{"type": "agent", "id": agentID, "version": 99}, "environment_id": envID}, 404, "not_found_error"},
		"bad agent union type": {map[string]any{"agent": map[string]any{"type": "wizard", "id": agentID}, "environment_id": envID}, 400, "invalid_request_error"},
		"unknown environment":  {map[string]any{"agent": agentID, "environment_id": "env_missing"}, 404, "not_found_error"},
		"archived agent":       {map[string]any{"agent": archAgent["id"], "environment_id": envID}, 400, "invalid_request_error"},
		"archived environment": {map[string]any{"agent": agentID, "environment_id": archEnv["id"]}, 400, "invalid_request_error"},
		"github resources unsupported": {map[string]any{"agent": agentID, "environment_id": envID,
			"resources": []any{map[string]any{"type": "github_repository", "url": "https://github.com/x/y"}}}, 400, "invalid_request_error"},
		"memory resources unsupported": {map[string]any{"agent": agentID, "environment_id": envID,
			"resources": []any{map[string]any{"type": "memory_store", "memory_store_id": "mem_x"}}}, 400, "invalid_request_error"},
		"unknown vault": {map[string]any{"agent": agentID, "environment_id": envID,
			"vault_ids": []any{"vlt_missing0000000000000000"}}, 400, "invalid_request_error"},
		"malformed vault id": {map[string]any{"agent": agentID, "environment_id": envID,
			"vault_ids": []any{"env_wrongprefix"}}, 400, "invalid_request_error"},
		"malformed json": {`{"agent": `, 400, "invalid_request_error"},
	} {
		status, body := s.do(http.MethodPost, "/v1/sessions", tc.body)
		if status != tc.wantStatus {
			t.Errorf("%s: status %d, want %d (%v)", name, status, tc.wantStatus, body)
			continue
		}
		wantErr(t, status, body, tc.wantStatus, tc.wantType)
	}
}

func TestSessionGetAcceptsAltSessionPrefix(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	created := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID, "title": "alt"})
	id := created["id"].(string)

	// The wire accepts session_… as an alternate spelling of sesn_….
	alt := "session_" + strings.TrimPrefix(id, "sesn_")
	status, got := s.do(http.MethodGet, "/v1/sessions/"+alt, nil)
	if status != http.StatusOK || got["id"] != id {
		t.Fatalf("get with session_ prefix: %d %v", status, got)
	}

	status, body := s.do(http.MethodGet, "/v1/sessions/sesn_missing", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}

func TestSessionUpdate(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	created := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"metadata": map[string]any{"keep": "1", "drop": "2"},
	})
	id := created["id"].(string)

	tools := []any{map[string]any{"type": "agent_toolset_20260401"}}
	mcp := []any{map[string]any{"type": "url", "name": "docs", "url": "https://mcp.example.com"}}
	status, updated := s.do(http.MethodPost, "/v1/sessions/"+id, map[string]any{
		"title":    "titled",
		"metadata": map[string]any{"drop": nil, "new": "3"},
		"agent":    map[string]any{"tools": tools, "mcp_servers": mcp},
	})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, updated)
	}
	if updated["title"] != "titled" {
		t.Errorf("title = %v", updated["title"])
	}
	if md, _ := updated["metadata"].(map[string]any); !reflect.DeepEqual(md, map[string]any{"keep": "1", "new": "3"}) {
		t.Errorf("metadata = %v", updated["metadata"])
	}
	a, _ := updated["agent"].(map[string]any)
	if !reflect.DeepEqual(jsonNorm(t, a["tools"]), jsonNorm(t, tools)) {
		t.Errorf("agent.tools = %v", a["tools"])
	}
	if !reflect.DeepEqual(jsonNorm(t, a["mcp_servers"]), jsonNorm(t, mcp)) {
		t.Errorf("agent.mcp_servers = %v", a["mcp_servers"])
	}
	// The rest of the snapshot is untouched.
	if a["system"] != "base system" || a["version"] != float64(1) {
		t.Errorf("snapshot fields changed: %v", a)
	}

	// vault_ids on update matches the reference server: not yet supported. Any
	// presence is rejected — an array or an explicit null.
	status, body := s.do(http.MethodPost, "/v1/sessions/"+id, map[string]any{"vault_ids": []any{"vlt_x"}})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	status, body = s.do(http.MethodPost, "/v1/sessions/"+id, map[string]any{"vault_ids": nil})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// Only tools/mcp_servers are updatable inside agent.
	status, body = s.do(http.MethodPost, "/v1/sessions/"+id, map[string]any{"agent": map[string]any{"system": "sneaky"}})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	status, body = s.do(http.MethodPost, "/v1/sessions/sesn_missing", map[string]any{"title": "x"})
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}

func TestSessionListFiltersAndBidirectionalPagination(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	other := createAgent(t, s, map[string]any{"name": "other", "model": "m"})
	otherID := other["id"].(string)

	var ids []string
	for i := 0; i < 3; i++ {
		res := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
		ids = append(ids, res["id"].(string))
	}
	createSession(t, s, map[string]any{"agent": otherID, "environment_id": envID})

	// agent_id filter.
	status, list := s.do(http.MethodGet, "/v1/sessions?agent_id="+agentID, nil)
	if status != http.StatusOK || len(listData(t, list)) != 3 {
		t.Errorf("agent_id filter: %d %v", status, list)
	}
	// agent_version filter (all fixture sessions pin version 1).
	status, list = s.do(http.MethodGet, "/v1/sessions?agent_id="+agentID+"&agent_version=2", nil)
	if status != http.StatusOK || len(listData(t, list)) != 0 {
		t.Errorf("agent_version filter: %d %v", status, list)
	}
	// statuses[] filter, bracket style as the SDK sends it.
	status, list = s.do(http.MethodGet, "/v1/sessions?statuses[]=idle&statuses[]=running", nil)
	if status != http.StatusOK || len(listData(t, list)) != 4 {
		t.Errorf("statuses filter: %d %v", status, list)
	}
	status, list = s.do(http.MethodGet, "/v1/sessions?statuses[]=terminated", nil)
	if status != http.StatusOK || len(listData(t, list)) != 0 {
		t.Errorf("terminated filter: %d %v", status, list)
	}
	status, body := s.do(http.MethodGet, "/v1/sessions?statuses[]=zombie", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// deployment_id / memory_store_id reference features we don't host: no
	// session can match, so the result is empty rather than an error.
	for _, q := range []string{"deployment_id=depl_x", "memory_store_id=memstore_x"} {
		status, list = s.do(http.MethodGet, "/v1/sessions?"+q, nil)
		if status != http.StatusOK || len(listData(t, list)) != 0 {
			t.Errorf("%s: %d %v", q, status, list)
		}
	}

	// Bidirectional pagination, default order desc.
	status, page1 := s.do(http.MethodGet, "/v1/sessions?agent_id="+agentID+"&limit=2", nil)
	if status != http.StatusOK {
		t.Fatalf("page 1: %d", status)
	}
	d1 := listData(t, page1)
	if len(d1) != 2 || d1[0]["id"] != ids[2] || d1[1]["id"] != ids[1] {
		t.Errorf("page 1 = %v, want newest first", d1)
	}
	if v, ok := page1["prev_page"]; !ok || v != nil {
		t.Errorf("page 1 prev_page = %v (present %v), want null", v, ok)
	}
	status, page2 := s.do(http.MethodGet, "/v1/sessions?agent_id="+agentID+"&limit=2&page="+nextPage(t, page1), nil)
	if status != http.StatusOK {
		t.Fatalf("page 2: %d", status)
	}
	d2 := listData(t, page2)
	if len(d2) != 1 || d2[0]["id"] != ids[0] {
		t.Errorf("page 2 = %v", d2)
	}
	if prev, _ := page2["prev_page"].(string); prev == "" {
		t.Errorf("page 2 prev_page = %v, want a cursor", page2["prev_page"])
	}

	// Following prev_page from page 2 returns exactly page 1 again.
	prev, _ := page2["prev_page"].(string)
	status, back := s.do(http.MethodGet, "/v1/sessions?agent_id="+agentID+"&limit=2&page="+prev, nil)
	if status != http.StatusOK {
		t.Fatalf("prev page: %d", status)
	}
	db := listData(t, back)
	if len(db) != 2 || db[0]["id"] != ids[2] || db[1]["id"] != ids[1] {
		t.Errorf("prev page = %v, want page 1 content", db)
	}
	// Walking backwards from page 2 there is nothing before page 1, and the
	// forward cursor must lead back to page 2.
	if v, ok := back["prev_page"]; !ok || v != nil {
		t.Errorf("prev_page of first page = %v (present %v), want null", v, ok)
	}
	next2, _ := back["next_page"].(string)
	if next2 == "" {
		t.Fatalf("next_page after backwards walk missing: %v", back)
	}
	status, fwd := s.do(http.MethodGet, "/v1/sessions?agent_id="+agentID+"&limit=2&page="+next2, nil)
	if status != http.StatusOK || listData(t, fwd)[0]["id"] != ids[0] {
		t.Errorf("forward after backwards walk: %d %v", status, fwd)
	}

	// order=asc flips it.
	status, list = s.do(http.MethodGet, "/v1/sessions?agent_id="+agentID+"&order=asc&limit=1", nil)
	if status != http.StatusOK || listData(t, list)[0]["id"] != ids[0] {
		t.Errorf("order=asc: %d %v", status, list)
	}
	status, body = s.do(http.MethodGet, "/v1/sessions?order=sideways", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
}

func TestSessionArchiveAndDelete(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	id := sess["id"].(string)

	status, archived := s.do(http.MethodPost, "/v1/sessions/"+id+"/archive", nil)
	if status != http.StatusOK {
		t.Fatalf("archive: %d %v", status, archived)
	}
	if ts, _ := archived["archived_at"].(string); ts == "" {
		t.Fatalf("archived_at = %v", archived["archived_at"])
	}
	_, list := s.do(http.MethodGet, "/v1/sessions", nil)
	if entries := listData(t, list); len(entries) != 0 {
		t.Errorf("default list shows archived session: %v", entries)
	}
	_, list = s.do(http.MethodGet, "/v1/sessions?include_archived=true", nil)
	if entries := listData(t, list); len(entries) != 1 {
		t.Errorf("include_archived = %v", entries)
	}

	status, deleted := s.do(http.MethodDelete, "/v1/sessions/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, deleted)
	}
	if deleted["id"] != id || deleted["type"] != "session_deleted" {
		t.Errorf("delete response = %v", deleted)
	}
	status, body := s.do(http.MethodGet, "/v1/sessions/"+id, nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	status, body = s.do(http.MethodDelete, "/v1/sessions/sesn_missing", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}
