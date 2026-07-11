package api_test

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// environmentRequiredFields is the BetaEnvironment wire surface (all
// api:"required"). Note: no "state" field — lifecycle is archived_at only.
var environmentRequiredFields = []string{
	"id", "type", "name", "description", "config", "metadata",
	"created_at", "updated_at", "archived_at",
}

func createEnvironment(t *testing.T, s *tserver, body map[string]any) map[string]any {
	t.Helper()
	status, res := s.do(http.MethodPost, "/v1/environments", body)
	if status != http.StatusOK {
		t.Fatalf("create environment: status %d, body %v", status, res)
	}
	return res
}

var emptyPackages = map[string]any{
	"apt": []any{}, "cargo": []any{}, "gem": []any{},
	"go": []any{}, "npm": []any{}, "pip": []any{},
}

func TestEnvironmentCreateMinimalDefaultsToCloud(t *testing.T) {
	s := newTestServer(t)
	res := createEnvironment(t, s, map[string]any{"name": "dev"})

	wantFields(t, res, environmentRequiredFields...)
	id, _ := res["id"].(string)
	if len(id) < 4 || id[:4] != "env_" {
		t.Errorf("id = %q, want env_ prefix", id)
	}
	if res["type"] != "environment" {
		t.Errorf(`type = %v, want "environment"`, res["type"])
	}
	if _, hasState := res["state"]; hasState {
		t.Errorf(`response leaks non-wire "state" field: %v`, res)
	}
	if res["scope"] != "organization" {
		t.Errorf(`scope = %v, want "organization" (single-tenant v1)`, res["scope"])
	}
	cfg, _ := res["config"].(map[string]any)
	if cfg["type"] != "cloud" {
		t.Fatalf("default config = %v, want cloud", res["config"])
	}
	if nw, _ := cfg["networking"].(map[string]any); nw["type"] != "unrestricted" {
		t.Errorf("default networking = %v, want unrestricted", cfg["networking"])
	}
	if !reflect.DeepEqual(cfg["packages"], emptyPackages) {
		t.Errorf("default packages = %v, want all six empty lists", cfg["packages"])
	}
	if res["archived_at"] != nil {
		t.Errorf("archived_at = %v, want null", res["archived_at"])
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

func TestEnvironmentCreateSelfHostedAndLimitedCloud(t *testing.T) {
	s := newTestServer(t)

	sh := createEnvironment(t, s, map[string]any{
		"name":   "byoc",
		"config": map[string]any{"type": "self_hosted"},
	})
	if cfg, _ := sh["config"].(map[string]any); !reflect.DeepEqual(cfg, map[string]any{"type": "self_hosted"}) {
		t.Errorf("self_hosted config = %v", sh["config"])
	}

	lim := createEnvironment(t, s, map[string]any{
		"name":        "locked",
		"description": "restricted egress",
		"config": map[string]any{
			"type": "cloud",
			"networking": map[string]any{
				"type":          "limited",
				"allowed_hosts": []any{"api.example.com", "*.internal.example.com"},
			},
			"packages": map[string]any{"pip": []any{"requests==2.32.0"}},
		},
		"metadata": map[string]any{"env": "prod"},
	})
	cfg, _ := lim["config"].(map[string]any)
	nw, _ := cfg["networking"].(map[string]any)
	if nw["type"] != "limited" {
		t.Fatalf("networking = %v", cfg["networking"])
	}
	// Required wire fields of a limited network are always present.
	wantFields(t, nw, "allowed_hosts", "allow_mcp_servers", "allow_package_managers")
	if nw["allow_mcp_servers"] != false {
		t.Errorf("allow_mcp_servers default = %v, want false", nw["allow_mcp_servers"])
	}
	pkgs, _ := cfg["packages"].(map[string]any)
	wantFields(t, pkgs, "apt", "cargo", "gem", "go", "npm", "pip")
	if pip, _ := pkgs["pip"].([]any); len(pip) != 1 || pip[0] != "requests==2.32.0" {
		t.Errorf("pip packages = %v", pkgs["pip"])
	}
	if lim["description"] != "restricted egress" {
		t.Errorf("description = %v", lim["description"])
	}
}

func TestEnvironmentCreateValidation(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		name string
		body any
	}{
		{"missing name", map[string]any{}},
		{"bad config type", map[string]any{"name": "x", "config": map[string]any{"type": "orbital"}}},
		{"bad networking type", map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "networking": map[string]any{"type": "mesh"}}}},
		{"unknown package manager", map[string]any{"name": "x", "config": map[string]any{"type": "cloud", "packages": map[string]any{"brew": []any{"jq"}}}}},
		{"account scope unsupported", map[string]any{"name": "x", "scope": "account"}},
		{"malformed json", `{`},
	}
	for _, tc := range cases {
		status, body := s.do(http.MethodPost, "/v1/environments", tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%v)", tc.name, status, body)
			continue
		}
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}
}

