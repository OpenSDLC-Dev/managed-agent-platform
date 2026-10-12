package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// skillsRootRefusal is the refusal of a repository mounted where the
// executor lands the session's skills.
func skillsRootRefusal(mount string) string {
	return `mount_path "` + mount + `" is reserved: the agent's skills are materialized under /workspace/skills`
}

// skilledAgent creates an agent carrying one custom skill reference — create
// checks no custom reference (docs/plan/06_skills.md decision 7), so it need
// not exist.
func skilledAgent(t *testing.T, s *tserver, name string) string {
	t.Helper()
	return createAgent(t, s, map[string]any{"name": name, "model": "claude-opus-4-8",
		"skills": []any{map[string]any{"type": "custom", "skill_id": "skill_01DoesNotExistXXXXXXXXXXX"}}})["id"].(string)
}

// skillsRepo is a repository named `skills` — github.com/anthropics/skills is
// a real one — whose default mount is /workspace/skills.
func skillsRepo(extra map[string]any) map[string]any {
	obj := map[string]any{"type": "github_repository", "url": "https://github.com/anthropics/skills",
		"authorization_token": "ghp_skills"}
	for k, v := range extra {
		obj[k] = v
	}
	return obj
}

// A repository may mount on the skills root only when the session has no
// skills to land there: the reference was recorded cloning anthropics/skills
// to /workspace/skills in a skill-free session. Once any agent of the session
// carries a skill — its own, an override's, or a roster member's — the root
// and everything below it are refused at create. /workspace itself, the
// sandbox workdir, is refused whatever the agent carries.
func TestSkillsRootRepositoryNeedsASkillFreeSession(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	plain, envID := fixture(t, s)
	skilled := skilledAgent(t, s, "skilled")

	sess := createSession(t, s, map[string]any{"agent": plain, "environment_id": envID,
		"resources": []any{skillsRepo(nil)}})
	if got := resourcesOf(t, sess)[0]["mount_path"]; got != "/workspace/skills" {
		t.Errorf("skill-free session mount_path = %v, want /workspace/skills", got)
	}

	member := skilledAgent(t, s, "member")
	coordinator := createAgent(t, s, map[string]any{"name": "coordinator", "model": "claude-opus-4-8",
		"multiagent": map[string]any{"type": "coordinator", "agents": []any{member}}})["id"].(string)
	override := map[string]any{"type": "agent_with_overrides", "id": plain,
		"skills": []any{map[string]any{"type": "custom", "skill_id": "skill_01DoesNotExistXXXXXXXXXXX"}}}

	for _, tc := range []struct {
		name  string
		agent any
		repo  map[string]any
		mount string
	}{
		{"own skill, default mount", skilled, skillsRepo(nil), "/workspace/skills"},
		{"own skill, a skill's directory", skilled,
			skillsRepo(map[string]any{"mount_path": "/workspace/skills/pdf"}), "/workspace/skills/pdf"},
		{"override skill", override, skillsRepo(nil), "/workspace/skills"},
		{"roster member's skill", coordinator, skillsRepo(nil), "/workspace/skills"},
	} {
		status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{
			"agent": tc.agent, "environment_id": envID, "resources": []any{tc.repo}})
		if status != http.StatusBadRequest || errMessage(body) != skillsRootRefusal(tc.mount) {
			t.Errorf("%s: status %d, message %q; want 400 %q", tc.name, status, errMessage(body), skillsRootRefusal(tc.mount))
		}
	}
	if got := resourceMutationCount(t, collect(), "invalid"); got != 4 {
		t.Errorf("session.resources{invalid} = %d, want 4 for the four refused creates", got)
	}

	// A sibling whose name merely starts with "skills" is not below the root.
	createSession(t, s, map[string]any{"agent": skilled, "environment_id": envID,
		"resources": []any{skillsRepo(map[string]any{"mount_path": "/workspace/skillset"})}})

	for _, agent := range []string{plain, skilled} {
		status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{"agent": agent, "environment_id": envID,
			"resources": []any{skillsRepo(map[string]any{"mount_path": "/workspace"})}})
		wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error", `mount_path "/workspace" is reserved`)
	}
}

