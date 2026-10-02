package api_test

import (
	"io"
	"net/http"
	"testing"
)

// TestSkillReadsEnvironmentKeyLane pins the dual-auth lane slice 4 adds: a
// BYOC worker materializes skills over the wire with the same Authorization:
// Bearer environment key it polls work with (the SDK's SetupSkills documents
// exactly this option set), so the skill version read+download routes accept
// it. The skill itself and the collection list are management routes, where
// the key gets the reference's recorded 403 (skillsScopeMessage; #840), and
// every mutation gets the management lane's 401.
func TestSkillReadsEnvironmentKeyLane(t *testing.T) {
	s := newTestServer(t)
	_, envID := fixture(t, s)
	wkey := issueKey(t, s.pool, envID, "skills-lane")
	bearer := map[string]string{"Authorization": "Bearer " + wkey}

	created := s.createSkill(t)
	id, _ := created["id"].(string)
	version, _ := created["latest_version_id"].(string)

	// The three version read routes serve an environment key.
	for _, path := range []string{
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
	versions := "/v1/skills/" + id + "/versions"
	res := s.doRaw("GET", versions, nil, map[string]string{"Authorization": "Bearer nope"})
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad env key = %d, want 401", res.StatusCode)
	}
	res = s.doRaw("GET", versions, nil,
		map[string]string{"Authorization": "Bearer nope", "x-api-key": testKey})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("x-api-key alongside a Bearer = %d, want the management lane's 200", res.StatusCode)
	}

	// The skill itself and the collection list: the reference's 403, never
	// the handler (2026-09-03 batch2 `envkey.skills.get-anthropic`,
	// `envkey.skills.collection-list`).
	for _, path := range []string{"/v1/skills/" + id, "/v1/skills"} {
		status, body := readJSON(t, s.doRaw("GET", path, nil, bearer))
		wantErrMsg(t, status, body, http.StatusForbidden, "permission_error", skillsScopeMessage)
	}

	// Every mutation stays management-only: an environment key gets the
	// management lane's 401, never the handler.
	for _, probe := range []struct{ method, path string }{
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