func TestEnvironmentGetUpdate(t *testing.T) {
	s := newTestServer(t)
	created := createEnvironment(t, s, map[string]any{
		"name": "u", "description": "before",
		"metadata": map[string]any{"keep": "1", "drop": "2"},
	})
	id := created["id"].(string)

	status, got := s.do(http.MethodGet, "/v1/environments/"+id, nil)
	if status != http.StatusOK || got["id"] != id {
		t.Fatalf("get: %d %v", status, got)
	}
	status, body := s.do(http.MethodGet, "/v1/environments/env_missing", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")

	// Update: no optimistic version; patch name/description/config/metadata.
	// The config update stays within the environment's kind (cloud) — kind is
	// immutable after creation (see TestEnvironmentKindIsImmutable).
	status, updated := s.do(http.MethodPost, "/v1/environments/"+id, map[string]any{
		"name":        "renamed",
		"description": "after",
		"config":      map[string]any{"type": "cloud", "networking": map[string]any{"type": "limited", "allowed_hosts": []any{"internal.corp"}}},
		"metadata":    map[string]any{"drop": nil, "new": "3"},
	})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, updated)
	}
	if updated["name"] != "renamed" || updated["description"] != "after" {
		t.Errorf("name/description = %v/%v", updated["name"], updated["description"])
	}
	cfg, _ := updated["config"].(map[string]any)
	if cfg["type"] != "cloud" {
		t.Errorf("config = %v", updated["config"])
	}
	if net, _ := cfg["networking"].(map[string]any); net["type"] != "limited" {
		t.Errorf("config networking not updated: %v", updated["config"])
	}
	if md, _ := updated["metadata"].(map[string]any); !reflect.DeepEqual(md, map[string]any{"keep": "1", "new": "3"}) {
		t.Errorf("metadata = %v", updated["metadata"])
	}

	// Environments alone also delete on empty string (the SDK's
	// map[string]string metadata cannot express null).
	status, updated = s.do(http.MethodPost, "/v1/environments/"+id, map[string]any{
		"metadata": map[string]any{"keep": ""},
	})
	if status != http.StatusOK {
		t.Fatalf("empty-string delete: %d", status)
	}
	if md, _ := updated["metadata"].(map[string]any); !reflect.DeepEqual(md, map[string]any{"new": "3"}) {
		t.Errorf(`metadata after empty-string delete = %v, want {"new":"3"}`, updated["metadata"])
	}

	status, body = s.do(http.MethodPost, "/v1/environments/env_missing", map[string]any{"name": "x"})
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}

