package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
)

// selfHostedResourcesRefusal is the reference's sentence for a self-hosted
// session that carries a file or github_repository resource
// (platform.claude.com managed-agents/self-hosted-sandboxes § "Session
// resources"). This platform refuses only the repository arm with it: its
// worker materializes files, and clones nothing.
func selfHostedResourcesRefusal(envID string) string {
	return "Environment " + envID + " is a self-hosted environment. `resources` are not supported with self-hosted environments."
}

func selfHostedRepo() map[string]any {
	return map[string]any{"type": "github_repository", "url": "https://github.com/acme/orders",
		"authorization_token": "ghp_byoc"}
}

// A self_hosted session that mounts a repository is refused before anything
// is stored, in the reference's words, and counted as an invalid resource
// mutation; a file stays accepted, because the BYOC worker does materialize
// one.
func TestSelfHostedSessionRefusesARepository(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	agentID, _ := fixture(t, s)
	envID := selfHostedEnv(t, s, "byoc")

	status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agentID, "environment_id": envID, "resources": []any{repoBody("ghp_byoc", nil)}})
	wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error", selfHostedResourcesRefusal(envID))
	if got := resourceMutationCount(t, collect(), "invalid"); got != 1 {
		t.Errorf("session.resources{invalid} = %d, want 1 for the refused create", got)
	}

	var sessions, credentials int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM sessions WHERE environment_id = $1`, envID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM session_resource_credentials`).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || credentials != 0 {
		t.Errorf("stored %d sessions and %d sealed tokens despite the refusal, want none", sessions, credentials)
	}

	fileID := s.uploadFile(t, "notes.txt", nil, "hello")["id"].(string)
	sess := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"resources": []any{map[string]any{"type": "file", "file_id": fileID}}})
	if got := resourcesOf(t, sess); len(got) != 1 || got[0]["type"] != "file" {
		t.Errorf("self_hosted file resources = %v, want the one file", got)
	}
}

// The refusal comes before the token is sealed, so a deployment with no
// secrets cipher answers the reference's 400 rather than the 500 a cloud
// repository gets there — on session create and on both deployment routes.
func TestSelfHostedRepositoryRefusalPrecedesTheCipher(t *testing.T) {
	s := newTestServerWithCipher(t, nil)
	agentID, _ := fixture(t, s)
	envID := selfHostedEnv(t, s, "byoc")

	status, body := s.do(http.MethodPost, "/v1/sessions", map[string]any{
		"agent": agentID, "environment_id": envID, "resources": []any{repoBody("ghp_byoc", nil)}})
	wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error", selfHostedResourcesRefusal(envID))

	create := deploymentBody(agentID, envID)
	create["resources"] = []any{selfHostedRepo()}
	status, body = s.do(http.MethodPost, "/v1/deployments", create)
	wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error", selfHostedResourcesRefusal(envID))

	deplID := createDeployment(t, s, deploymentBody(agentID, envID))["id"].(string)
	status, body = s.do(http.MethodPost, "/v1/deployments/"+deplID, map[string]any{"resources": []any{selfHostedRepo()}})
	wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error", selfHostedResourcesRefusal(envID))
}

// A deployment is refused the same way on create and on update: whether the
// update brings the repository or moves a repository-bearing deployment onto a
// self_hosted environment. A file stays accepted.
func TestSelfHostedDeploymentRefusesARepository(t *testing.T) {
	s := newTestServer(t)
	agentID, cloudEnv := fixture(t, s)
	envID := selfHostedEnv(t, s, "byoc")

	body := deploymentBody(agentID, envID)
	body["resources"] = []any{selfHostedRepo()}
	status, res := s.do(http.MethodPost, "/v1/deployments", body)
	wantErrMsg(t, status, res, http.StatusBadRequest, "invalid_request_error", selfHostedResourcesRefusal(envID))
	var count int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM deployments`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("%d deployments stored despite the refusal", count)
	}

	fileID := s.uploadFile(t, "input.txt", nil, "hello")["id"].(string)
	withFile := deploymentBody(agentID, envID)
	withFile["resources"] = []any{map[string]any{"type": "file", "file_id": fileID}}
	byoc := createDeployment(t, s, withFile)["id"].(string)

	status, res = s.do(http.MethodPost, "/v1/deployments/"+byoc, map[string]any{"resources": []any{selfHostedRepo()}})
	wantErrMsg(t, status, res, http.StatusBadRequest, "invalid_request_error", selfHostedResourcesRefusal(envID))

	cloudBody := deploymentBody(agentID, cloudEnv)
	cloudBody["resources"] = []any{selfHostedRepo()}
	cloud := createDeployment(t, s, cloudBody)["id"].(string)
	status, res = s.do(http.MethodPost, "/v1/deployments/"+cloud, map[string]any{"environment_id": envID})
	wantErrMsg(t, status, res, http.StatusBadRequest, "invalid_request_error", selfHostedResourcesRefusal(envID))
	var stored string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT environment_id FROM deployments WHERE id = $1`, cloud).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != cloudEnv {
		t.Errorf("environment_id = %s after the refused update, want %s", stored, cloudEnv)
	}
}

