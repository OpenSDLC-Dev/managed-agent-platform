package api_test

import (
	"context"
	"net/http"
	"testing"
)

// TestCreateValidationEdgeCases sweeps the request-shape branches shared by
// the parsers: wrong JSON types, bad unions, bad nested fields.
func TestCreateValidationEdgeCases(t *testing.T) {
	s := newTestServer(t)

	agentCases := map[string]any{
		"model wrong type":        map[string]any{"name": "x", "model": true},
		"model without id":        map[string]any{"name": "x", "model": map[string]any{"speed": "fast"}},
		"model bad speed":         map[string]any{"name": "x", "model": map[string]any{"id": "m", "speed": "warp"}},
		"name wrong type":         map[string]any{"name": 7, "model": "m"},
		"tools not array":         map[string]any{"name": "x", "model": "m", "tools": map[string]any{}},
		"tools entry not object":  map[string]any{"name": "x", "model": "m", "tools": []any{"bash"}},
		"mcp_servers not array":   map[string]any{"name": "x", "model": "m", "mcp_servers": "docs"},
		"mcp_servers bad entry":   map[string]any{"name": "x", "model": "m", "mcp_servers": []any{map[string]any{"type": "stdio", "name": "d", "url": "u"}}},
		"mcp_servers non-object":  map[string]any{"name": "x", "model": "m", "mcp_servers": []any{1}},
		"skills bad type":         map[string]any{"name": "x", "model": "m", "skills": []any{map[string]any{"type": "community", "skill_id": "s"}}},
		"skills missing skill_id": map[string]any{"name": "x", "model": "m", "skills": []any{map[string]any{"type": "custom"}}},
		"skills entry non-object": map[string]any{"name": "x", "model": "m", "skills": []any{[]any{}}},
		"metadata not object":     map[string]any{"name": "x", "model": "m", "metadata": []any{}},
		"metadata non-string val": map[string]any{"name": "x", "model": "m", "metadata": map[string]any{"k": 1}},
		"body is array":           `[1,2]`,
		"unknown field":           map[string]any{"name": "x", "model": "m", "sytem": "typo"},
	}
	for name, body := range agentCases {
		status, res := s.do(http.MethodPost, "/v1/agents", body)
		if status != http.StatusBadRequest {
			t.Errorf("agent %s: status %d, want 400 (%v)", name, status, res)
			continue
		}
		wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	}

	envCases := map[string]any{
		"config not object":     map[string]any{"name": "x", "config": []any{}},
		"networking not object": map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "networking": []any{}}},
		"packages not object":   map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "packages": []any{}}},
		"self_hosted extras":    map[string]any{"name": "x", "config": map[string]any{"type": "self_hosted", "packages": map[string]any{}}},
		"cloud unknown field":   map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "gpu": true}},
		"scope wrong type":      map[string]any{"name": "x", "scope": 1},
		"scope unknown value":   map[string]any{"name": "x", "scope": "galaxy"},
		"unknown top-level":     map[string]any{"name": "x", "descripton": "typo"},
		"networking typo field": map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "networking": map[string]any{"type": "limited", "allowedHosts": []any{"a"}}}},
		"unrestricted extras":   map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "networking": map[string]any{"type": "unrestricted", "allowed_hosts": []any{"a"}}}},
		"hosts not a list":      map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "networking": map[string]any{"type": "limited", "allowed_hosts": "internal.corp"}}},
		"flag not a bool":       map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "networking": map[string]any{"type": "limited", "allow_mcp_servers": "yes"}}},
		"package not a list":    map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "packages": map[string]any{"pip": "requests"}}},
	}
	for name, body := range envCases {
		status, res := s.do(http.MethodPost, "/v1/environments", body)
		if status != http.StatusBadRequest {
			t.Errorf("environment %s: status %d, want 400 (%v)", name, status, res)
			continue
		}
		wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	}

	agentID, envID := fixture(t, s)
	sessionCases := map[string]any{
		"agent wrong type":        map[string]any{"agent": 7, "environment_id": envID},
		"agent object no id":      map[string]any{"agent": map[string]any{"type": "agent"}, "environment_id": envID},
		"negative version":        map[string]any{"agent": map[string]any{"type": "agent", "id": agentID, "version": -1}, "environment_id": envID},
		"null model override":     map[string]any{"agent": map[string]any{"type": "agent_with_overrides", "id": agentID, "model": nil}, "environment_id": envID},
		"explicit version zero":   map[string]any{"agent": map[string]any{"type": "agent", "id": agentID, "version": 0}, "environment_id": envID},
		"non-string sys override": map[string]any{"agent": map[string]any{"type": "agent_with_overrides", "id": agentID, "system": 5}, "environment_id": envID},
		"bad override tools":      map[string]any{"agent": map[string]any{"type": "agent_with_overrides", "id": agentID, "tools": []any{map[string]any{"type": "bogus"}}}, "environment_id": envID},
		"title wrong type":        map[string]any{"agent": agentID, "environment_id": envID, "title": 3},
		"resources not array":     map[string]any{"agent": agentID, "environment_id": envID, "resources": map[string]any{}},
		"unknown field":           map[string]any{"agent": agentID, "environment_id": envID, "titel": "typo"},
	}
	for name, body := range sessionCases {
		status, res := s.do(http.MethodPost, "/v1/sessions", body)
		if status != http.StatusBadRequest {
			t.Errorf("session %s: status %d, want 400 (%v)", name, status, res)
			continue
		}
		wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	}
}

