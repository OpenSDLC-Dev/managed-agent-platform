package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestXShouldRetryFollowsTheRecordings pins `x-should-retry: false` to the
// refusals the reference was recorded answering with it, where this platform
// answers the same status (#842), and its absence from those recorded without
// it. Every case cites the recording; the console namespace takes it on every
// refusal, as every refusal recorded there carries it, and a helper whose every
// recorded answer carries it takes it on the inputs it shares
// (docs/DIVERGENCES.md lists both).
func TestXShouldRetryFollowsTheRecordings(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	shEnv, _, shKey := selfHostedWorker(t, s, "noretry")
	cloudKey := issueKey(t, s.pool, envID, "cloud")
	store := createMemoryStore(t, s, "noretry")
	mem := createMemory(t, s, store, "/x/y/z.md", "first")
	fileID := uploadOneFile(t, s, "a.txt")
	skill := s.createSkill(t)
	skillID, _ := skill["id"].(string)
	versionID, _ := skill["latest_version_id"].(string)
	const absentEnv = "env_0000000000000000000000gk"

	mgmt := map[string]string{"x-api-key": testKey}
	bearer := func(key string) map[string]string { return map[string]string{"Authorization": "Bearer " + key} }
	ct, noFiles := skillFormRaw(t, nil, [2]string{"display_name", "no files"})

	for _, tc := range []struct {
		name, method, path string
		body               any
		headers            map[string]string
		status             int
		noRetry            bool
	}{
		// 2026-09-05-dreams batch1 `rec91.control.nonsense-path`.
		{"an unknown path", http.MethodGet, "/v1/nonsense_route_does_not_exist", nil, mgmt, 404, true},
		// 2026-09-02 free_batch1 `memver.list.limit5`.
		{"an unknown memory subpath", http.MethodGet, "/v1/memory_stores/" + store + "/memories/" + mem["id"].(string) + "/memory_versions", nil, mgmt, 404, true},
		// 2026-09-19-self-hosted-docker worker-network idx 0.
		{"no key", http.MethodGet, "/v1/agents", nil, nil, 401, true},
		// 2026-09-03 batch1 `env.create.bogus-probe`, `env.create.cloud`; 2026-09-05 batch8 `probe.env.kind-enum`.
		{"an environment without a name", http.MethodPost, "/v1/environments", map[string]any{}, mgmt, 400, true},
		{"an environment's extra key", http.MethodPost, "/v1/environments", map[string]any{"name": "x", "kind": "cloud"}, mgmt, 400, true},
		{"an environment's unknown tag", http.MethodPost, "/v1/environments", map[string]any{"name": "x", "config": map[string]any{"type": "bogus"}}, mgmt, 400, true},
		// 2026-09-05 batch1 `rec82.env.delete.environments-beta`; 2026-09-02 batch2 `env.archive.with-deployment`.
		{"deleting an absent environment", http.MethodDelete, "/v1/environments/" + absentEnv, nil, mgmt, 404, true},
		{"deleting a malformed environment id", http.MethodDelete, "/v1/environments/not-an-id", nil, mgmt, 404, true},
		{"archiving an absent environment", http.MethodPost, "/v1/environments/" + absentEnv + "/archive", nil, mgmt, 404, true},
		// 2026-09-02 batch2 `file.get.after-delete`, `file.content.download`; 2026-09-03 batch2 `envkey.files.content-original`.
		{"an absent file", http.MethodGet, "/v1/files/file_0000000000000000000000gk", nil, mgmt, 404, true},
		{"an upload's content", http.MethodGet, "/v1/files/" + fileID + "/content", nil, mgmt, 400, true},
		{"a cloud key's file content", http.MethodGet, "/v1/files/" + fileID + "/content", nil, bearer(cloudKey), 404, true},
		// 2026-09-02 free_batch1 `mem.create.at-occupied`; 2026-09-03 batch1 `memory.update.precondition-wrong-sha`.
		{"an occupied memory path", http.MethodPost, "/v1/memory_stores/" + store + "/memories", map[string]any{"path": "/x/y/z.md", "content": "again"}, mgmt, 409, true},
		{"a stale memory precondition", http.MethodPost, "/v1/memory_stores/" + store + "/memories/" + mem["id"].(string),
			map[string]any{"content": "b", "precondition": map[string]any{"type": "content_sha256", "content_sha256": strings.Repeat("0", 64)}}, mgmt, 409, true},
		// 2026-09-05-dreams batch1 `rec91.betaguard.skills.limit0.nobeta`; 2026-09-04 batch1 #80-#83, #66.
		{"a skills limit of 0", http.MethodGet, "/v1/skills?limit=0", nil, mgmt, 400, true},
		{"an absent skill", http.MethodGet, "/v1/skills/skill_0000000000000000000000gk", nil, mgmt, 404, true},
		{"a skill's only version", http.MethodDelete, "/v1/skills/" + skillID + "/versions/" + versionID, nil, mgmt, 400, true},
		// 2026-09-03 batch2 idx 4 `envkey.skills.collection-list`; 2026-09-05 batch2 #38, #32; 2026-09-03 batch2 `envkey.dead-key-verify`.
		{"an environment key's skills list", http.MethodGet, "/v1/skills", nil, bearer(shKey), 403, true},
		{"a cloud key's poll", http.MethodGet, "/v1/environments/" + envID + "/work/poll", nil, bearer(cloudKey), 400, true},
		{"a key's poll elsewhere", http.MethodGet, "/v1/environments/" + envID + "/work/poll", nil, bearer(shKey), 403, true},
		{"a dead key's work list", http.MethodGet, "/v1/environments/" + shEnv + "/work", nil, bearer("not-a-key"), 401, true},
		// The console namespace: 2026-09-05 batch2 `rec83.edge5.list.limit.0`, and an unknown path there.
		{"a console list's limit of 0", http.MethodGet, consoleTokens(envID) + "?limit=0", nil, mgmt, 400, true},
		{"an unknown console path", http.MethodGet, "/api/oauth/organizations/default/environments/" + envID + "/secrets", nil, mgmt, 404, true},

		// Recorded without it: 2026-09-05-dreams `rec91.control.dreams.extra-segment`, 2026-09-03 batch2 idx 5
		// `envkey.agents.list-should-refuse`, 2026-09-02 batch2 `agent.update.stale-version`, #160's lane for
		// session create; and an environment's own 404 on a route no recording reaches.
		{"an unknown dream subpath", http.MethodGet, "/v1/dreams/drm_0000000000000000000000gk/bogus", nil, mgmt, 404, false},
		{"an environment key's agents list", http.MethodGet, "/v1/agents", nil, bearer(shKey), 401, false},
		{"a stale agent version", http.MethodPost, "/v1/agents/" + agentID, map[string]any{"version": 7, "name": "renamed"}, mgmt, 409, false},
		{"a session in an absent environment", http.MethodPost, "/v1/sessions", map[string]any{"agent": agentID, "environment_id": absentEnv}, mgmt, 404, false},
		{"reading an absent environment", http.MethodGet, "/v1/environments/" + absentEnv, nil, mgmt, 404, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := s.doRaw(tc.method, tc.path, tc.body, tc.headers)
			raw, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != tc.status {
				t.Fatalf("status %d, want %d (%s)", res.StatusCode, tc.status, raw)
			}
			got := res.Header.Get("x-should-retry")
			if tc.noRetry && got != "false" {
				t.Errorf("x-should-retry = %q, want \"false\" (%s)", got, raw)
			}
			if !tc.noRetry && got != "" {
				t.Errorf("x-should-retry = %q, want none (%s)", got, raw)
			}
		})
	}

	// The header leaves the path conflict's own members in its body.
	res := s.doRaw(http.MethodPost, "/v1/memory_stores/"+store+"/memories", map[string]any{"path": "/x/y/z.md", "content": "again"}, mgmt)
	var body struct {
		Error map[string]any `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if body.Error["conflicting_memory_id"] != mem["id"] || body.Error["conflicting_path"] != "/x/y/z.md" ||
		body.Error["type"] != "memory_path_conflict_error" || res.Header.Get("x-should-retry") != "false" {
		t.Errorf("path conflict = %v (x-should-retry %q), want its members and the header", body.Error, res.Header.Get("x-should-retry"))
	}

	// A skill upload's refusal, which takes a form body.
	res = s.doRaw(http.MethodPost, "/v1/skills", noFiles, map[string]string{"x-api-key": testKey, "Content-Type": ct})
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest || res.Header.Get("x-should-retry") != "false" {
		t.Errorf("an upload without files[]: %d, x-should-retry %q; want 400 and \"false\" (2026-09-04 batch1 #11)",
			res.StatusCode, res.Header.Get("x-should-retry"))
	}
}