// legacySelfHostedRepoDeployment stores what the create and update routes now
// refuse — a self_hosted deployment mounting a repository — the way a row
// written before the refusal holds it: created against a cloud environment,
// then moved by SQL, past the route.
func legacySelfHostedRepoDeployment(t *testing.T, s *tserver, body map[string]any) (deplID, envID string) {
	t.Helper()
	body["resources"] = []any{selfHostedRepo()}
	deplID = createDeployment(t, s, body)["id"].(string)
	envID = selfHostedEnv(t, s, "byoc-"+deplID)
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE deployments SET environment_id = $2 WHERE id = $1`, deplID, envID); err != nil {
		t.Fatal(err)
	}
	return deplID, envID
}

// A row stored before the refusal is left alone by an update that touches
// neither its environment nor its resources, and either repair — dropping the
// repository, or moving the deployment to a cloud environment — is accepted.
func TestSelfHostedRepositoryDeploymentStoredEarlierCanBeRepaired(t *testing.T) {
	s := newTestServer(t)
	agentID, cloudEnv := fixture(t, s)

	renamed, _ := legacySelfHostedRepoDeployment(t, s, deploymentBody(agentID, cloudEnv))
	if status, res := s.do(http.MethodPost, "/v1/deployments/"+renamed, map[string]any{"name": "renamed"}); status != http.StatusOK {
		t.Errorf("rename: status %d (%v), want 200", status, res)
	}

	cleared, _ := legacySelfHostedRepoDeployment(t, s, deploymentBody(agentID, cloudEnv))
	if status, res := s.do(http.MethodPost, "/v1/deployments/"+cleared, map[string]any{"resources": []any{}}); status != http.StatusOK {
		t.Errorf("clearing the repository: status %d (%v), want 200", status, res)
	}

	moved, _ := legacySelfHostedRepoDeployment(t, s, deploymentBody(agentID, cloudEnv))
	if status, res := s.do(http.MethodPost, "/v1/deployments/"+moved, map[string]any{"environment_id": cloudEnv}); status != http.StatusOK {
		t.Errorf("moving to a cloud environment: status %d (%v), want 200", status, res)
	}
}

// Such a deployment fails its fire as self_hosted_resources_unsupported_error,
// the reference's run type for it, rather than creating a session whose
// repository nothing will ever mount: a manual run records the type and leaves
// the deployment active, and a scheduled fire records it and pauses the
// deployment.
func TestSelfHostedRepositoryDeploymentFailsItsFire(t *testing.T) {
	s := newTestServer(t)
	agentID, cloudEnv := fixture(t, s)

	manual, envID := legacySelfHostedRepoDeployment(t, s, deploymentBody(agentID, cloudEnv))
	run := runDeployment(t, s, manual)
	re, _ := run["error"].(map[string]any)
	if run["session_id"] != nil || re["type"] != "self_hosted_resources_unsupported_error" {
		t.Fatalf("manual run = session %v, error %v; want self_hosted_resources_unsupported_error", run["session_id"], re)
	}
	if re["message"] != selfHostedResourcesRefusal(envID) {
		t.Errorf("error.message = %v, want %q", re["message"], selfHostedResourcesRefusal(envID))
	}
	if code, d := s.do(http.MethodGet, "/v1/deployments/"+manual, nil); code != http.StatusOK || d["status"] != "active" {
		t.Errorf("after the manual run: %d, status %v; a failed manual run must not pause", code, d["status"])
	}

	scheduled, _ := legacySelfHostedRepoDeployment(t, s, scheduledBody(agentID, cloudEnv, "0 9 * * *", "UTC"))
	setResumedAt(t, s, scheduled, time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC))
	if err := api.SchedulerTick(t.Context(), s.pool, time.Date(2026, 3, 12, 9, 0, 10, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	runs := scheduledRuns(t, s, scheduled)
	if len(runs) != 1 || runs[0].errType == nil || *runs[0].errType != "self_hosted_resources_unsupported_error" {
		t.Fatalf("scheduled runs = %+v, want one self_hosted_resources_unsupported_error", runs)
	}
	code, d := s.do(http.MethodGet, "/v1/deployments/"+scheduled, nil)
	if code != http.StatusOK {
		t.Fatalf("get: %d %v", code, d)
	}
	reason, _ := d["paused_reason"].(map[string]any)
	inner, _ := reason["error"].(map[string]any)
	if d["status"] != "paused" || inner["type"] != "self_hosted_resources_unsupported_error" {
		t.Errorf("status = %v, paused_reason = %v; want paused on self_hosted_resources_unsupported_error", d["status"], reason)
	}

	var sessions int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM sessions WHERE deployment_id IS NOT NULL`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Errorf("%d sessions survived the refused fires, want 0", sessions)
	}
}