func TestUpdateValidationEdgeCases(t *testing.T) {
	s := newTestServer(t)
	agent := createAgent(t, s, map[string]any{"name": "u", "model": "m"})
	agentID := agent["id"].(string)
	env := createEnvironment(t, s, map[string]any{"name": "ue"})
	envID := env["id"].(string)
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	sessID := sess["id"].(string)

	for name, tc := range map[string]struct {
		path string
		body any
	}{
		"agent version wrong type": {"/v1/agents/" + agentID, map[string]any{"version": "one"}},
		"agent model cleared":      {"/v1/agents/" + agentID, map[string]any{"version": 1, "model": nil}},
		"agent name cleared":       {"/v1/agents/" + agentID, map[string]any{"version": 1, "name": nil}},
		"agent metadata bad":       {"/v1/agents/" + agentID, map[string]any{"version": 1, "metadata": []any{}}},
		"agent malformed body":     {"/v1/agents/" + agentID, `{"version"`},
		"env name cleared":         {"/v1/environments/" + envID, map[string]any{"name": ""}},
		"env bad config":           {"/v1/environments/" + envID, map[string]any{"config": map[string]any{"type": "bad"}}},
		"env account scope":        {"/v1/environments/" + envID, map[string]any{"scope": "account"}},
		"session agent not object": {"/v1/sessions/" + sessID, map[string]any{"agent": "raw"}},
		"session bad tools":        {"/v1/sessions/" + sessID, map[string]any{"agent": map[string]any{"tools": "x"}}},
		"session bad mcp":          {"/v1/sessions/" + sessID, map[string]any{"agent": map[string]any{"mcp_servers": []any{map[string]any{"type": "url"}}}}},
		"session metadata bad":     {"/v1/sessions/" + sessID, map[string]any{"metadata": "x"}},
		"session title wrong type": {"/v1/sessions/" + sessID, map[string]any{"title": []any{}}},
	} {
		status, res := s.do(http.MethodPost, tc.path, tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%v)", name, status, res)
			continue
		}
		wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	}

	// Null title on session update clears it; null metadata is a no-op.
	status, updated := s.do(http.MethodPost, "/v1/sessions/"+sessID, map[string]any{"title": nil, "metadata": nil, "agent": nil})
	if status != http.StatusOK || updated["title"] != "" {
		t.Errorf("null title: %d %v", status, updated["title"])
	}
	// Null description clears; omitted config is preserved on environments.
	status, envUpd := s.do(http.MethodPost, "/v1/environments/"+envID, map[string]any{"description": nil, "config": nil})
	if status != http.StatusOK || envUpd["description"] != "" {
		t.Errorf("env null description: %d %v", status, envUpd["description"])
	}
	if cfg, _ := envUpd["config"].(map[string]any); cfg["type"] != "cloud" {
		t.Errorf("env config not preserved: %v", envUpd["config"])
	}
}

