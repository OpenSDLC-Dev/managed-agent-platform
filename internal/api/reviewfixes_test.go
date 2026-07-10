package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// TestArchivedResourcesAreImmutable: archive means read-only — update paths
// must reject archived agents, environments, and sessions just as the create
// paths already reject archived referents.
func TestArchivedResourcesAreImmutable(t *testing.T) {
	s := newTestServer(t)
	agent := createAgent(t, s, map[string]any{"name": "frozen", "model": "m"})
	agentID := agent["id"].(string)
	env := createEnvironment(t, s, map[string]any{"name": "frozen-env"})
	envID := env["id"].(string)
	sess := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	sessID := sess["id"].(string)

	for _, path := range []string{
		"/v1/sessions/" + sessID + "/archive",
		"/v1/agents/" + agentID + "/archive",
		"/v1/environments/" + envID + "/archive",
	} {
		if status, body := s.do(http.MethodPost, path, nil); status != http.StatusOK {
			t.Fatalf("archive %s: %d %v", path, status, body)
		}
	}

	for name, tc := range map[string]struct {
		path string
		body map[string]any
	}{
		"agent update":       {"/v1/agents/" + agentID, map[string]any{"version": 1, "name": "thaw"}},
		"environment update": {"/v1/environments/" + envID, map[string]any{"name": "thaw"}},
		"session update":     {"/v1/sessions/" + sessID, map[string]any{"title": "thaw"}},
	} {
		status, body := s.do(http.MethodPost, tc.path, tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s on archived resource: status %d, want 400 (%v)", name, status, body)
			continue
		}
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}

	// The frozen agent grew no new version snapshot.
	_, versions := s.do(http.MethodGet, "/v1/agents/"+agentID+"/versions", nil)
	if entries := listData(t, versions); len(entries) != 1 {
		t.Errorf("archived agent versions = %d, want 1", len(entries))
	}
}

// TestEnvironmentConfigUpdatePreservesOmittedFields: per the reference's
// update semantics, omitted cloud sub-fields keep their existing values — a
// packages-only update must not silently reset limited networking to
// unrestricted.
func TestEnvironmentConfigUpdatePreservesOmittedFields(t *testing.T) {
	s := newTestServer(t)
	env := createEnvironment(t, s, map[string]any{
		"name": "locked",
		"config": map[string]any{
			"type": "cloud",
			"networking": map[string]any{
				"type":          "limited",
				"allowed_hosts": []any{"internal.corp"},
			},
			"packages": map[string]any{"pip": []any{"requests"}},
		},
	})
	id := env["id"].(string)

	// Packages-only update: networking and the pip list must survive.
	status, updated := s.do(http.MethodPost, "/v1/environments/"+id, map[string]any{
		"config": map[string]any{"type": "cloud", "packages": map[string]any{"npm": []any{"left-pad"}}},
	})
	if status != http.StatusOK {
		t.Fatalf("packages-only update: %d %v", status, updated)
	}
	cfg, _ := updated["config"].(map[string]any)
	nw, _ := cfg["networking"].(map[string]any)
	if nw["type"] != "limited" {
		t.Fatalf("networking reset by packages-only update: %v", cfg["networking"])
	}
	hosts, _ := nw["allowed_hosts"].([]any)
	if len(hosts) != 1 || hosts[0] != "internal.corp" {
		t.Errorf("allowed_hosts lost: %v", nw["allowed_hosts"])
	}
	pkgs, _ := cfg["packages"].(map[string]any)
	if pip, _ := pkgs["pip"].([]any); len(pip) != 1 {
		t.Errorf("pip list lost on npm-only patch: %v", pkgs["pip"])
	}
	if npm, _ := pkgs["npm"].([]any); len(npm) != 1 {
		t.Errorf("npm list not applied: %v", pkgs["npm"])
	}

	// Field-level networking patch: flipping one flag keeps allowed_hosts.
	status, updated = s.do(http.MethodPost, "/v1/environments/"+id, map[string]any{
		"config": map[string]any{"type": "cloud", "networking": map[string]any{
			"type": "limited", "allow_mcp_servers": true,
		}},
	})
	if status != http.StatusOK {
		t.Fatalf("networking flag update: %d %v", status, updated)
	}
	cfg, _ = updated["config"].(map[string]any)
	nw, _ = cfg["networking"].(map[string]any)
	if nw["allow_mcp_servers"] != true {
		t.Errorf("allow_mcp_servers not applied: %v", nw)
	}
	if hosts, _ := nw["allowed_hosts"].([]any); len(hosts) != 1 {
		t.Errorf("allowed_hosts lost on flag-only patch: %v", nw["allowed_hosts"])
	}

	// Switching the networking type starts from that type's defaults.
	status, updated = s.do(http.MethodPost, "/v1/environments/"+id, map[string]any{
		"config": map[string]any{"type": "cloud", "networking": map[string]any{"type": "unrestricted"}},
	})
	if status != http.StatusOK {
		t.Fatalf("type switch: %d", status)
	}
	cfg, _ = updated["config"].(map[string]any)
	if nw, _ := cfg["networking"].(map[string]any); nw["type"] != "unrestricted" || len(nw) != 1 {
		t.Errorf("unrestricted switch = %v", cfg["networking"])
	}
}