// A deployment answers the same refusal on create and update, judged against
// the agent version it pins: bringing the repository to a skilled agent, or
// re-pinning a repository-bearing deployment to one.
func TestSkillsRootRepositoryDeploymentNeedsASkillFreeAgent(t *testing.T) {
	s := newTestServer(t)
	plain, envID := fixture(t, s)
	skilled := skilledAgent(t, s, "skilled")

	body := deploymentBody(skilled, envID)
	body["resources"] = []any{skillsRepo(nil)}
	status, res := s.do(http.MethodPost, "/v1/deployments", body)
	wantErrMsg(t, status, res, http.StatusBadRequest, "invalid_request_error", skillsRootRefusal("/workspace/skills"))

	bare := createDeployment(t, s, deploymentBody(skilled, envID))["id"].(string)
	status, res = s.do(http.MethodPost, "/v1/deployments/"+bare, map[string]any{"resources": []any{skillsRepo(nil)}})
	wantErrMsg(t, status, res, http.StatusBadRequest, "invalid_request_error", skillsRootRefusal("/workspace/skills"))

	withRepo := deploymentBody(plain, envID)
	withRepo["resources"] = []any{skillsRepo(nil)}
	repoed := createDeployment(t, s, withRepo)["id"].(string)
	status, res = s.do(http.MethodPost, "/v1/deployments/"+repoed, map[string]any{"agent": skilled})
	wantErrMsg(t, status, res, http.StatusBadRequest, "invalid_request_error", skillsRootRefusal("/workspace/skills"))

	if status, res := s.do(http.MethodPost, "/v1/deployments/"+repoed, map[string]any{"name": "renamed"}); status != http.StatusOK {
		t.Errorf("an update touching neither agent nor resources: status %d (%v), want 200", status, res)
	}

	status, res = s.do(http.MethodPost, "/v1/deployments", map[string]any{
		"name": "workdir", "agent": plain, "environment_id": envID,
		"initial_events": deploymentBody(plain, envID)["initial_events"],
		"resources":      []any{skillsRepo(map[string]any{"mount_path": "/workspace"})}})
	wantErrMsg(t, status, res, http.StatusBadRequest, "invalid_request_error", `mount_path "/workspace" is reserved`)
}

// A deployment stored before the refusal fails its fire as
// session_creation_rejected_error, the reference's type for a non-retryable
// validation error, which pauses nothing: the schedule keeps firing and each
// fire records the failure.
func TestSkillsRootRepositoryDeploymentStoredEarlierFailsItsFire(t *testing.T) {
	s := newTestServer(t)
	plain, envID := fixture(t, s)
	skilled := skilledAgent(t, s, "skilled")
	var skilledVersion int
	if err := s.pool.QueryRow(t.Context(), `SELECT version FROM agents WHERE id = $1`, skilled).Scan(&skilledVersion); err != nil {
		t.Fatal(err)
	}

	stored := func(body map[string]any) string {
		body["resources"] = []any{skillsRepo(nil)}
		id := createDeployment(t, s, body)["id"].(string)
		if _, err := s.pool.Exec(t.Context(),
			`UPDATE deployments SET agent_id = $2, agent_version = $3 WHERE id = $1`, id, skilled, skilledVersion); err != nil {
			t.Fatal(err)
		}
		return id
	}

	manual := stored(deploymentBody(plain, envID))
	run := runDeployment(t, s, manual)
	re, _ := run["error"].(map[string]any)
	if run["session_id"] != nil || re["type"] != "session_creation_rejected_error" || re["message"] != skillsRootRefusal("/workspace/skills") {
		t.Fatalf("manual run = session %v, error %v; want session_creation_rejected_error %q",
			run["session_id"], re, skillsRootRefusal("/workspace/skills"))
	}

	scheduled := stored(scheduledBody(plain, envID, "0 9 * * *", "UTC"))
	setResumedAt(t, s, scheduled, time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC))
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 0, 10, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	runs := scheduledRuns(t, s, scheduled)
	if len(runs) != 1 || runs[0].errType == nil || *runs[0].errType != "session_creation_rejected_error" {
		t.Fatalf("scheduled runs = %+v, want one session_creation_rejected_error", runs)
	}
	if code, d := s.do(http.MethodGet, "/v1/deployments/"+scheduled, nil); code != http.StatusOK || d["status"] != "active" {
		t.Errorf("after the scheduled fire: %d, status %v; session_creation_rejected_error pauses nothing", code, d["status"])
	}

	// Such a row is left alone by an update that touches neither its agent nor
	// its resources, and re-pinning it to a skill-free agent repairs it.
	kept := stored(deploymentBody(plain, envID))
	for _, update := range []map[string]any{{"name": "renamed"}, {"environment_id": envID}, {"agent": plain}} {
		if status, res := s.do(http.MethodPost, "/v1/deployments/"+kept, update); status != http.StatusOK {
			t.Errorf("update %v: status %d (%v), want 200", update, status, res)
		}
	}
}