// TestNullFieldLeniency: the reference treats explicit null as "clear" (or
// "absent") for optional fields; none of these may error.
func TestNullFieldLeniency(t *testing.T) {
	s := newTestServer(t)

	agent := createAgent(t, s, map[string]any{
		"name": "n", "model": "m",
		"system": nil, "description": nil, "multiagent": nil,
		"tools": nil, "mcp_servers": nil, "skills": nil, "metadata": nil,
	})
	if agent["system"] != "" || agent["description"] != "" {
		t.Errorf("null strings should clear: %v/%v", agent["system"], agent["description"])
	}
	for _, k := range []string{"tools", "mcp_servers", "skills"} {
		if arr, ok := agent[k].([]any); !ok || len(arr) != 0 {
			t.Errorf("null %s should render []: %v", k, agent[k])
		}
	}

	status, updated := s.do(http.MethodPost, "/v1/agents/"+agent["id"].(string),
		map[string]any{"version": 1, "system": nil, "tools": nil})
	if status != http.StatusOK || updated["system"] != "" {
		t.Errorf("update null system: %d %v", status, updated["system"])
	}

	env := createEnvironment(t, s, map[string]any{
		"name": "e", "description": nil, "scope": "organization",
		"config": map[string]any{
			"type":       "cloud",
			"networking": map[string]any{"type": "limited"},
			"packages": map[string]any{
				"apt": []any{"jq"}, "cargo": nil, "gem": []any{}, "go": []any{"golang.org/x/tools"},
				"npm": []any{"left-pad"}, "pip": []any{"requests"},
			},
		},
	})
	cfg, _ := env["config"].(map[string]any)
	nw, _ := cfg["networking"].(map[string]any)
	if hosts, ok := nw["allowed_hosts"].([]any); !ok || len(hosts) != 0 {
		t.Errorf("limited without allowed_hosts should default []: %v", nw)
	}
	// Explicit null allowed_hosts also normalizes to [].
	nullHosts := createEnvironment(t, s, map[string]any{
		"name": "e2",
		"config": map[string]any{"type": "cloud",
			"networking": map[string]any{"type": "limited", "allowed_hosts": nil}},
	})
	cfg2, _ := nullHosts["config"].(map[string]any)
	if nw2, _ := cfg2["networking"].(map[string]any); nw2["allowed_hosts"] == nil {
		t.Errorf("null allowed_hosts should render []: %v", nw2)
	}
	pkgs, _ := cfg["packages"].(map[string]any)
	if cargo, ok := pkgs["cargo"].([]any); !ok || len(cargo) != 0 {
		t.Errorf("null cargo should render []: %v", pkgs["cargo"])
	}

	sess := createSession(t, s, map[string]any{
		"agent": map[string]any{
			"type": "agent_with_overrides", "id": agent["id"],
			"skills":      []any{map[string]any{"type": "anthropic", "skill_id": "xlsx"}},
			"mcp_servers": []any{map[string]any{"type": "url", "name": "d", "url": "https://x"}},
		},
		"environment_id": env["id"],
		"title":          nil, "metadata": nil, "resources": nil, "vault_ids": nil,
	})
	a, _ := sess["agent"].(map[string]any)
	if skills, _ := a["skills"].([]any); len(skills) != 1 {
		t.Errorf("skills override lost: %v", a["skills"])
	}
	if mcp, _ := a["mcp_servers"].([]any); len(mcp) != 1 {
		t.Errorf("mcp_servers override lost: %v", a["mcp_servers"])
	}

	// A literal null body is an empty object, so required-field checks fire.
	status, body := s.do(http.MethodPost, "/v1/agents", "null")
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
}

func TestQueryParamValidation(t *testing.T) {
	s := newTestServer(t)
	agent := createAgent(t, s, map[string]any{"name": "q", "model": "m"})
	id := agent["id"].(string)

	for name, path := range map[string]string{
		"bad include_archived":    "/v1/agents?include_archived=banana",
		"agent version zero":      "/v1/agents/" + id + "?version=0",
		"agent version not a num": "/v1/agents/" + id + "?version=abc",
		"session agent_version":   "/v1/sessions?agent_id=" + id + "&agent_version=abc",
	} {
		status, body := s.do(http.MethodGet, path, nil)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%v)", name, status, body)
			continue
		}
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}
}