// TestKeysetPaginationSurvivesConcurrentInsert: a cursor taken before a new
// row lands must neither repeat page-1 rows nor skip older rows (the offset
// pagination this replaced failed exactly this way).
func TestKeysetPaginationSurvivesConcurrentInsert(t *testing.T) {
	s := newTestServer(t)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, createAgent(t, s, map[string]any{"name": "k", "model": "m"})["id"].(string))
	}
	status, page1 := s.do(http.MethodGet, "/v1/agents?limit=2", nil)
	if status != http.StatusOK {
		t.Fatalf("page 1: %d", status)
	}
	cursor := nextPage(t, page1)

	// A newer agent lands between page fetches.
	createAgent(t, s, map[string]any{"name": "newcomer", "model": "m"})

	status, page2 := s.do(http.MethodGet, "/v1/agents?limit=2&page="+cursor, nil)
	if status != http.StatusOK {
		t.Fatalf("page 2: %d", status)
	}
	d2 := listData(t, page2)
	if len(d2) != 1 || d2[0]["id"] != ids[0] {
		t.Errorf("page 2 = %v, want exactly the oldest agent %s (no duplicates, no skips)", d2, ids[0])
	}
}

func TestOversizeBodyReturns413(t *testing.T) {
	s := newTestServer(t)
	huge := `{"name":"x","model":"m","system":"` + strings.Repeat("a", 4<<20) + `"}`
	status, body := s.do(http.MethodPost, "/v1/agents", huge)
	wantErr(t, status, body, http.StatusRequestEntityTooLarge, "request_too_large")
}

// TestSessionListIgnoresAgentVersionWithoutAgentID: the reference documents
// agent_version as "only applies when agent_id is also set" — ignored, not
// rejected.
func TestSessionListIgnoresAgentVersionWithoutAgentID(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})

	status, list := s.do(http.MethodGet, "/v1/sessions?agent_version=42", nil)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 (parameter ignored)", status)
	}
	if entries := listData(t, list); len(entries) != 1 {
		t.Errorf("agent_version without agent_id filtered the list: %v", entries)
	}
}

// TestBootstrapKeyRotation: ensuring a new key under the same name revokes
// the previous one — a leaked key must die on rotation, not live forever.
func TestBootstrapKeyRotation(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	const rotated = "rotated-bootstrap-key"
	if err := api.EnsureAPIKey(ctx, s.pool, "test", rotated); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	res := s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"x-api-key": testKey})
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("old key after rotation: %d, want 401", res.StatusCode)
	}
	res = s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"x-api-key": rotated})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("new key after rotation: %d, want 200", res.StatusCode)
	}

	// Rotating back un-revokes the original hash and kills the interim key.
	if err := api.EnsureAPIKey(ctx, s.pool, "test", testKey); err != nil {
		t.Fatalf("rotate back: %v", err)
	}
	res = s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"x-api-key": testKey})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("original key after rotate-back: %d, want 200", res.StatusCode)
	}
	res = s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"x-api-key": rotated})
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("interim key after rotate-back: %d, want 401", res.StatusCode)
	}

	// A broken pool surfaces as an error, not a silent no-op.
	s.pool.Close()
	if err := api.EnsureAPIKey(ctx, s.pool, "test", "another"); err == nil {
		t.Error("EnsureAPIKey on a closed pool should fail")
	}
}
