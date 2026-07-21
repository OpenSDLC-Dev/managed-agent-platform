package api_test

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// TestSkillReadsEnvironmentKeyLane pins the dual-auth lane slice 4 adds: a
// BYOC worker materializes skills over the wire with the same Authorization:
// Bearer environment key it polls work with (the SDK's SetupSkills documents
// exactly this option set), so the skill read+download routes accept it.
// Everything mutating — and the collection list — stays management-only.
func TestSkillReadsEnvironmentKeyLane(t *testing.T) {
	s := newTestServer(t)
	_, envID := fixture(t, s)
	const wkey = "wkey_skills_lane"
	if err := api.EnsureEnvironmentKey(context.Background(), s.pool, envID, wkey); err != nil {
		t.Fatalf("EnsureEnvironmentKey: %v", err)
	}
	bearer := map[string]string{"Authorization": "Bearer " + wkey}

	created := s.createSkill(t)
	id, _ := created["id"].(string)
	version, _ := created["latest_version"].(string)

	// The four read routes serve an environment key.
	for _, path := range []string{
		"/v1/skills/" + id,
		"/v1/skills/" + id + "/versions",
		"/v1/skills/" + id + "/versions/" + version,
		"/v1/skills/" + id + "/versions/" + version + "/content",
	} {
		res := s.doRaw("GET", path, nil, bearer)
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("env-key GET %s = %d, want 200", path, res.StatusCode)
		}
	}

	// An invalid key is rejected; a valid x-api-key alongside a Bearer keeps
	// the management lane (the reference client never sends both).
	res := s.doRaw("GET", "/v1/skills/"+id, nil, map[string]string{"Authorization": "Bearer nope"})
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad env key = %d, want 401", res.StatusCode)
	}
	res = s.doRaw("GET", "/v1/skills/"+id, nil,
		map[string]string{"Authorization": "Bearer nope", "x-api-key": testKey})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("x-api-key alongside a Bearer = %d, want the management lane's 200", res.StatusCode)
	}

	// The collection list and every mutation stay management-only: an
	// environment key gets the management lane's 401, never the handler.
	for _, probe := range []struct{ method, path string }{
		{"GET", "/v1/skills"},
		{"POST", "/v1/skills"},
		{"DELETE", "/v1/skills/" + id},
		{"POST", "/v1/skills/" + id + "/versions"},
		{"DELETE", "/v1/skills/" + id + "/versions/" + version},
	} {
		res := s.doRaw(probe.method, probe.path, nil, bearer)
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("env-key %s %s = %d, want 401", probe.method, probe.path, res.StatusCode)
		}
	}
}