// TestCorruptStoredDataSurfacesAsAPIError drives the defensive decode
// branches: rows corrupted out-of-band must produce the 500 api_error
// envelope, not a panic or a silent success.
func TestCorruptStoredDataSurfacesAsAPIError(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	agent := createAgent(t, s, map[string]any{"name": "c", "model": "m"})
	agentID := agent["id"].(string)
	env := createEnvironment(t, s, map[string]any{"name": "ce"})
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": env["id"]})
	sessID := sess["id"].(string)

	if _, err := s.pool.Exec(ctx, `UPDATE agents SET spec = '"corrupt"' WHERE id = $1`, agentID); err != nil {
		t.Fatalf("corrupt agent: %v", err)
	}
	status, body := s.do(http.MethodGet, "/v1/agents/"+agentID, nil)
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")

	if _, err := s.pool.Exec(ctx, `UPDATE sessions SET resolved_agent = '"corrupt"' WHERE id = $1`, sessID); err != nil {
		t.Fatalf("corrupt session: %v", err)
	}
	status, body = s.do(http.MethodGet, "/v1/sessions/"+sessID, nil)
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")

	if _, err := s.pool.Exec(ctx, `UPDATE environments SET metadata = '[]' WHERE id = $1`, env["id"]); err != nil {
		t.Fatalf("corrupt environment: %v", err)
	}
	status, body = s.do(http.MethodGet, "/v1/environments/"+env["id"].(string), nil)
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")

	// The corrupt rows also break the list, session-update, archive, and
	// pinned-version paths that decode them. Order matters: archiving the
	// corrupt session commits before its render fails, hiding it from the
	// default list and freezing it against updates — so those checks run first.
	for _, req := range []struct {
		name, method, path string
		body               any
	}{
		{"agents list", http.MethodGet, "/v1/agents", nil},
		{"environments list", http.MethodGet, "/v1/environments", nil},
		{"sessions list", http.MethodGet, "/v1/sessions", nil},
		{"session update", http.MethodPost, "/v1/sessions/" + sessID,
			map[string]any{"agent": map[string]any{"tools": []any{}}}},
		{"session archive", http.MethodPost, "/v1/sessions/" + sessID + "/archive", nil},
	} {
		status, body := s.do(req.method, req.path, req.body)
		if status != http.StatusInternalServerError {
			t.Errorf("%s: status %d, want 500 (%v)", req.name, status, body)
			continue
		}
		wantErr(t, status, body, http.StatusInternalServerError, "api_error")
	}

	// Corrupt agent metadata breaks the versions list and the pinned read
	// (both join the parent row's metadata).
	if _, err := s.pool.Exec(ctx, `UPDATE agents SET metadata = '[]' WHERE id = $1`, agentID); err != nil {
		t.Fatalf("corrupt agent metadata: %v", err)
	}
	status, body = s.do(http.MethodGet, "/v1/agents/"+agentID+"/versions", nil)
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")
	status, body = s.do(http.MethodGet, "/v1/agents/"+agentID+"?version=1", nil)
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")

	// A second session with corrupt usage breaks its own GET.
	sess2 := createSession(t, s, map[string]any{"agent": mustCleanAgent(t, s), "environment_id": mustCleanEnv(t, s)})
	if _, err := s.pool.Exec(ctx, `UPDATE sessions SET usage = '"x"' WHERE id = $1`, sess2["id"]); err != nil {
		t.Fatalf("corrupt usage: %v", err)
	}
	status, body = s.do(http.MethodGet, "/v1/sessions/"+sess2["id"].(string), nil)
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")
}

func mustCleanAgent(t *testing.T, s *tserver) string {
	t.Helper()
	return createAgent(t, s, map[string]any{"name": "clean", "model": "m"})["id"].(string)
}

func mustCleanEnv(t *testing.T, s *tserver) string {
	t.Helper()
	return createEnvironment(t, s, map[string]any{"name": "clean"})["id"].(string)
}

// TestDatabaseFailuresSurfaceAsAPIError drives the mid-transaction error
// branches by removing a table the write path needs.
func TestDatabaseFailuresSurfaceAsAPIError(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	agent := createAgent(t, s, map[string]any{"name": "d", "model": "m"})
	agentID := agent["id"].(string)

	if _, err := s.pool.Exec(ctx, `DROP TABLE agent_versions CASCADE`); err != nil {
		t.Fatalf("drop agent_versions: %v", err)
	}
	// Create and update both snapshot into agent_versions; versions list reads it.
	status, body := s.do(http.MethodPost, "/v1/agents", map[string]any{"name": "x", "model": "m"})
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")
	status, body = s.do(http.MethodPost, "/v1/agents/"+agentID, map[string]any{"version": 1, "name": "y"})
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")
	status, body = s.do(http.MethodGet, "/v1/agents/"+agentID+"/versions", nil)
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")
}

// TestAuthDatabaseFailure: a broken pool must yield the 500 envelope from the
// auth middleware, not a hung or leaked request.
func TestAuthDatabaseFailure(t *testing.T) {
	s := newTestServer(t)
	s.pool.Close()
	status, body := s.do(http.MethodGet, "/v1/agents", nil)
	wantErr(t, status, body, http.StatusInternalServerError, "api_error")
}