func TestEnvironmentListPaginationAndArchive(t *testing.T) {
	s := newTestServer(t)
	e1 := createEnvironment(t, s, map[string]any{"name": "e1"})
	e2 := createEnvironment(t, s, map[string]any{"name": "e2"})
	e3 := createEnvironment(t, s, map[string]any{"name": "e3"})

	status, page1 := s.do(http.MethodGet, "/v1/environments?limit=2", nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d", status)
	}
	d1 := listData(t, page1)
	if len(d1) != 2 || d1[0]["id"] != e3["id"] || d1[1]["id"] != e2["id"] {
		t.Errorf("page 1 = %v, want e3,e2", d1)
	}
	status, page2 := s.do(http.MethodGet, "/v1/environments?limit=2&page="+nextPage(t, page1), nil)
	if status != http.StatusOK {
		t.Fatalf("page 2: %d", status)
	}
	if d2 := listData(t, page2); len(d2) != 1 || d2[0]["id"] != e1["id"] {
		t.Errorf("page 2 = %v, want e1", d2)
	}

	id := e2["id"].(string)
	status, archived := s.do(http.MethodPost, "/v1/environments/"+id+"/archive", nil)
	if status != http.StatusOK {
		t.Fatalf("archive: %d", status)
	}
	if ts, _ := archived["archived_at"].(string); ts == "" {
		t.Fatalf("archived_at = %v", archived["archived_at"])
	}
	_, list := s.do(http.MethodGet, "/v1/environments", nil)
	if entries := listData(t, list); len(entries) != 2 {
		t.Errorf("default list = %d entries, want 2 (archived hidden)", len(entries))
	}
	_, list = s.do(http.MethodGet, "/v1/environments?include_archived=true", nil)
	if entries := listData(t, list); len(entries) != 3 {
		t.Errorf("include_archived = %d entries, want 3", len(entries))
	}
	status, body := s.do(http.MethodPost, "/v1/environments/env_missing/archive", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}

func TestEnvironmentDelete(t *testing.T) {
	s := newTestServer(t)
	env := createEnvironment(t, s, map[string]any{"name": "gone"})
	id := env["id"].(string)

	status, res := s.do(http.MethodDelete, "/v1/environments/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, res)
	}
	if res["id"] != id || res["type"] != "environment_deleted" {
		t.Errorf("delete response = %v", res)
	}
	status, body := s.do(http.MethodGet, "/v1/environments/"+id, nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	status, body = s.do(http.MethodDelete, "/v1/environments/env_missing", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}

func TestEnvironmentDeleteBlockedBySessions(t *testing.T) {
	s := newTestServer(t)
	agent := createAgent(t, s, map[string]any{"name": "a", "model": "m"})
	env := createEnvironment(t, s, map[string]any{"name": "busy"})
	status, sess := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agent["id"], "environment_id": env["id"],
	})
	if status != http.StatusOK {
		t.Fatalf("create session: %d %v", status, sess)
	}

	status, body := s.do(http.MethodDelete, "/v1/environments/"+env["id"].(string), nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// Still there.
	status, _ = s.do(http.MethodGet, "/v1/environments/"+env["id"].(string), nil)
	if status != http.StatusOK {
		t.Fatalf("environment vanished after failed delete: %d", status)
	}
}

// TestEnvironmentKindIsImmutable pins that an environment's cloud/self_hosted
// kind is fixed at creation: a config update that flips the kind is rejected.
// The queue routes work by kind (the executor claims cloud tool_exec, a BYOC
// worker polls self_hosted), so a mid-life switch could hand one item to both.
func TestEnvironmentKindIsImmutable(t *testing.T) {
	s := newTestServer(t)

	cloud := createEnvironment(t, s, map[string]any{"name": "c", "config": map[string]any{"type": "cloud"}})
	status, body := s.do(http.MethodPost, "/v1/environments/"+cloud["id"].(string),
		map[string]any{"config": map[string]any{"type": "self_hosted"}})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	self := createEnvironment(t, s, map[string]any{"name": "s", "config": map[string]any{"type": "self_hosted"}})
	status, body = s.do(http.MethodPost, "/v1/environments/"+self["id"].(string),
		map[string]any{"config": map[string]any{"type": "cloud"}})
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// A same-kind config update still works (kind unchanged).
	status, _ = s.do(http.MethodPost, "/v1/environments/"+self["id"].(string),
		map[string]any{"config": map[string]any{"type": "self_hosted"}})
	if status != http.StatusOK {
		t.Errorf("same-kind config update rejected: status %d", status)
	}
}
