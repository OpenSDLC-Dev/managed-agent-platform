package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

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

// A repository may share the skills directory with the session's skills,
// whoever carries them — the agent, an override or a roster member — as the
// reference was recorded doing: the agent carrying the pdf skill, a repository
// at /workspace/skills was accepted, and the two shared the directory
// (2026-10-13 setup[17–21], #922). Before plan 64 this was refused (#584).
// /workspace itself names the default.
func TestSkillsRootRepositorySharesTheDirectory(t *testing.T) {
	s := newTestServer(t)
	plain, envID := fixture(t, s)
	skilled := skilledAgent(t, s, "skilled")
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
		{"skill-free, default mount", plain, skillsRepo(nil), "/workspace/skills"},
		{"own skill, default mount", skilled, skillsRepo(nil), "/workspace/skills"},
		{"own skill, a skill's directory", skilled,
			skillsRepo(map[string]any{"mount_path": "/workspace/skills/pdf"}), "/workspace/skills/pdf"},
		{"override skill", override, skillsRepo(nil), "/workspace/skills"},
		{"roster member's skill", coordinator, skillsRepo(nil), "/workspace/skills"},
		{"the workdir spelled", skilled, skillsRepo(map[string]any{"mount_path": "/workspace"}), "/workspace/skills"},
	} {
		sess := createSession(t, s, map[string]any{"agent": tc.agent, "environment_id": envID,
			"resources": []any{tc.repo}})
		if got := resourcesOf(t, sess)[0]["mount_path"]; got != tc.mount {
			t.Errorf("%s: mount_path = %v, want %s", tc.name, got, tc.mount)
		}
	}
}

// A deployment takes the same pair on create and update, and fires it, by
// hand and on its schedule; before plan 64 each was refused (#584).
func TestSkillsRootRepositoryDeploymentSharesTheDirectory(t *testing.T) {
	s := newTestServer(t)
	plain, envID := fixture(t, s)
	skilled := skilledAgent(t, s, "skilled")

	body := deploymentBody(skilled, envID)
	body["resources"] = []any{skillsRepo(nil)}
	createDeployment(t, s, body)

	withRepo := deploymentBody(plain, envID)
	withRepo["resources"] = []any{skillsRepo(nil)}
	repoed := createDeployment(t, s, withRepo)["id"].(string)
	if status, res := s.do(http.MethodPost, "/v1/deployments/"+repoed, map[string]any{"agent": skilled}); status != http.StatusOK {
		t.Fatalf("re-pin to a skilled agent: status %d (%v), want 200", status, res)
	}
	run := runDeployment(t, s, repoed)
	if run["session_id"] == nil {
		t.Errorf("the fire created no session: %v", run["error"])
	}

	scheduled := scheduledBody(skilled, envID, "0 9 * * *", "UTC")
	scheduled["resources"] = []any{skillsRepo(nil)}
	id := createDeployment(t, s, scheduled)["id"].(string)
	setResumedAt(t, s, id, time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC))
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 0, 10, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runs := scheduledRuns(t, s, id); len(runs) != 1 || runs[0].errType != nil {
		t.Errorf("scheduled runs = %+v, want one without an error", runs)
	}
}
