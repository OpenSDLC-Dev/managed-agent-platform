package api_test

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/clioauth"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
)

func TestTwoWorkspaceClients(t *testing.T) {
	a := newTestServer(t)
	b := secondWorkspace(t, a)
	const research = "wrkspc_00000000000000000000000002"
	const resources = "/api/organizations/default/proxy/v1/agents"
	created := map[string]string{}
	for _, tc := range []struct {
		s    *tserver
		hint string
	}{{a, "default"}, {b, research}} {
		status, agent := tc.s.do("POST", "/v1/agents", agentBody(nil))
		if status != 200 {
			t.Fatalf("create agent: %d %v", status, agent)
		}
		created[tc.hint] = agent["id"].(string)
	}
	tab := func(hint, path string) (int, map[string]any) {
		headers := a.cookieHeaders()
		if hint != "" {
			headers["x-workspace-id"] = hint
		}
		return a.decode(a.doRaw("GET", path, nil, headers))
	}
	for hint, id := range created {
		status, account := tab(hint, "/api/auth/session")
		if status != 200 || account["workspace_id"] != hint || account["workspace_role"] != "admin" {
			t.Fatalf("session %s: %d %v", hint, status, account)
		}
		status, agents := tab(hint, resources)
		if status != 200 || len(listData(t, agents)) != 1 || listData(t, agents)[0]["id"] != id {
			t.Fatalf("agents %s: %d %v", hint, status, agents)
		}
	}
	if status, _ := tab("", resources); status != 400 {
		t.Errorf("ambiguous tab HTTP %d", status)
	}
	if status, _ := tab(research, "/api/console/organizations/default/workspaces/default/agents/overview"); status != 404 {
		t.Errorf("conflicting tab HTTP %d", status)
	}
	if status, ws := tab("", "/api/auth/workspaces"); status != 200 || len(listData(t, ws)) != 2 {
		t.Fatalf("navigation: %d %v", status, ws)
	}

	headers := a.cookieHeaders()
	headers["x-workspace-id"] = research
	path := "/api/console/organizations/default/workspaces/" + research + "/api_keys"
	status, k := a.decode(a.doRaw("POST", path, map[string]any{"name": "Research"}, headers))
	if status != 200 || k["workspace_id"] != research {
		t.Fatalf("issue workspace key: %d %v", status, k)
	}
	status, agents := a.decode(a.doRaw("GET", "/v1/agents", nil, map[string]string{"x-api-key": k["raw_key"].(string)}))
	if status != 200 || len(listData(t, agents)) != 1 || listData(t, agents)[0]["id"] != created[research] {
		t.Fatalf("issued key scope: %d", status)
	}

	tokens := issueCLITokens(t, a, "org:admin user:developer", research)
	grant := tokens["workspace"].(map[string]any)
	if grant["id"] != research {
		t.Fatalf("CLI workspace: %v", grant)
	}
	bearer := asBearer(tokens["access_token"].(string))
	for _, tc := range []struct {
		suffix string
		want   int
	}{{"", 1}, {"?include_default=true", 2}, {"?include_default=true&ids%5B%5D=" + workspace.DefaultPublicID, 1}, {"?include_default=true&ids%5B%5D=missing", 0}} {
		status, ws := a.decode(a.doRaw("GET", "/v1/organizations/workspaces"+tc.suffix, nil, bearer))
		if status != 200 || len(listData(t, ws)) != tc.want {
			t.Errorf("organization workspaces %s %d %v", tc.suffix, status, ws)
		}
	}
	status, keys := a.decode(a.doRaw("GET", auditKeysPath+"?workspace_id="+research, nil, bearer))
	if status != 200 || len(listData(t, keys)) != 2 {
		t.Fatalf("CLI scoped key audit %d %v", status, keys)
	}
	for _, row := range listData(t, keys) {
		if row["scope"].(map[string]any)["workspace_id"] != research {
			t.Errorf("public key workspace: %v", row["scope"])
		}
	}
	status, keys = a.decode(a.doRaw("GET", auditKeysPath+"?workspace_id="+workspace.DefaultPublicID+"&after_id="+k["id"].(string), nil, bearer))
	if status != 200 || len(listData(t, keys)) != 0 {
		t.Errorf("foreign audit cursor: %d %v", status, keys)
	}
	status, other := tab(research, "/api/organizations/default/proxy/v1/agents/"+created["default"])
	if status != http.StatusNotFound || strings.Contains(other["error"].(map[string]any)["message"].(string), "workspace") {
		t.Errorf("foreign resource: %d %v", status, other)
	}
}

func TestNonDefaultProfileGrantRequiresMembership(t *testing.T) {
	s := newTestServer(t)
	secondWorkspace(t, s)
	const b = "wrkspc_00000000000000000000000002"
	var actor string
	if err := s.pool.QueryRow(t.Context(), `SELECT id FROM principals WHERE kind='user' AND role='admin'`).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	profileA := issueCLITokens(t, s, "user:profile", workspace.DefaultPublicID)
	profileB := issueCLITokens(t, s, "user:profile", b)
	pendingB := consentCLI(t, s, "user:profile", b)
	if _, err := s.pool.Exec(t.Context(), `UPDATE principals SET role='user' WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	refresh := func(tokens map[string]any) int {
		values := url.Values{"grant_type": {"refresh_token"}, "client_id": {antClientID}, "refresh_token": {tokens["refresh_token"].(string)}}
		status, _ := s.decode(s.doRaw("POST", "/v1/oauth/token", values.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}))
		return status
	}
	if _, err := clioauth.Authenticate(t.Context(), s.pool, profileB["access_token"].(string)); !errors.Is(err, clioauth.ErrInvalidGrant) {
		t.Errorf("removed B profile token error %v", err)
	}
	if status, _ := s.decode(s.doRaw("POST", "/v1/oauth/token", pendingB.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})); status != 400 {
		t.Errorf("removed B profile exchange HTTP %d, want 400", status)
	}
	if status := refresh(profileB); status != 400 {
		t.Errorf("removed B profile refresh HTTP %d, want 400", status)
	}
	if status := refresh(profileA); status != 200 {
		t.Errorf("Default profile refresh HTTP %d, want 200", status)
	}
	h := s.cookieHeaders()
	h["x-workspace-id"] = b
	if status, _ := s.decode(s.doRaw("POST", "/api/auth/logout", nil, h)); status != 200 {
		t.Errorf("logout from removed workspace HTTP %d", status)
	}
	if status, _ := s.decode(s.doRaw("GET", "/api/auth/session", nil, s.cookieHeaders())); status != 401 {
		t.Errorf("revoked cookie HTTP %d", status)
	}
}
